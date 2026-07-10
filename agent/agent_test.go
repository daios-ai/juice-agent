package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// --- fakes -----------------------------------------------------------------

// scriptedDecider returns a fixed sequence of selections, one per turn, then
// errors if asked beyond the script. It records what it was offered.
type scriptedDecider struct {
	script      []Selection
	i           int
	lastCatalog []Candidate
	lastTools   []Tool
	lastMsgs    []Message
	decideErr   error
	errAtTurn   int
}

func (d *scriptedDecider) Decide(_ context.Context, msgs []Message, catalog []Candidate, tools []Tool) (*Selection, error) {
	d.lastMsgs, d.lastCatalog, d.lastTools = msgs, catalog, tools
	if d.decideErr != nil && d.i == d.errAtTurn {
		return nil, d.decideErr
	}
	if d.i >= len(d.script) {
		return nil, errors.New("script exhausted")
	}
	s := d.script[d.i]
	d.i++
	return &s, nil
}

// fakeCatalog serves canned lookup results and per-ref meta.
type fakeCatalog struct {
	seed  []Candidate
	extra map[string][]Candidate // keyed by query substring match on first call
	meta  map[string]struct {
		price int
		stats ActionStats
	}
	lookups int
}

func (c *fakeCatalog) Lookup(_ context.Context, _ string, _ int) ([]Candidate, error) {
	c.lookups++
	return c.seed, nil
}

func (c *fakeCatalog) Meta(_ context.Context, ref string) (int, ActionStats, error) {
	if m, ok := c.meta[ref]; ok {
		return m.price, m.stats, nil
	}
	return 0, ActionStats{}, nil
}

// recordingRunner records dispatched calls and returns canned results/errors.
type recordingRunner struct {
	calls   []string
	results map[string]RunResult
	errs    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, action string, _ map[string]any) (RunResult, error) {
	r.calls = append(r.calls, action)
	if err := r.errs[action]; err != nil {
		return RunResult{}, err
	}
	if res, ok := r.results[action]; ok {
		return res, nil
	}
	return RunResult{Result: map[string]any{"ok": true}}, nil
}

// memTranscript is an in-memory Transcript with bracket support: msgs is this
// frame's view; all (shared across frames) is the durable record.
type memTranscript struct {
	bracket string
	msgs    []Message
	all     *[]Message
	seq     *int
}

func (t *memTranscript) init() {
	if t.all == nil {
		t.all = &[]Message{}
		t.seq = new(int)
	}
}
func (t *memTranscript) Append(_ context.Context, m Message) error {
	t.init()
	m.Bracket = t.bracket
	t.msgs = append(t.msgs, m)
	*t.all = append(*t.all, m)
	return nil
}
func (t *memTranscript) Messages() []Message { return t.msgs }
func (t *memTranscript) All() []Message      { t.init(); return *t.all }
func (t *memTranscript) Child(ctx context.Context, seed []Message) (Transcript, error) {
	t.init()
	*t.seq++
	child := &memTranscript{bracket: fmt.Sprintf("b%d", *t.seq), all: t.all, seq: t.seq}
	for _, m := range seed {
		if err := child.Append(ctx, m); err != nil {
			return nil, err
		}
	}
	return child, nil
}

// replyIntrinsic is the terminal echo intrinsic used across tests, keyed by the
// plain tool name.
func replyIntrinsic() map[string]Intrinsic {
	return map[string]Intrinsic{
		"reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil
		}},
	}
}

func baseConfig() Config {
	return Config{
		ReplyTool:      "reply",
		LookupAction:   "@sys/lookup",
		Tools:          []Tool{{Name: "reply", Description: "finish", InputSchema: map[string]any{"type": "object"}}},
		Always:         []Candidate{{Ref: "@sys/lookup"}},
		MaxTurns:       32,
		CandidateLimit: 24,
		LookupLimit:    8,
		Intrinsics:     replyIntrinsic(),
	}
}

func newDeps(d Decider, c Catalog, r Runner, t Transcript) Deps {
	return Deps{Decider: d, Catalog: c, Runner: r, Transcript: t}
}

// --- tests -----------------------------------------------------------------

func TestStopsWhenReplySelected(t *testing.T) {
	dec := &scriptedDecider{script: []Selection{
		{Action: "@sys/time"},
		{Action: "reply", Args: map[string]any{"answer": "it is noon"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "@sys/time"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered", out.Status)
	}
	if out.Answer["answer"] != "it is noon" {
		t.Fatalf("answer = %v, want echoed args", out.Answer)
	}
	if out.Turns != 2 {
		t.Fatalf("turns = %d, want 2", out.Turns)
	}
	if len(run.calls) != 1 || run.calls[0] != "@sys/time" {
		t.Fatalf("dispatched calls = %v, want [@sys/time]", run.calls)
	}
}

func TestBudgetRefusesThenContinues(t *testing.T) {
	// @expensive costs 100; budget is 10. The loop must refuse it (not dispatch)
	// and let the model recover with reply.
	dec := &scriptedDecider{script: []Selection{
		{Action: "@expensive"},
		{Action: "reply", Args: map[string]any{"answer": "gave up on expensive"}},
	}}
	cat := &fakeCatalog{
		seed: []Candidate{{Ref: "@expensive"}},
		meta: map[string]struct {
			price int
			stats ActionStats
		}{"@expensive": {price: 100}},
	}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Task{Budget: 10}, baseConfig(), newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered", out.Status)
	}
	if len(run.calls) != 0 {
		t.Fatalf("dispatched %v, want nothing (over budget)", run.calls)
	}
	if out.Spent != 0 {
		t.Fatalf("spent = %d, want 0", out.Spent)
	}
	// a budget-refusal record must be in the transcript
	found := false
	for _, m := range tr.msgs {
		if m.Kind == "budget" {
			found = true
		}
	}
	if !found {
		t.Fatal("no budget-refusal record appended")
	}
}

func TestBudgetSpendAccumulates(t *testing.T) {
	dec := &scriptedDecider{script: []Selection{
		{Action: "@a"},
		{Action: "@b"},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{
		seed: []Candidate{{Ref: "@a"}, {Ref: "@b"}},
		meta: map[string]struct {
			price int
			stats ActionStats
		}{"@a": {price: 3}, "@b": {price: 4}},
	}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, _ := Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if out.Spent != 7 {
		t.Fatalf("spent = %d, want 7", out.Spent)
	}
}

func TestTurnCapHalts(t *testing.T) {
	// The model keeps calling a free action forever; only the turn cap stops it.
	dec := &loopingDecider{action: "@sys/time"}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "@sys/time"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}
	cfg := baseConfig()
	cfg.MaxTurns = 5

	out, err := Run(context.Background(), Task{Budget: 100}, cfg, newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusFailed || out.Reason != "turn-cap" {
		t.Fatalf("got status=%q reason=%q, want failed/turn-cap", out.Status, out.Reason)
	}
	if out.Turns != 5 {
		t.Fatalf("turns = %d, want 5", out.Turns)
	}
}

// loopingDecider always selects the same action (a doom loop).
type loopingDecider struct{ action string }

func (d *loopingDecider) Decide(_ context.Context, _ []Message, _ []Candidate, _ []Tool) (*Selection, error) {
	return &Selection{Action: d.action}, nil
}

// alwaysErrorDecider fails every Decide call.
type alwaysErrorDecider struct{ calls int }

func (d *alwaysErrorDecider) Decide(context.Context, []Message, []Candidate, []Tool) (*Selection, error) {
	d.calls++
	return nil, errors.New("model produced no valid selection")
}

func TestPersistentDecideErrorEndsRunAfterCap(t *testing.T) {
	dec := &alwaysErrorDecider{}
	cfg := baseConfig()
	cfg.MaxDecideErrors = 2
	out, err := Run(context.Background(), Task{Budget: 100}, cfg, newDeps(dec, &fakeCatalog{}, &recordingRunner{}, &memTranscript{}))
	if err != nil {
		t.Fatalf("model failure should be a failed outcome, not a Go error: %v", err)
	}
	if out.Status != StatusFailed || !strings.HasPrefix(out.Reason, "decide-error") {
		t.Fatalf("status=%q reason=%q, want failed with a decide-error reason", out.Status, out.Reason)
	}
	// One past the cap: attempts 1,2 fed back, 3rd exceeds cap and ends.
	if dec.calls != 3 {
		t.Fatalf("decide calls = %d, want 3 (cap 2 + 1)", dec.calls)
	}
}

// errorThenReplyDecider fails once, then selects reply — modelling a transient
// fumble the loop recovers from by feeding the error back.
type errorThenReplyDecider struct{ calls int }

func (d *errorThenReplyDecider) Decide(_ context.Context, msgs []Message, _ []Candidate, _ []Tool) (*Selection, error) {
	d.calls++
	if d.calls == 1 {
		return nil, errors.New("malformed args")
	}
	return &Selection{Action: "reply", Args: map[string]any{"answer": "recovered"}}, nil
}

func TestSingleDecideErrorRecovers(t *testing.T) {
	dec := &errorThenReplyDecider{}
	tr := &memTranscript{}
	out, err := Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
	if err != nil {
		t.Fatalf("run should recover, got %v", err)
	}
	if out.Status != StatusAnswered || out.Answer["answer"] != "recovered" {
		t.Fatalf("outcome = %+v, want answered/recovered", out)
	}
	// The fed-back error must appear in the transcript for the model to see.
	found := false
	for _, m := range tr.msgs {
		if m.Kind == "decide-error" {
			found = true
		}
	}
	if !found {
		t.Fatal("decide error was not fed back into the transcript")
	}
}

func TestSteeringInjectedBeforeDecide(t *testing.T) {
	steer := []Message{{Role: RoleUser, Content: "actually, use @sys/time"}}
	dec := &scriptedDecider{script: []Selection{
		{Action: "reply", Args: map[string]any{"answer": "ok"}},
	}}
	cat := &fakeCatalog{}
	run := &recordingRunner{}
	tr := &memTranscript{}
	deps := newDeps(dec, cat, run, tr)
	once := false
	deps.Steering = func() []Message {
		if once {
			return nil
		}
		once = true
		return steer
	}

	Run(context.Background(), Task{Budget: 100}, baseConfig(), deps)
	// the steering message must appear in the transcript the decider saw
	found := false
	for _, m := range dec.lastMsgs {
		if m.Content == "actually, use @sys/time" {
			found = true
		}
	}
	if !found {
		t.Fatal("steering message was not injected before decide")
	}
}

func TestWorkingSetEvictsLowestValue(t *testing.T) {
	ws := newWorkingSet(2)
	// proven-good
	ws.put(Candidate{Ref: "@good", Stats: ActionStats{Known: true, Uses: 10, Successes: 10, RatingCount: 5, RatingEstimate: 1.0}})
	// persistent failer
	ws.put(Candidate{Ref: "@bad", Stats: ActionStats{Known: true, Uses: 10, Successes: 0, RatingCount: 5, RatingEstimate: 0.0}})
	// untested (neutral) — adding this exceeds the cap of 2, so the lowest-value
	// candidate (@bad) must be evicted.
	ws.put(Candidate{Ref: "@new"})

	if _, ok := ws.by["@bad"]; ok {
		t.Fatal("@bad (lowest value) should have been evicted")
	}
	if _, ok := ws.by["@good"]; !ok {
		t.Fatal("@good (highest value) should be retained")
	}
	if len(ws.by) != 2 {
		t.Fatalf("size = %d, want 2", len(ws.by))
	}
}

func TestCandidatesRankProvenFirstAndAlwaysPresent(t *testing.T) {
	ws := newWorkingSet(10)
	ws.put(Candidate{Ref: "@proven", Price: 5, Stats: ActionStats{Known: true, Uses: 8, Successes: 8, RatingCount: 3, RatingEstimate: 0.9}})
	ws.put(Candidate{Ref: "@fresh", Price: 2})

	cfg := Config{Always: []Candidate{{Ref: "@sys/lookup"}}}
	cands := ws.candidates(cfg)
	// Value-sorted: proven before fresh.
	var order []string
	for _, c := range cands {
		order = append(order, c.Ref)
	}
	pi, fi := indexOf(order, "@proven"), indexOf(order, "@fresh")
	if pi < 0 || fi < 0 || pi > fi {
		t.Fatalf("proven should rank before fresh: %v", order)
	}
	// The always-offered candidate is present even though it was never put().
	if indexOf(order, "@sys/lookup") < 0 {
		t.Fatalf("always candidate missing: %v", order)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestParseLookup(t *testing.T) {
	res := map[string]any{"results": []any{
		map[string]any{"action": "@sys/time", "description": "current time", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"description": "no action ref — skipped"},
		map[string]any{"action": "@sys/web", "description": "fetch a page"},
	}}
	got := parseLookup(res)
	if len(got) != 2 {
		t.Fatalf("parsed %d candidates, want 2", len(got))
	}
	if got[0].Ref != "@sys/time" || got[1].Ref != "@sys/web" {
		t.Fatalf("refs = %q,%q", got[0].Ref, got[1].Ref)
	}
}

func TestMergeEnrichesWithMeta(t *testing.T) {
	ws := newWorkingSet(10)
	cat := &fakeCatalog{meta: map[string]struct {
		price int
		stats ActionStats
	}{"@sys/web": {price: 3, stats: ActionStats{Known: true, Uses: 4, Successes: 3}}}}
	deps := Deps{Catalog: cat}
	mergeCandidates(context.Background(), deps, ws, []Candidate{{Ref: "@sys/web", Description: "fetch"}})

	c := ws.by["@sys/web"]
	if c.Price != 3 {
		t.Fatalf("price = %d, want 3 (from Meta)", c.Price)
	}
	if !c.Stats.Known || c.Stats.Uses != 4 {
		t.Fatalf("stats not enriched: %+v", c.Stats)
	}
}

func TestLookupResultMergesIntoWorkingSet(t *testing.T) {
	// A lookup dispatch must expand the offered refs on the next turn.
	dec := &refCapturingDecider{script: []Selection{
		{Action: "@sys/lookup", Args: map[string]any{"query": "x"}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: nil}
	run := &recordingRunner{results: map[string]RunResult{
		"@sys/lookup": {Result: map[string]any{"results": []any{
			map[string]any{"action": "@sys/web", "description": "fetch"},
		}}},
	}}
	tr := &memTranscript{}
	Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))

	if !dec.everOffered("@sys/web") {
		t.Fatalf("@sys/web from lookup was never offered to decide; offered=%v", dec.allRefs)
	}
}

type refCapturingDecider struct {
	script  []Selection
	i       int
	allRefs [][]string
}

func (d *refCapturingDecider) Decide(_ context.Context, _ []Message, catalog []Candidate, _ []Tool) (*Selection, error) {
	refs := make([]string, 0, len(catalog))
	for _, c := range catalog {
		refs = append(refs, c.Ref)
	}
	d.allRefs = append(d.allRefs, refs)
	if d.i >= len(d.script) {
		return &Selection{Action: "reply", Args: map[string]any{"answer": "fallback"}}, nil
	}
	s := d.script[d.i]
	d.i++
	return &s, nil
}

func (d *refCapturingDecider) everOffered(ref string) bool {
	for _, refs := range d.allRefs {
		for _, r := range refs {
			if r == ref {
				return true
			}
		}
	}
	return false
}

func TestMissingRequiredArgFedBack(t *testing.T) {
	// The model selects the reply tool WITHOUT the required "answer" arg the first
	// time, then supplies it. The loop must feed the miss back, not terminate.
	dec := &scriptedDecider{script: []Selection{
		{Action: "reply", Args: map[string]any{}},                      // missing "answer"
		{Action: "reply", Args: map[string]any{"answer": "recovered"}}, // fixed
	}}
	cfg := baseConfig()
	cfg.Tools = []Tool{{Name: "reply", InputSchema: map[string]any{
		"type": "object", "required": []any{"answer"}}}}
	tr := &memTranscript{}
	out, err := Run(context.Background(), Task{Budget: 100}, cfg, newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered || out.Answer["answer"] != "recovered" {
		t.Fatalf("outcome = %+v", out)
	}
	found := false
	for _, m := range tr.msgs {
		if m.Kind == "decide-error" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing-arg was not fed back to the model")
	}
}

func TestLookupResultTrimmedInTranscript(t *testing.T) {
	// A lookup dispatch persists only ref+description+score (no schemas), while
	// the working set retains the full candidate for the client to render.
	dec := &scriptedDecider{script: []Selection{
		{Action: "@sys/lookup", Args: map[string]any{"query": "x"}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	run := &recordingRunner{results: map[string]RunResult{
		"@sys/lookup": {Result: map[string]any{"results": []any{
			map[string]any{"action": "@sys/web", "description": "fetch",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{}}}},
		}}},
	}}
	tr := &memTranscript{}
	Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, run, tr))

	// find the persisted lookup tool-result
	var lookupRes map[string]any
	for _, m := range tr.msgs {
		if m.Role == RoleTool && m.Action == "@sys/lookup" {
			lookupRes = m.Result
		}
	}
	if lookupRes == nil {
		t.Fatal("no lookup result persisted")
	}
	results, _ := lookupRes["results"].([]any)
	first, _ := results[0].(map[string]any)
	if first["action"] != "@sys/web" || first["description"] != "fetch" {
		t.Fatalf("trimmed result missing ref/desc: %v", first)
	}
	if _, hasSchema := first["input_schema"]; hasSchema {
		t.Fatalf("schema should be trimmed from the transcript: %v", first)
	}
}

func subtaskConfig() Config {
	cfg := baseConfig()
	cfg.SubtaskTool = "subtask"
	cfg.Tools = append(cfg.Tools, Tool{Name: "subtask", Description: "open a subtask",
		InputSchema: map[string]any{"type": "object", "required": []any{"instruction"}}})
	return cfg
}

func TestSubtaskOpensAndRollsUp(t *testing.T) {
	// root opens a subtask; the bracket answers; the parent sees ONE rolled-up
	// result, not the bracket's internal turns.
	dec := &scriptedDecider{script: []Selection{
		{Action: "subtask", Args: map[string]any{"instruction": "find X"}},
		{Action: "reply", Args: map[string]any{"answer": "X=42"}},        // inside bracket
		{Action: "reply", Args: map[string]any{"answer": "final: X=42"}}, // root
	}}
	tr := &memTranscript{}
	out, err := Run(context.Background(), Task{Budget: 100}, subtaskConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered || out.Answer["answer"] != "final: X=42" {
		t.Fatalf("outcome = %+v", out)
	}
	// The parent view carries the subtask rollup as one tool-result...
	var rollup map[string]any
	for _, m := range tr.msgs {
		if m.Role == RoleTool && m.Action == "subtask" {
			rollup = m.Result
		}
		if m.Bracket != "" {
			t.Fatalf("bracket turn leaked into the parent view: %+v", m)
		}
	}
	if rollup == nil || rollup["answer"] != "X=42" {
		t.Fatalf("subtask rollup missing/wrong: %v", rollup)
	}
	// ...while the durable record keeps the bracket's internal turns (tagged).
	bracketSeen := false
	for _, m := range tr.All() {
		if m.Bracket != "" {
			bracketSeen = true
		}
	}
	if !bracketSeen {
		t.Fatal("bracket turns absent from All() durable record")
	}
}

func TestSubtaskSharesTurnCap(t *testing.T) {
	// A bracket's turns count against the ONE shared cap: cap 4 = 1 root (subtask)
	// + 3 bracket dispatches, then exhausted — not 4-per-frame.
	dec := &loopingSubtaskDecider{}
	cfg := subtaskConfig()
	cfg.MaxTurns = 4
	run := &recordingRunner{}
	out, _ := Run(context.Background(), Task{Budget: 1000}, cfg, newDeps(dec, &fakeCatalog{}, run, &memTranscript{}))
	if out.Status != StatusFailed || out.Reason != "turn-cap" {
		t.Fatalf("outcome = %+v, want failed/turn-cap", out)
	}
	if out.Turns != 4 {
		t.Fatalf("total turns = %d, want 4 (shared cap, not per-frame)", out.Turns)
	}
	// root turn opened the bracket (not dispatched); the 3 remaining turns were
	// dispatches inside the bracket.
	if len(run.calls) != 3 {
		t.Fatalf("dispatched %d, want 3", len(run.calls))
	}
}

// loopingSubtaskDecider opens a subtask on its first call, then loops a free
// action forever (inside the bracket).
type loopingSubtaskDecider struct{ n int }

func (d *loopingSubtaskDecider) Decide(_ context.Context, _ []Message, _ []Candidate, _ []Tool) (*Selection, error) {
	d.n++
	if d.n == 1 {
		return &Selection{Action: "subtask", Args: map[string]any{"instruction": "loop"}}, nil
	}
	return &Selection{Action: "@sys/time"}, nil
}
