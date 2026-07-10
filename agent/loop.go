package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

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

		refs := ws.refs(cfg)
		msgs := deps.Transcript.Messages()
		if line, ok := ws.statsMessage(); ok {
			msgs = append(append([]Message{}, msgs...), line)
		}

		sel, err := deps.Decider.Decide(ctx, msgs, refs)
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

		// Intrinsic: intercepted, not dispatched; may terminate the run.
		if intr, ok := cfg.Intrinsics[sel.Action]; ok {
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
					"Choose a cheaper action or finish with %s.", sel.Action, price, task.Budget-spent, cfg.ReplyAction),
			}); err != nil {
				return out, err
			}
			logf("budget_refusal", map[string]any{"action": sel.Action, "price": price, "remaining": task.Budget - spent})
			continue
		}

		rr, rerr := deps.Runner.Run(ctx, sel.Action, sel.Args)
		spent += price
		if err := deps.Transcript.Append(ctx, Message{
			Role: RoleTool, Action: sel.Action, Result: rr.Result, TxID: rr.TxID,
			IsError: rerr != nil, Content: errText(rerr),
		}); err != nil {
			return out, err
		}
		logf("dispatch", map[string]any{"action": sel.Action, "price": price, "tx_id": rr.TxID, "error": rerr != nil})

		// Discovery is in-band: a lookup result expands the working set.
		if sel.Action == cfg.LookupAction && rerr == nil {
			mergeCandidates(ctx, deps, ws, parseLookup(rr.Result))
		}
	}
}

// prompt renders the task's seed conversation into a lookup query: the
// concatenated user/system content, which is what lookup ranks against.
func prompt(t Task) string {
	var b strings.Builder
	for _, m := range t.Messages {
		if m.Role == RoleUser || m.Role == RoleSystem {
			if m.Content != "" {
				b.WriteString(m.Content)
				b.WriteByte('\n')
			}
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

// refs is the deduplicated, sorted list of action refs offered to decide: the
// working set plus the always-available and intrinsic refs. Sorting makes the
// offered set deterministic (independent of Go map iteration order).
func (w *workingSet) refs(cfg Config) []string {
	seen := map[string]bool{}
	for ref := range w.by {
		seen[ref] = true
	}
	for _, ref := range cfg.AlwaysRefs {
		if ref != "" {
			seen[ref] = true
		}
	}
	for ref := range cfg.Intrinsics {
		seen[ref] = true
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// statsMessage builds the transient per-turn stats line describing the working
// set's track record. Returns ok=false when the set is empty.
func (w *workingSet) statsMessage() (Message, bool) {
	if len(w.by) == 0 {
		return Message{}, false
	}
	refs := make([]string, 0, len(w.by))
	for ref := range w.by {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return value(w.by[refs[i]]) > value(w.by[refs[j]]) })

	var b strings.Builder
	b.WriteString("Available tools and their track record (prefer proven tools; use an untested " +
		"tool only when no proven tool fits, or a proven one has failed here):\n")
	for _, ref := range refs {
		c := w.by[ref]
		fmt.Fprintf(&b, "  %s  price=%d", c.Ref, c.Price)
		if s := c.Stats; s.Known && s.Uses > 0 {
			fmt.Fprintf(&b, "  uses=%d  success=%d%%", s.Uses, pct(s.Successes, s.Uses))
			if s.RatingCount > 0 {
				fmt.Fprintf(&b, "  rating=%.2f (n=%d)", s.RatingEstimate, s.RatingCount)
			}
			if s.LatencyEstimate > 0 {
				fmt.Fprintf(&b, "  latency≈%.0f", s.LatencyEstimate)
			}
		} else {
			b.WriteString("  untested")
		}
		b.WriteByte('\n')
	}
	return Message{Role: RoleSystem, Content: strings.TrimRight(b.String(), "\n"), Kind: "stats"}, true
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

func pct(n, d int) int {
	if d == 0 {
		return 0
	}
	return n * 100 / d
}
