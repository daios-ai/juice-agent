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

// Role labels a transcript message. The values map onto @sys/llm/decide's
// message roles (system, user, assistant, tool).
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

// Selection is one @sys/llm/decide result: the chosen action, args already
// validated against that action's input_schema by the kernel, and an optional
// natural-language message.
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
// one record; Messages returns the current message list to send to the model
// (which the implementation may have compacted). The core keeps no other state.
type Transcript interface {
	Append(ctx context.Context, m Message) error
	Messages() []Message
}

// Intrinsic is an agent-native capability the harness intercepts instead of
// dispatching through Runner. Its action is nonetheless registered in the kernel
// (zero price) so decide can select it and validate its args. Handle returns the
// tool result, whether the run should terminate, and an error.
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
	LookupAction   string      // the discovery action ref (e.g. "@sys/lookup")
	Tools          []Tool      // harness tools rendered to the model each turn
	Always         []Candidate // catalog candidates always offered (e.g. @sys/lookup)
	MaxTurns       int
	CandidateLimit int
	LookupLimit    int
	// MaxDecideErrors bounds consecutive invalid model selections before the run
	// fails; each is fed back for self-correction. Default 3 when zero.
	MaxDecideErrors int
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

// Run executes one task to completion: the hand-owned pi-style loop with the
// Juice catalog as its tool set. It seeds a working set of candidate actions
// (via lookup), then repeatedly asks the Decider for the next tool, dispatches
// it (or intercepts an intrinsic), and feeds the result back — until the model
// selects the terminal reply action, the budget or turn cap is hit, or a
// decide error ends the run.
//
// The stop rule keys off the *selection* (reply action chosen), never a
// provider stop reason. The primary runaway rail is economic: every dispatched
// call costs its advertised price, prepaid against the task budget. The turn cap
// is the secondary rail for zero-price actions that escape the money rail.
func Run(ctx context.Context, task Task, cfg Config, deps Deps) (Outcome, error) {
	logf := deps.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	out := Outcome{Status: StatusFailed}

	for _, m := range task.Messages {
		if err := deps.Transcript.Append(ctx, m); err != nil {
			return out, err
		}
	}

	ws := newWorkingSet(cfg.CandidateLimit)
	for _, c := range cfg.Always {
		ws.put(c)
	}
	if seed, err := deps.Catalog.Lookup(ctx, prompt(task), cfg.LookupLimit); err == nil {
		mergeCandidates(ctx, deps, ws, seed)
	}

	maxDecideErrors := cfg.MaxDecideErrors
	if maxDecideErrors <= 0 {
		maxDecideErrors = 3
	}
	decideErrors := 0

	spent := 0
	for turn := 1; ; turn++ {
		if cfg.MaxTurns > 0 && turn > cfg.MaxTurns {
			out.Reason = "turn-cap"
			out.Spent, out.Turns = spent, turn-1
			logf("task_end", map[string]any{"status": out.Status, "reason": out.Reason, "spent": spent})
			return out, nil
		}

		if deps.Steering != nil {
			for _, m := range deps.Steering() {
				if err := deps.Transcript.Append(ctx, m); err != nil {
					return out, err
				}
			}
		}

		catalog := ws.candidates(cfg)
		sel, err := deps.Decider.Decide(ctx, deps.Transcript.Messages(), catalog, cfg.Tools)
		if err != nil {
			// A real model occasionally emits an unusable selection (malformed
			// args, no valid choice). Rather than kill the task, feed the error
			// back and let the model self-correct on the next turn — bounded by a
			// consecutive-error cap so a persistently broken model still ends.
			decideErrors++
			logf("decide_error", map[string]any{"turn": turn, "consecutive": decideErrors, "error": err.Error()})
			if decideErrors > maxDecideErrors {
				out.Reason = "decide-error: " + err.Error()
				out.Spent, out.Turns = spent, turn
				logf("task_end", map[string]any{"status": out.Status, "reason": out.Reason})
				return out, err
			}
			if aerr := deps.Transcript.Append(ctx, Message{Role: RoleSystem, Kind: "decide-error",
				Content: "Your previous tool selection was invalid: " + err.Error() +
					". Choose one of the available actions and provide arguments that exactly match its input schema."}); aerr != nil {
				return out, aerr
			}
			continue
		}
		decideErrors = 0
		if err := deps.Transcript.Append(ctx, Message{
			Role: RoleAssistant, Action: sel.Action, Args: sel.Args, Content: sel.Message,
		}); err != nil {
			return out, err
		}
		logf("turn", map[string]any{"turn": turn, "action": sel.Action})

		// Harness tool: intercepted, not dispatched; may terminate the run. Its
		// args are checked against the tool's required keys in-harness (catalog
		// actions are validated kernel-side at dispatch instead).
		if intr, ok := cfg.Intrinsics[sel.Action]; ok {
			if miss := missingRequired(toolSchema(cfg.Tools, sel.Action), sel.Args); miss != "" {
				decideErrors++
				if decideErrors > maxDecideErrors {
					out.Reason = "decide-error: missing required arg " + miss
					out.Spent, out.Turns = spent, turn
					return out, nil
				}
				if aerr := deps.Transcript.Append(ctx, Message{Role: RoleSystem, Kind: "decide-error",
					Content: fmt.Sprintf("Your %q selection is missing the required argument %q. Re-select with it.", sel.Action, miss)}); aerr != nil {
					return out, aerr
				}
				continue
			}
			res, terminal, ierr := intr.Handle(ctx, sel.Args)
			if err := deps.Transcript.Append(ctx, Message{
				Role: RoleTool, Action: sel.Action, Result: res, IsError: ierr != nil, Content: errText(ierr),
			}); err != nil {
				return out, err
			}
			if terminal {
				out.Status, out.Answer = StatusAnswered, res
				out.Spent, out.Turns = spent, turn
				logf("task_end", map[string]any{"status": out.Status, "turns": turn, "spent": spent})
				return out, nil
			}
			continue
		}

		// Budget rail: tell the model, do not kill it.
		price := ws.priceOf(sel.Action)
		if spent+price > task.Budget {
			if err := deps.Transcript.Append(ctx, Message{
				Role: RoleUser, Kind: "budget",
				Content: fmt.Sprintf("Budget exceeded: %q costs %d but only %d credits remain. "+
					"Choose a cheaper action or finish with the %q tool.", sel.Action, price, task.Budget-spent, cfg.ReplyTool),
			}); err != nil {
				return out, err
			}
			logf("budget_refusal", map[string]any{"action": sel.Action, "price": price, "remaining": task.Budget - spent})
			continue
		}

		rr, rerr := deps.Runner.Run(ctx, sel.Action, sel.Args)
		spent += price
		// Discovery is in-band: a lookup result expands the working set. The full
		// result (with schemas) feeds the working set, but only a trimmed form
		// (ref + description + score) is persisted to the transcript — the schemas
		// live in the working set and are rendered per turn, so keeping them in the
		// chat too is pure noise.
		result := rr.Result
		if sel.Action == cfg.LookupAction && rerr == nil {
			mergeCandidates(ctx, deps, ws, parseLookup(rr.Result))
			result = trimLookup(rr.Result)
		}
		if err := deps.Transcript.Append(ctx, Message{
			Role: RoleTool, Action: sel.Action, Result: result, TxID: rr.TxID,
			IsError: rerr != nil, Content: errText(rerr),
		}); err != nil {
			return out, err
		}
		logf("dispatch", map[string]any{"action": sel.Action, "price": price, "tx_id": rr.TxID, "error": rerr != nil})
	}
}

// prompt renders the task's seed conversation into a lookup query: the
// concatenated user content, which is what lookup ranks against. System messages
// (the injected persona/knowledge/memory prompt) are deliberately excluded so
// they don't drown out the actual request in the discovery query.
func prompt(t Task) string {
	var b strings.Builder
	for _, m := range t.Messages {
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
