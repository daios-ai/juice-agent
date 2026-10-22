// Package agent is the pi-style core loop of the Juice agent: a hand-owned
// LLM<->tools<->history cycle whose tool set is the Juice action catalog.
//
// It is deliberately pure. It imports neither net/http nor the juice client;
// it depends only on the small interfaces declared here (Decider, Catalog,
// Runner, Transcript), which the policy and client layers implement. The loop
// therefore runs entirely against fakes in tests, with no kernel and no LLM.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Role labels a transcript message. The values are the LLM chat roles (system,
// user, assistant, tool).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one record in a task transcript. An assistant selection carries
// Action+Args; a tool result carries Action+Result (+TxID, +IsError). Plain
// system/user messages carry Content. Kind tags synthetic records (e.g.
// "budget", "summary", "interrupted") so persistence and recovery can find them.
type Message struct {
	Role    Role           `json:"role"`
	Content string         `json:"content,omitempty"`
	Action  string         `json:"action,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	TxID    string         `json:"tx_id,omitempty"`
	IsError bool           `json:"is_error,omitempty"`
	Kind    string         `json:"kind,omitempty"`
	// Bracket tags a record with the subtask frame that produced it ("" = root).
	// Bracket turns are teed to the durable transcript but kept out of the parent
	// frame's model view (§4b).
	Bracket string `json:"bracket,omitempty"`
}

// Candidate is a callable action in the working set: its address, the contract
// fields an authoritative read returns (Catalog.Read), its price, the quote_hash
// that pins those terms on dispatch, its value effect if any, and the kernel's
// evidence about it.
type Candidate struct {
	Ref         string         `json:"ref"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Price       int            `json:"price"`
	QuoteHash   string         `json:"quote_hash,omitempty"`
	Effect      string         `json:"effect,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
}

// ErrTermsChanged is the Runner's report that a pinned quote no longer holds: the
// action's terms moved after the job read them, and the kernel refused the call
// before locking any funds.
var ErrTermsChanged = errors.New("terms changed")

// ErrBudget is the report that a call the harness makes on its own (reasoning,
// the opening lookup, compaction, rating) does not fit what the job has left, so
// it was not made.
var ErrBudget = errors.New("budget exceeded")

// Meter is a job's spending: every priced call the job makes, whether the model
// chose it or the harness made it, is checked against Budget before it runs and
// adds its charge to Spent after. A job runs on one goroutine, so it needs no
// lock. Once closed, no call fits, so Spent is final.
type Meter struct {
	Budget int
	Spent  int
	closed bool
}

// Fits reports whether a call at price may run.
func (m *Meter) Fits(price int) bool { return !m.closed && m.Spent+price <= m.Budget }

// Add counts a call's charge.
func (m *Meter) Add(charge int) { m.Spent += charge }

// Close fixes Spent: no call fits afterwards.
func (m *Meter) Close() { m.closed = true }

// Charged is what a call at price cost the agent: the charge the kernel
// reported when it settled the call, nothing when a moved quote refused it
// before any funds were locked, and otherwise its price, since a call whose
// settlement went unreported may still draw it.
func Charged(price int, rr RunResult, err error) int {
	switch {
	case rr.TxID != "":
		return rr.Charge
	case errors.Is(err, ErrTermsChanged):
		return 0
	}
	return price
}

// Selection is one model turn extracted by the Decider: the chosen action, its
// args, and an optional natural-language message. Args are validated later — a
// harness tool's required keys in-loop, a catalog action's schema kernel-side at
// dispatch.
type Selection struct {
	Action  string         `json:"action"`
	Args    map[string]any `json:"args,omitempty"`
	Message string         `json:"message,omitempty"`
}

// RunResult is the outcome of dispatching a tool call through Runner. TxID and
// Charge are the settlement the kernel reported (Charge is what left the agent);
// a failed call returns them alongside its error. An empty TxID means no
// settlement was reported.
type RunResult struct {
	Result map[string]any
	TxID   string
	Charge int
}

// Decider performs one model round-trip: given the conversation, the catalog
// candidates (with descriptions, schemas, prices and evidence to render), and the harness tools,
// it returns the next selection. Implemented over the reasoning affordance by
// juice.Decide, which builds the tool list itself and extracts a JSON selection;
// a call the job cannot pay for returns ErrBudget.
type Decider interface {
	Decide(ctx context.Context, messages []Message, catalog []Candidate, tools []Tool) (*Selection, error)
}

// Catalog discovers actions and reads their terms. Lookup ranks callable actions
// for a query (the lookup native; its rows are indicative); Read returns one
// action's authoritative terms.
type Catalog interface {
	Lookup(ctx context.Context, query string, limit int) ([]Candidate, error)
	Read(ctx context.Context, ref string) (Candidate, error)
}

// Runner dispatches a tool call: POST /v1/run on the action, pinned to quoteHash
// and named by key when non-empty. A repeat under the same key answers with the
// first run's outcome, so a retried dispatch never runs twice. A kernel-level
// failure (the action ran and failed, or was rejected) is returned as an error; a
// moved quote as ErrTermsChanged.
type Runner interface {
	Run(ctx context.Context, action string, args map[string]any, quoteHash, key string) (RunResult, error)
}

// Transcript is the durable, possibly-compacted working memory. Append persists
// one record; Messages returns the current *frame's* model view (which the
// implementation may have compacted). Child opens a nested bracket frame — a
// fresh model view seeded with `seed`, whose records are teed to the same durable
// store, tagged with a new bracket id. All returns every record this job wrote,
// across frames (for rating); history a persona carried in from earlier jobs is
// never part of it. The core keeps no other state.
type Transcript interface {
	Append(ctx context.Context, m Message) error
	Messages() []Message
	Child(ctx context.Context, seed []Message) (Transcript, error)
	All() []Message
}

// Intrinsic is an agent-native capability (e.g. publish) the harness intercepts
// by tool name instead of dispatching through Runner. It is not registered on the
// platform — it is a plain-named harness tool, not a catalog action. Handle
// returns the tool result, whether the run should terminate, and an error.
type Intrinsic struct {
	Handle func(ctx context.Context, args map[string]any) (result map[string]any, terminal bool, err error)
}

// Job is one unit of the agent's work: a transcript seed, the persona whose
// history it continues, and a credit budget bounding total spend. (A kernel
// task, D6, is what intake turns into a Job; the two are distinct.) The loop
// never reads Persona; it names the transcript the executor opens.
type Job struct {
	ID       string
	Persona  string
	Messages []Message
	Budget   int
}

// Status is a job's terminal disposition.
type Status string

const (
	StatusAnswered    Status = "answered"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// Outcome is what a completed loop reports back through its channel.
type Outcome struct {
	Status Status         `json:"status"`
	Answer map[string]any `json:"answer,omitempty"`
	Reason string         `json:"reason,omitempty"`
	Spent  int            `json:"spent"`
	Turns  int            `json:"turns"`
}

// Tool is a plain-named harness capability offered to the model alongside the
// catalog's actions (addressed owner@kernel/name). Unlike an action it is not registered anywhere
// on the platform — the harness intercepts its selection (see Intrinsic). Names
// never contain "/", so they are unambiguous against action refs.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Config is the loop's static policy: the reply tool name, the lookup action,
// the harness tools + their intrinsic handlers, the always-offered catalog
// candidates, the caps, and the working-set bound.
type Config struct {
	ReplyTool      string      // the terminal tool's name (e.g. "reply")
	SubtaskTool    string      // the bracket-opening tool's name (e.g. "subtask"); "" disables brackets
	ActionLookup   string      // the action-lookup affordance's address (e.g. "sys@work/lookup")
	Tools          []Tool      // harness tools rendered to the model each turn
	Always         []Candidate // catalog candidates always offered (e.g. the action lookup); never evicted
	MaxTurns       int
	CandidateLimit int
	LookupLimit    int
	// MaxDecideErrors bounds consecutive invalid model selections before the run
	// fails; each is fed back for self-correction. Default 3 when zero.
	MaxDecideErrors int
	// MaxRepeats bounds how many times the model may confirm-and-repeat an
	// identical (action, args) selection after being challenged, before the frame
	// hard-stops (§4c). Default 3 when zero; negative disables the repeat guard.
	MaxRepeats int
	// Intrinsics maps a harness tool name to its handler. reply is terminal;
	// publish/subtask are non-terminal.
	Intrinsics map[string]Intrinsic
}

// Deps is the loop's wiring: the four interfaces, the job's meter, and optional
// steering and log hooks. Steering, when set, is polled before every model call.
// Meter is shared with whatever else spends for the job (the Decider's reasoning,
// compaction, rating); when nil, Run meters the job against job.Budget alone.
type Deps struct {
	Decider    Decider
	Catalog    Catalog
	Runner     Runner
	Transcript Transcript
	Meter      *Meter
	Steering   func() []Message
	Log        func(event string, fields map[string]any)
}

// counters are the per-job turns shared by every frame (root and brackets), so a
// subtask tree totals at most MaxTurns turns, as the shared meter bounds its spend
// to job.Budget: the bound is the shared count, not a per-frame cap.
type counters struct {
	turns int
}

// Run executes one job to completion via the root loop frame (§4). Subtasks
// (§4b) recurse into brackets that share the same counters and roll up to a
// single result in the parent chat. The stop rule keys off the *selection* (the
// reply tool), never a provider stop reason; the primary runaway rail is
// economic (priced calls prepaid against job.Budget), the secondary the turn cap.
func Run(ctx context.Context, job Job, cfg Config, deps Deps) (Outcome, error) {
	logf := deps.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	for _, m := range job.Messages {
		if err := deps.Transcript.Append(ctx, m); err != nil {
			return Outcome{Status: StatusFailed}, err
		}
	}
	if deps.Meter == nil {
		deps.Meter = &Meter{Budget: job.Budget}
	}
	cnt := &counters{}
	res, status, reason, err := runFrame(ctx, cfg, deps, cnt, 0, logf)
	out := Outcome{Status: status, Reason: reason, Spent: deps.Meter.Spent, Turns: cnt.turns}
	if status == StatusAnswered {
		out.Answer = res
	}
	return out, err
}

// runFrame runs one loop frame — the root task (depth 0) or a subtask bracket
// (depth > 0). It returns the frame's result (the reply args on success, or a
// rollup error map), a status, a reason, and a Go error (only for infrastructure
// failures; model/budget/turn failures roll up as results). deps.Transcript is
// this frame's model view; its records are teed to the shared durable store.
func runFrame(ctx context.Context, cfg Config, deps Deps, cnt *counters, depth int, logf func(string, map[string]any)) (map[string]any, Status, string, error) {
	ws := newWorkingSet(cfg.CandidateLimit)
	if seed, err := deps.Catalog.Lookup(ctx, promptOf(deps.Transcript.Messages()), cfg.LookupLimit); err == nil {
		mergeCandidates(ctx, deps, ws, seed)
	}

	maxDecideErrors := cfg.MaxDecideErrors
	if maxDecideErrors <= 0 {
		maxDecideErrors = 3
	}
	decideErrors := 0

	// Repeat guard (§4c): a repeated *action* (reply exempt) is neither persisted
	// nor dispatched — the loop drops it and re-queries after a single reworded
	// nudge, so the repeat never enters the chat (no pollution, no self-priming)
	// and the side-effect is bounded to the one legitimate call. lastAction is the
	// last dispatched action ("" at frame start never matches); repeatStreak counts
	// consecutive drops; nudged keeps the nudge to one line per stuck point.
	maxRepeats := cfg.MaxRepeats
	if maxRepeats == 0 {
		maxRepeats = 3
	}
	var lastAction string
	repeatStreak := 0
	nudged := false
	// progressed marks a genuinely dispatched action, resetting the repeat guard so
	// the next identical action is measured from here.
	progressed := func(action string) { lastAction, repeatStreak, nudged = action, 0, false }

	// feedback records a self-correction nudge and reports whether the cap is now
	// exceeded (so the caller ends the frame).
	feedback := func(content string) (exceeded bool, aerr error) {
		decideErrors++
		if decideErrors > maxDecideErrors {
			return true, nil
		}
		return false, deps.Transcript.Append(ctx, Message{Role: RoleSystem, Kind: "decide-error", Content: content})
	}
	// exhausted rolls the frame up: the root fails the task; a bracket returns an
	// error result its parent can act on.
	exhausted := func(reason string) (map[string]any, Status, string, error) {
		if depth == 0 {
			return nil, StatusFailed, reason, nil
		}
		return map[string]any{"error": reason}, StatusFailed, reason, nil
	}

	for {
		if cfg.MaxTurns > 0 && cnt.turns >= cfg.MaxTurns {
			return exhausted("turn-cap")
		}

		if depth == 0 && deps.Steering != nil {
			for _, m := range deps.Steering() {
				if err := deps.Transcript.Append(ctx, m); err != nil {
					return nil, StatusFailed, "", err
				}
			}
		}

		sel, err := deps.Decider.Decide(ctx, deps.Transcript.Messages(), ws.candidates(cfg), cfg.Tools)
		// A turn the job cannot pay for, or whose terms moved, ends the frame: the
		// model did nothing wrong, and telling it would itself take a turn. Neither
		// reached the model, so neither is counted; every other turn is, even one
		// lost in transport, since it may have reached the model.
		switch {
		case errors.Is(err, ErrBudget):
			return exhausted("budget")
		case errors.Is(err, ErrTermsChanged):
			return exhausted("terms-changed")
		}
		cnt.turns++
		if err != nil {
			logf("decide_error", map[string]any{"turn": cnt.turns, "depth": depth, "error": err.Error()})
			over, aerr := feedback("Your previous tool selection was invalid: " + err.Error() +
				". Choose one of the available tools or actions and provide well-formed arguments.")
			if aerr != nil {
				return nil, StatusFailed, "", aerr
			}
			if over {
				return exhausted("decide-error: " + err.Error())
			}
			continue
		}
		decideErrors = 0

		// Repeat guard (§4c): a repeated action is dropped before it is persisted or
		// dispatched; the loop re-queries after a single reworded nudge. Each retry
		// already cost a turn (counted when Decide returned), so MaxTurns bounds the
		// rephrase loop; maxRepeats consecutive drops end the frame with repeat-loop.
		if maxRepeats >= 0 && sel.Action != cfg.ReplyTool && sel.Action == lastAction {
			repeatStreak++
			if repeatStreak > maxRepeats {
				return exhausted("repeat-loop")
			}
			logf("repeat_retry", map[string]any{"turn": cnt.turns, "depth": depth, "action": sel.Action, "streak": repeatStreak})
			if !nudged {
				if err := deps.Transcript.Append(ctx, Message{Role: RoleSystem, Kind: "repeat-challenge",
					Content: fmt.Sprintf("You just ran %q, which already completed. Are you sure you want to "+
						"run it again? If the task is complete, select the %q tool with your answer; otherwise "+
						"select the action that moves things forward.", sel.Action, cfg.ReplyTool)}); err != nil {
					return nil, StatusFailed, "", err
				}
				nudged = true
			}
			continue
		}

		if err := deps.Transcript.Append(ctx, Message{
			Role: RoleAssistant, Action: sel.Action, Args: sel.Args, Content: sel.Message,
		}); err != nil {
			return nil, StatusFailed, "", err
		}
		logf("turn", map[string]any{"turn": cnt.turns, "depth": depth, "action": sel.Action})

		// Harness-tool args are checked against required keys in-harness (catalog
		// actions are validated kernel-side at dispatch).
		if sch := toolSchema(cfg.Tools, sel.Action); sch != nil {
			if miss := missingRequired(sch, sel.Args); miss != "" {
				over, aerr := feedback(fmt.Sprintf("Your %q selection is missing the required argument %q. Re-select with it.", sel.Action, miss))
				if aerr != nil {
					return nil, StatusFailed, "", aerr
				}
				if over {
					return exhausted("decide-error: missing arg " + miss)
				}
				continue
			}
		}

		switch {
		// reply → this frame is done; its args are the frame result.
		case sel.Action == cfg.ReplyTool:
			if err := deps.Transcript.Append(ctx, Message{Role: RoleTool, Action: sel.Action, Result: sel.Args}); err != nil {
				return nil, StatusFailed, "", err
			}
			return sel.Args, StatusAnswered, "", nil

		// subtask → open a bracket sharing the counters; roll its result up. The
		// child inherits the parent's context (bracketSeed) but is NOT offered the
		// subtask tool, capping nesting at one sublevel: a subtask must do the work
		// itself, not re-delegate — which, cloned from the parent, would spiral.
		case cfg.SubtaskTool != "" && sel.Action == cfg.SubtaskTool:
			child, cerr := deps.Transcript.Child(ctx, bracketSeed(deps.Transcript.Messages(), sel.Args))
			if cerr != nil {
				return nil, StatusFailed, "", cerr
			}
			childDeps := deps
			childDeps.Transcript = child
			childDeps.Steering = nil
			childCfg := cfg
			childCfg.SubtaskTool = ""
			childCfg.Tools = withoutTool(cfg.Tools, cfg.SubtaskTool)
			res, _, _, ferr := runFrame(ctx, childCfg, childDeps, cnt, depth+1, logf)
			if ferr != nil {
				return nil, StatusFailed, "", ferr
			}
			if err := deps.Transcript.Append(ctx, Message{Role: RoleTool, Action: sel.Action, Result: res}); err != nil {
				return nil, StatusFailed, "", err
			}
			progressed(sel.Action)

		// other harness tool (publish) → intrinsic handler.
		case cfg.Intrinsics[sel.Action].Handle != nil:
			res, terminal, ierr := cfg.Intrinsics[sel.Action].Handle(ctx, sel.Args)
			if err := deps.Transcript.Append(ctx, Message{
				Role: RoleTool, Action: sel.Action, Result: res, IsError: ierr != nil, Content: errText(ierr),
			}); err != nil {
				return nil, StatusFailed, "", err
			}
			if terminal {
				return res, StatusAnswered, "", nil
			}
			progressed(sel.Action)

		// catalog action → dispatch (budget-gated). Only an offered candidate is
		// dispatched: its price is what the budget gate charges, so an action the
		// model names from elsewhere would escape the gate.
		default:
			cand, ok := ws.offered(cfg, sel.Action)
			if !ok {
				over, aerr := feedback(fmt.Sprintf("%q is not one of the offered tools or actions. Select one "+
					"that is offered, or run %q to find more.", sel.Action, cfg.ActionLookup))
				if aerr != nil {
					return nil, StatusFailed, "", aerr
				}
				if over {
					return exhausted("decide-error: unoffered action " + sel.Action)
				}
				continue
			}
			price, m := cand.Price, deps.Meter
			if !m.Fits(price) {
				if err := deps.Transcript.Append(ctx, Message{Role: RoleUser, Kind: "budget",
					Content: fmt.Sprintf("Budget exceeded: %q costs %d but only %d credits remain. "+
						"Choose a cheaper action or finish with the %q tool.", sel.Action, price, m.Budget-m.Spent, cfg.ReplyTool)}); err != nil {
					return nil, StatusFailed, "", err
				}
				logf("budget_refusal", map[string]any{"action": sel.Action, "price": price, "remaining": m.Budget - m.Spent})
				continue
			}
			rr, rerr := deps.Runner.Run(ctx, sel.Action, sel.Args, cand.QuoteHash, RunKey())
			charge := Charged(price, rr, rerr)
			if errors.Is(rerr, ErrTermsChanged) {
				ws.drop(sel.Action) // re-offered only after a fresh read
			}
			m.Add(charge)
			result := rr.Result
			if sel.Action == cfg.ActionLookup && rerr == nil {
				mergeCandidates(ctx, deps, ws, ParseLookup(rr.Result))
				result = trimLookup(rr.Result)
			}
			if err := deps.Transcript.Append(ctx, Message{
				Role: RoleTool, Action: sel.Action, Result: result, TxID: rr.TxID,
				IsError: rerr != nil, Content: errText(rerr),
			}); err != nil {
				return nil, StatusFailed, "", err
			}
			progressed(sel.Action)
			logf("dispatch", map[string]any{"action": sel.Action, "price": price, "charge": charge, "tx_id": rr.TxID, "error": rerr != nil})
		}
	}
}

// bracketSeed builds a subtask frame's opening conversation: the parent's full
// turn-by-turn history (so the subtask knows the real context — what was read,
// tried, and decided, not a distilled summary), then the optional explicit
// context, then the instruction. Inheritance flows only *down*: the child's own
// turns still roll up as a single result, so the parent chat stays clean.
func bracketSeed(parent []Message, args map[string]any) []Message {
	seed := append([]Message{}, parent...)
	if c, _ := args["context"].(string); strings.TrimSpace(c) != "" {
		seed = append(seed, Message{Role: RoleUser, Content: "Context from the parent task:\n" + c})
	}
	instr, _ := args["instruction"].(string)
	seed = append(seed, Message{Role: RoleUser, Content: "Subtask: " + instr})
	return seed
}

// promptOf renders a frame's user content into a lookup query — what lookup
// ranks against. System messages (persona/knowledge/memory) are excluded so they
// don't drown out the actual request.
func promptOf(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == RoleUser && m.Content != "" {
			b.WriteString(m.Content)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mergeCandidates replaces each lookup row with the action's authoritative read
// (Catalog.Read) and folds it into the working set; refs already present are
// refreshed. A row whose read fails is dropped, and so is an action with a value
// effect: it moves the caller's own balance beyond its price, so the budget gate,
// which charges the price, could not bound it.
func mergeCandidates(ctx context.Context, deps Deps, ws *workingSet, cs []Candidate) {
	for _, c := range cs {
		if c.Ref == "" {
			continue
		}
		full, err := deps.Catalog.Read(ctx, c.Ref)
		if err != nil || full.Effect != "" {
			continue
		}
		ws.put(full)
	}
}

// ParseLookup extracts candidates from a lookup result object
// ({"results":[{action, description, input_schema, ...}]}).
func ParseLookup(res map[string]any) []Candidate {
	raw, _ := res["results"].([]any)
	out := make([]Candidate, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := m["action"].(string)
		if ref == "" {
			continue
		}
		desc, _ := m["description"].(string)
		schema, _ := m["input_schema"].(map[string]any)
		out = append(out, Candidate{Ref: ref, Description: desc, InputSchema: schema})
	}
	return out
}

// workingSet is the bounded set of candidate actions offered to the model each
// turn, most recently merged last. It caps at limit by evicting the least
// recently merged candidate; the always-offered candidates sit outside it.
type workingSet struct {
	limit int
	order []string
	by    map[string]Candidate
}

func newWorkingSet(limit int) *workingSet {
	if limit <= 0 {
		limit = 24
	}
	return &workingSet{limit: limit, by: map[string]Candidate{}}
}

// put adds or refreshes c as the most recent candidate, evicting the oldest
// when over the limit.
func (w *workingSet) put(c Candidate) {
	w.drop(c.Ref)
	w.by[c.Ref] = c
	w.order = append(w.order, c.Ref)
	if len(w.order) > w.limit {
		delete(w.by, w.order[0])
		w.order = w.order[1:]
	}
}

func (w *workingSet) drop(ref string) {
	if _, ok := w.by[ref]; !ok {
		return
	}
	delete(w.by, ref)
	for i, r := range w.order {
		if r == ref {
			w.order = append(w.order[:i], w.order[i+1:]...)
			break
		}
	}
}

// offered returns the candidate the model may dispatch under ref: a working-set
// member or an always-offered candidate.
func (w *workingSet) offered(cfg Config, ref string) (Candidate, bool) {
	if c, ok := w.by[ref]; ok {
		return c, true
	}
	return always(cfg, ref)
}

// candidates returns what the model is offered this turn: the always-offered
// candidates, then the working set from most to least recent.
func (w *workingSet) candidates(cfg Config) []Candidate {
	out := append([]Candidate{}, cfg.Always...)
	for i := len(w.order) - 1; i >= 0; i-- {
		if _, dup := always(cfg, w.order[i]); !dup {
			out = append(out, w.by[w.order[i]])
		}
	}
	return out
}

func always(cfg Config, ref string) (Candidate, bool) {
	for _, c := range cfg.Always {
		if c.Ref == ref {
			return c, true
		}
	}
	return Candidate{}, false
}

// trimLookup rewrites a lookup result to carry only ref + description +
// score per candidate, dropping the schemas that bloat the transcript.
func trimLookup(res map[string]any) map[string]any {
	raw, _ := res["results"].([]any)
	out := make([]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		slim := map[string]any{"action": m["action"], "description": m["description"]}
		if s, ok := m["score"]; ok {
			slim["score"] = s
		}
		out = append(out, slim)
	}
	return map[string]any{"results": out}
}

// withoutTool returns tools with the named tool removed (used to withhold the
// subtask tool from a subtask frame, capping nesting depth at one sublevel).
func withoutTool(tools []Tool, name string) []Tool {
	if name == "" {
		return tools
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name != name {
			out = append(out, t)
		}
	}
	return out
}

// toolSchema returns the input schema of the named harness tool, or nil.
func toolSchema(tools []Tool, name string) map[string]any {
	for _, t := range tools {
		if t.Name == name {
			return t.InputSchema
		}
	}
	return nil
}

// missingRequired returns the first required key of schema absent from args, or
// "" if all are present (or the schema declares no required keys).
func missingRequired(schema map[string]any, args map[string]any) string {
	req, _ := schema["required"].([]any)
	for _, r := range req {
		k, _ := r.(string)
		if k == "" {
			continue
		}
		if _, ok := args[k]; !ok {
			return k
		}
	}
	return ""
}

// RunKey names one call for the kernel (external_key): random, so no two calls
// share it, and fixed across that call's retries.
func RunKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
