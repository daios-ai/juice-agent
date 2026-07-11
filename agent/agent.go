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
	"fmt"
	"sort"
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

// ActionStats is an action's kernel track record (GET /v1/stats/{id}). Known is
// false when no stats row was fetched (an untested action), so callers can
// distinguish "zero uses" from "unknown".
type ActionStats struct {
	Uses            int     `json:"uses"`
	Successes       int     `json:"successes"`
	Failures        int     `json:"failures"`
	LatencyEstimate float64 `json:"latency_estimate"`
	RatingEstimate  float64 `json:"rating_estimate"`
	RatingCount     int     `json:"rating_count"`
	Known           bool    `json:"-"`
}

// Candidate is a callable action in the working set: its @owner/name ref, the
// contract fields lookup returns, its price (the subtree bound), and its stats.
type Candidate struct {
	Ref         string         `json:"ref"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Price       int            `json:"price"`
	Stats       ActionStats    `json:"stats"`
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

// RunResult is the outcome of dispatching a tool call through Runner.
type RunResult struct {
	Result map[string]any
	TxID   string
}

// Decider performs one model round-trip: given the conversation, the catalog
// candidates (with descriptions/schemas/stats to render), and the harness tools,
// it returns the next selection. Implemented by the juice client over
// @sys/llm/chat (it builds the tool list itself and extracts a JSON selection).
type Decider interface {
	Decide(ctx context.Context, messages []Message, catalog []Candidate, tools []Tool) (*Selection, error)
}

// Catalog discovers actions and reads their metadata. Lookup ranks active
// actions for a query (@sys/lookup); Meta fetches an action's price and stats.
type Catalog interface {
	Lookup(ctx context.Context, query string, limit int) ([]Candidate, error)
	Meta(ctx context.Context, ref string) (price int, stats ActionStats, err error)
}

// Runner dispatches a tool call: POST /v1/run on the action. A kernel-level
// failure (the action ran and failed, or was rejected) is returned as an error.
type Runner interface {
	Run(ctx context.Context, action string, args map[string]any) (RunResult, error)
}

// Transcript is the durable, possibly-compacted working memory. Append persists
// one record; Messages returns the current *frame's* model view (which the
// implementation may have compacted). Child opens a nested bracket frame — a
// fresh model view seeded with `seed`, whose records are teed to the same durable
// store, tagged with a new bracket id. All returns every record across frames
// (for rating and memory). The core keeps no other state.
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

// Task is one unit of work: a transcript seed, an originating channel, and a
// credit budget bounding total spend.
type Task struct {
	ID       string
	Channel  string
	Messages []Message
	Budget   int
}

// Status is a task's terminal disposition.
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
// catalog's @owner/name actions. Unlike an action it is not registered anywhere
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
	LookupAction   string      // the discovery action ref (e.g. "@sys/lookup")
	Tools          []Tool      // harness tools rendered to the model each turn
	Always         []Candidate // catalog candidates always offered (e.g. @sys/lookup)
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

// Deps is the loop's wiring: the four interfaces plus optional steering and log
// hooks. Steering, when set, is polled before every model call.
type Deps struct {
	Decider    Decider
	Catalog    Catalog
	Runner     Runner
	Transcript Transcript
	Steering   func() []Message
	Log        func(event string, fields map[string]any)
}

// counters are the per-task budgets shared by every frame (root and brackets) so
// a subtask tree totals at most MaxTurns turns and task.Budget credits — the
// bound is the shared counter, not a per-frame cap.
type counters struct {
	turns int
	spent int
}

// Run executes one task to completion via the root loop frame (§4). Subtasks
// (§4b) recurse into brackets that share the same counters and roll up to a
// single result in the parent chat. The stop rule keys off the *selection* (the
// reply tool), never a provider stop reason; the primary runaway rail is
// economic (priced calls prepaid against task.Budget), the secondary the turn cap.
func Run(ctx context.Context, task Task, cfg Config, deps Deps) (Outcome, error) {
	logf := deps.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	for _, m := range task.Messages {
		if err := deps.Transcript.Append(ctx, m); err != nil {
			return Outcome{Status: StatusFailed}, err
		}
	}
	cnt := &counters{}
	res, status, reason, err := runFrame(ctx, cfg, deps, task.Budget, cnt, 0, logf)
	out := Outcome{Status: status, Reason: reason, Spent: cnt.spent, Turns: cnt.turns}
	if status == StatusAnswered {
		out.Answer = res
	}
	logf("task_end", map[string]any{"status": out.Status, "reason": out.Reason, "turns": cnt.turns, "spent": cnt.spent})
	return out, err
}

// runFrame runs one loop frame — the root task (depth 0) or a subtask bracket
// (depth > 0). It returns the frame's result (the reply args on success, or a
// rollup error map), a status, a reason, and a Go error (only for infrastructure
// failures; model/budget/turn failures roll up as results). deps.Transcript is
// this frame's model view; its records are teed to the shared durable store.
func runFrame(ctx context.Context, cfg Config, deps Deps, budget int, cnt *counters, depth int, logf func(string, map[string]any)) (map[string]any, Status, string, error) {
	ws := newWorkingSet(cfg.CandidateLimit)
	for _, c := range cfg.Always {
		ws.put(c)
	}
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
		cnt.turns++

		if depth == 0 && deps.Steering != nil {
			for _, m := range deps.Steering() {
				if err := deps.Transcript.Append(ctx, m); err != nil {
					return nil, StatusFailed, "", err
				}
			}
		}

		sel, err := deps.Decider.Decide(ctx, deps.Transcript.Messages(), ws.candidates(cfg), cfg.Tools)
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
		// already cost a turn (counted at the loop top), so MaxTurns bounds the
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
			res, _, _, ferr := runFrame(ctx, childCfg, childDeps, budget, cnt, depth+1, logf)
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

		// catalog action → dispatch (budget-gated).
		default:
			price := ws.priceOf(sel.Action)
			if cnt.spent+price > budget {
				if err := deps.Transcript.Append(ctx, Message{Role: RoleUser, Kind: "budget",
					Content: fmt.Sprintf("Budget exceeded: %q costs %d but only %d credits remain. "+
						"Choose a cheaper action or finish with the %q tool.", sel.Action, price, budget-cnt.spent, cfg.ReplyTool)}); err != nil {
					return nil, StatusFailed, "", err
				}
				logf("budget_refusal", map[string]any{"action": sel.Action, "price": price, "remaining": budget - cnt.spent})
				continue
			}
			rr, rerr := deps.Runner.Run(ctx, sel.Action, sel.Args)
			cnt.spent += price
			result := rr.Result
			if sel.Action == cfg.LookupAction && rerr == nil {
				mergeCandidates(ctx, deps, ws, parseLookup(rr.Result))
				result = trimLookup(rr.Result)
			}
			if err := deps.Transcript.Append(ctx, Message{
				Role: RoleTool, Action: sel.Action, Result: result, TxID: rr.TxID,
				IsError: rerr != nil, Content: errText(rerr),
			}); err != nil {
				return nil, StatusFailed, "", err
			}
			progressed(sel.Action)
			logf("dispatch", map[string]any{"action": sel.Action, "price": price, "tx_id": rr.TxID, "error": rerr != nil})
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

// mergeCandidates enriches each new candidate with its price and stats (via
// Catalog.Meta) and folds it into the working set. Refs already present are
// refreshed, not duplicated.
func mergeCandidates(ctx context.Context, deps Deps, ws *workingSet, cs []Candidate) {
	for _, c := range cs {
		if c.Ref == "" {
			continue
		}
		if price, stats, err := deps.Catalog.Meta(ctx, c.Ref); err == nil {
			c.Price, c.Stats = price, stats
		}
		ws.put(c)
	}
}

// parseLookup extracts candidates from a @sys/lookup result object
// ({"results":[{action, description, input_schema, ...}]}).
func parseLookup(res map[string]any) []Candidate {
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

// workingSet is the bounded, stats-ranked set of candidate actions offered to
// the model each turn. It caps at limit, evicting the lowest-value candidates.
type workingSet struct {
	limit int
	by    map[string]Candidate
}

func newWorkingSet(limit int) *workingSet {
	if limit <= 0 {
		limit = 24
	}
	return &workingSet{limit: limit, by: map[string]Candidate{}}
}

func (w *workingSet) put(c Candidate) {
	w.by[c.Ref] = c
	if len(w.by) <= w.limit {
		return
	}
	// Evict the single lowest-value candidate (deterministic tie-break by ref).
	worst, worstVal := "", 0.0
	first := true
	for ref, cand := range w.by {
		v := value(cand)
		if first || v < worstVal || (v == worstVal && ref > worst) {
			worst, worstVal, first = ref, v, false
		}
	}
	delete(w.by, worst)
}

func (w *workingSet) priceOf(ref string) int {
	if c, ok := w.by[ref]; ok {
		return c.Price
	}
	return 0 // always-refs / intrinsics are zero-price natives
}

// candidates returns the working-set actions to offer this turn, value-sorted
// (best first) so the client renders the most promising tools at the top, with
// the always-offered candidates guaranteed present regardless of eviction.
func (w *workingSet) candidates(cfg Config) []Candidate {
	merged := map[string]Candidate{}
	for ref, c := range w.by {
		merged[ref] = c
	}
	for _, c := range cfg.Always {
		if _, ok := merged[c.Ref]; !ok {
			merged[c.Ref] = c
		}
	}
	out := make([]Candidate, 0, len(merged))
	for _, c := range merged {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if vi, vj := value(out[i]), value(out[j]); vi != vj {
			return vi > vj
		}
		return out[i].Ref < out[j].Ref // deterministic tie-break
	})
	return out
}

// trimLookup rewrites a @sys/lookup result to carry only ref + description +
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

// value scores a candidate for eviction and stats-line ranking: a
// Laplace-smoothed success rate plus a rating prior. Unknown stats sit at a
// neutral ~1.0, a proven-good action scores higher, a persistent failer lower.
func value(c Candidate) float64 {
	s := c.Stats
	succ := float64(s.Successes+1) / float64(s.Uses+2)
	rating := 0.5
	if s.RatingCount > 0 {
		rating = s.RatingEstimate
	}
	return succ + rating
}
