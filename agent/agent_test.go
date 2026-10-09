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

// fakeCatalog serves canned lookup results and per-ref authoritative reads. A
// ref absent from read reads as a free, effect-free candidate.
type fakeCatalog struct {
	seed    []Candidate
	read    map[string]Candidate
	lookups int
}

func (c *fakeCatalog) Lookup(_ context.Context, _ string, _ int) ([]Candidate, error) {
	c.lookups++
	return c.seed, nil
}

func (c *fakeCatalog) Read(_ context.Context, ref string) (Candidate, error) {
	if r, ok := c.read[ref]; ok {
		return r, nil
	}
	return Candidate{Ref: ref}, nil
}

// recordingRunner records dispatched calls, their quote pins and run keys, and
// returns canned results/errors (a canned result rides with a canned error, as a
// failed call's settlement does).
type recordingRunner struct {
	calls   []string
	quotes  []string
	keys    []string
	results map[string]RunResult
	errs    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, action string, _ map[string]any, quoteHash, key string) (RunResult, error) {
	r.calls = append(r.calls, action)
	r.quotes = append(r.quotes, quoteHash)
	r.keys = append(r.keys, key)
	res, ok := r.results[action]
	if err := r.errs[action]; err != nil {
		return res, err
	}
	if ok {
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
		LookupAction:   "sys@k/lookup",
		Tools:          []Tool{{Name: "reply", Description: "finish", InputSchema: map[string]any{"type": "object"}}},
		Always:         []Candidate{{Ref: "sys@k/lookup"}},
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
		{Action: "sys@k/time"},
		{Action: "reply", Args: map[string]any{"answer": "it is noon"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "sys@k/time"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
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
	if len(run.calls) != 1 || run.calls[0] != "sys@k/time" {
		t.Fatalf("dispatched calls = %v, want [sys@k/time]", run.calls)
	}
}

func TestBudgetRefusesThenContinues(t *testing.T) {
	// expensive costs 100; budget is 10. The loop must refuse it (not dispatch)
	// and let the model recover with reply.
	dec := &scriptedDecider{script: []Selection{
		{Action: "expensive"},
		{Action: "reply", Args: map[string]any{"answer": "gave up on expensive"}},
	}}
	cat := &fakeCatalog{
		seed: []Candidate{{Ref: "expensive"}},
		read: map[string]Candidate{"expensive": {Ref: "expensive", Price: 100}},
	}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Job{Budget: 10}, baseConfig(), newDeps(dec, cat, run, tr))
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
		{Action: "a"},
		{Action: "b"},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{
		seed: []Candidate{{Ref: "a"}, {Ref: "b"}},
		read: map[string]Candidate{"a": {Ref: "a", Price: 3}, "b": {Ref: "b", Price: 4}},
	}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, _ := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if out.Spent != 7 {
		t.Fatalf("spent = %d, want 7", out.Spent)
	}
}

func TestSpentIsTheReportedCharge(t *testing.T) {
	// spent follows the kernel's settlement, not the quote: a failed call charges
	// what its sub-work consumed, a moved quote charges nothing, and a call with no
	// reported settlement (parked) counts at its price, which it may still draw.
	dec := &scriptedDecider{script: []Selection{
		{Action: "a"}, {Action: "b"}, {Action: "c"},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{
		seed: []Candidate{{Ref: "a"}, {Ref: "b"}, {Ref: "c"}},
		read: map[string]Candidate{"a": {Ref: "a", Price: 5}, "b": {Ref: "b", Price: 4}, "c": {Ref: "c", Price: 3}},
	}
	run := &recordingRunner{
		results: map[string]RunResult{"a": {TxID: "tx-a", Charge: 2}},
		errs: map[string]error{
			"a": errors.New("execution_failed"),
			"b": fmt.Errorf("refused: %w", ErrTermsChanged),
			"c": errors.New("timeout: parked"),
		},
	}
	tr := &memTranscript{}
	out, _ := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if out.Spent != 2+0+3 {
		t.Fatalf("spent = %d, want 5", out.Spent)
	}
	for _, m := range tr.msgs {
		if m.Role == RoleTool && m.Action == "a" && (m.TxID != "tx-a" || !m.IsError) {
			t.Fatalf("failed call record = %+v, want its tx_id kept with the error", m)
		}
	}
}

func TestEachDispatchHasItsOwnKey(t *testing.T) {
	dec := &scriptedDecider{script: []Selection{
		{Action: "a"}, {Action: "b"},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "a"}, {Ref: "b"}}}
	run := &recordingRunner{}
	Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, &memTranscript{}))
	if len(run.keys) != 2 || run.keys[0] == "" || run.keys[0] == run.keys[1] {
		t.Fatalf("keys = %q, want two distinct non-empty run keys", run.keys)
	}
}

func TestTurnCapHalts(t *testing.T) {
	// The model keeps calling a free action forever; only the turn cap stops it.
	dec := &loopingDecider{action: "sys@k/time"}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "sys@k/time"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}
	cfg := baseConfig()
	cfg.MaxTurns = 5
	cfg.MaxRepeats = -1 // isolate the turn cap from the repeat guard

	out, err := Run(context.Background(), Job{Budget: 100}, cfg, newDeps(dec, cat, run, tr))
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
	out, err := Run(context.Background(), Job{Budget: 100}, cfg, newDeps(dec, &fakeCatalog{}, &recordingRunner{}, &memTranscript{}))
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
	out, err := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
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
	steer := []Message{{Role: RoleUser, Content: "actually, use sys@k/time"}}
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

	Run(context.Background(), Job{Budget: 100}, baseConfig(), deps)
	// the steering message must appear in the transcript the decider saw
	found := false
	for _, m := range dec.lastMsgs {
		if m.Content == "actually, use sys@k/time" {
			found = true
		}
	}
	if !found {
		t.Fatal("steering message was not injected before decide")
	}
}

func TestWorkingSetEvictsLeastRecent(t *testing.T) {
	ws := newWorkingSet(2)
	ws.put(Candidate{Ref: "a"})
	ws.put(Candidate{Ref: "b"})
	ws.put(Candidate{Ref: "a"}) // refreshed: now the most recent
	ws.put(Candidate{Ref: "c"}) // over the cap: the least recent (b) leaves

	if _, ok := ws.by["b"]; ok {
		t.Fatal("b (least recently merged) should have been evicted")
	}
	if len(ws.by) != 2 || len(ws.order) != 2 {
		t.Fatalf("size = %d/%d, want 2", len(ws.by), len(ws.order))
	}
}

func TestCandidatesAlwaysFirstThenMostRecent(t *testing.T) {
	ws := newWorkingSet(10)
	ws.put(Candidate{Ref: "old"})
	ws.put(Candidate{Ref: "new"})
	ws.put(Candidate{Ref: "sys@k/lookup"}) // an always ref merged again is not listed twice

	var order []string
	for _, c := range ws.candidates(Config{Always: []Candidate{{Ref: "sys@k/lookup"}}}) {
		order = append(order, c.Ref)
	}
	if strings.Join(order, ",") != "sys@k/lookup,new,old" {
		t.Fatalf("order = %v, want [sys@k/lookup new old]", order)
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
		map[string]any{"action": "sys@k/time", "description": "current time", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"description": "no action ref — skipped"},
		map[string]any{"action": "sys@k/web", "description": "fetch a page"},
	}}
	got := ParseLookup(res)
	if len(got) != 2 {
		t.Fatalf("parsed %d candidates, want 2", len(got))
	}
	if got[0].Ref != "sys@k/time" || got[1].Ref != "sys@k/web" {
		t.Fatalf("refs = %q,%q", got[0].Ref, got[1].Ref)
	}
}

func TestMergeTakesAuthoritativeReadAndDropsEffects(t *testing.T) {
	ws := newWorkingSet(10)
	cat := &fakeCatalog{read: map[string]Candidate{
		"sys@k/web":      {Ref: "sys@k/web", Price: 3, QuoteHash: "q1"},
		"sys@k/transfer": {Ref: "sys@k/transfer", Effect: "transfer"},
	}}
	mergeCandidates(context.Background(), Deps{Catalog: cat}, ws,
		[]Candidate{{Ref: "sys@k/web", Description: "fetch"}, {Ref: "sys@k/transfer"}})

	if c := ws.by["sys@k/web"]; c.Price != 3 || c.QuoteHash != "q1" {
		t.Fatalf("sys@k/web = %+v, want the read's price and quote", c)
	}
	if _, ok := ws.by["sys@k/transfer"]; ok {
		t.Fatal("an action with a value effect must not enter the working set")
	}
}

func TestUnofferedActionIsNotDispatched(t *testing.T) {
	// The model names an action it was never offered: the loop must feed that back
	// instead of dispatching it at an unknown price.
	dec := &scriptedDecider{script: []Selection{
		{Action: "x/costly"},
		{Action: "reply", Args: map[string]any{"answer": "ok"}},
	}}
	run := &recordingRunner{}
	tr := &memTranscript{}
	out, err := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, run, tr))
	if err != nil || out.Status != StatusAnswered {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("dispatched %v, want nothing", run.calls)
	}
	if !hasKind(tr.msgs, "decide-error") {
		t.Fatal("unoffered selection was not fed back")
	}
}

func TestQuotePinnedAndMovedTermsEvict(t *testing.T) {
	// The candidate's quote travels with its dispatch; a moved quote evicts it, so
	// a second selection of it is refused until a fresh read re-offers it.
	dec := &scriptedDecider{script: []Selection{
		{Action: "a"},
		{Action: "a"},
		{Action: "reply", Args: map[string]any{"answer": "ok"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "a"}}, read: map[string]Candidate{"a": {Ref: "a", QuoteHash: "q-a"}}}
	run := &recordingRunner{errs: map[string]error{"a": fmt.Errorf("refused: %w", ErrTermsChanged)}}
	cfg := baseConfig()
	cfg.MaxRepeats = -1
	tr := &memTranscript{}
	Run(context.Background(), Job{Budget: 100}, cfg, newDeps(dec, cat, run, tr))

	if len(run.calls) != 1 || run.quotes[0] != "q-a" {
		t.Fatalf("calls=%v quotes=%v, want one call pinned to q-a", run.calls, run.quotes)
	}
	if !hasKind(tr.msgs, "decide-error") {
		t.Fatal("the evicted candidate's second selection should be refused")
	}
}

func hasKind(msgs []Message, kind string) bool {
	for _, m := range msgs {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

func TestLookupResultMergesIntoWorkingSet(t *testing.T) {
	// A lookup dispatch must expand the offered refs on the next turn.
	dec := &refCapturingDecider{script: []Selection{
		{Action: "sys@k/lookup", Args: map[string]any{"query": "x"}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: nil}
	run := &recordingRunner{results: map[string]RunResult{
		"sys@k/lookup": {Result: map[string]any{"results": []any{
			map[string]any{"action": "sys@k/web", "description": "fetch"},
		}}},
	}}
	tr := &memTranscript{}
	Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))

	if !dec.everOffered("sys@k/web") {
		t.Fatalf("sys@k/web from lookup was never offered to decide; offered=%v", dec.allRefs)
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
	out, err := Run(context.Background(), Job{Budget: 100}, cfg, newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
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
		{Action: "sys@k/lookup", Args: map[string]any{"query": "x"}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	run := &recordingRunner{results: map[string]RunResult{
		"sys@k/lookup": {Result: map[string]any{"results": []any{
			map[string]any{"action": "sys@k/web", "description": "fetch",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{}}}},
		}}},
	}}
	tr := &memTranscript{}
	Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, run, tr))

	// find the persisted lookup tool-result
	var lookupRes map[string]any
	for _, m := range tr.msgs {
		if m.Role == RoleTool && m.Action == "sys@k/lookup" {
			lookupRes = m.Result
		}
	}
	if lookupRes == nil {
		t.Fatal("no lookup result persisted")
	}
	results, _ := lookupRes["results"].([]any)
	first, _ := results[0].(map[string]any)
	if first["action"] != "sys@k/web" || first["description"] != "fetch" {
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
	out, err := Run(context.Background(), Job{Budget: 100}, subtaskConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
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
	cfg.MaxRepeats = -1 // isolate the shared turn cap from the repeat guard
	run := &recordingRunner{}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "sys@k/time"}}}
	out, _ := Run(context.Background(), Job{Budget: 1000}, cfg, newDeps(dec, cat, run, &memTranscript{}))
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
	return &Selection{Action: "sys@k/time"}, nil
}

// toolNamingDecider records the harness tools it is offered on each turn, opens a
// subtask once, then replies from whatever frame it is in.
type toolNamingDecider struct {
	offered [][]string
	opened  bool
}

func (d *toolNamingDecider) Decide(_ context.Context, _ []Message, _ []Candidate, tools []Tool) (*Selection, error) {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	d.offered = append(d.offered, names)
	if !d.opened {
		d.opened = true
		return &Selection{Action: "subtask", Args: map[string]any{"instruction": "do it"}}, nil
	}
	return &Selection{Action: "reply", Args: map[string]any{"answer": "done"}}, nil
}

func TestSubtaskChildNotOfferedSubtaskTool(t *testing.T) {
	// The child of a subtask must NOT be offered the subtask tool — nesting is
	// capped at one sublevel so a cloned subtask cannot re-delegate and spiral.
	dec := &toolNamingDecider{}
	out, err := Run(context.Background(), Job{Budget: 100}, subtaskConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, &memTranscript{}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("outcome = %+v, want answered", out)
	}
	// Turn 1 is the root (offered subtask, and it opened one); turn 2 runs inside
	// the bracket and must not see subtask.
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if len(dec.offered) < 2 {
		t.Fatalf("expected at least 2 decide calls, got %d", len(dec.offered))
	}
	if !has(dec.offered[0], "subtask") {
		t.Fatalf("root turn should be offered subtask, offered=%v", dec.offered[0])
	}
	if has(dec.offered[1], "subtask") {
		t.Fatalf("subtask child must NOT be offered subtask, offered=%v", dec.offered[1])
	}
	if !has(dec.offered[1], "reply") {
		t.Fatalf("subtask child should still be offered reply, offered=%v", dec.offered[1])
	}
}

// --- repeat guard (§4c) -----------------------------------------------------

func countKind(msgs []Message, kind string) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

func TestRepeatChallengeThenBackDown(t *testing.T) {
	// The model posts the same successful action twice; the second is a repeat, so
	// the loop drops it (does NOT dispatch) and nudges, and the model backs down
	// with reply. Exactly ONE dispatch, one nudge, a clean answered outcome.
	dec := &scriptedDecider{script: []Selection{
		{Action: "chat/message", Args: map[string]any{"text": "read it"}},
		{Action: "chat/message", Args: map[string]any{"text": "read it"}}, // repeat → challenged
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "chat/message"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered", out.Status)
	}
	if len(run.calls) != 1 {
		t.Fatalf("dispatched %v, want exactly one (the repeat was challenged, not run)", run.calls)
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 1 {
		t.Fatalf("repeat-challenge messages = %d, want 1", n)
	}
}

// paraphrasingDecider keeps selecting the same action but with a *different*
// argument every turn — the real doom loop (a sampled model rewords rather than
// repeats verbatim). It optionally backs down with reply after `backdownAfter`
// selections (0 = never backs down).
type paraphrasingDecider struct {
	action        string
	n             int
	backdownAfter int
}

func (d *paraphrasingDecider) Decide(_ context.Context, _ []Message, _ []Candidate, _ []Tool) (*Selection, error) {
	d.n++
	if d.backdownAfter > 0 && d.n > d.backdownAfter {
		return &Selection{Action: "reply", Args: map[string]any{"answer": "done"}}, nil
	}
	// Distinct text each turn, so an args-based guard would never match.
	return &Selection{Action: d.action, Args: map[string]any{"text": fmt.Sprintf("phrasing %d", d.n)}}, nil
}

func TestParaphraseLoopIntercepted(t *testing.T) {
	// The case the old byte-identity guard missed: same action, different args every
	// turn. Detection is action-based, so the repeats are dropped (never dispatched)
	// and the model, nudged, backs down with reply. Exactly ONE dispatch.
	dec := &paraphrasingDecider{action: "chat/message", backdownAfter: 3}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "chat/message"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Job{Budget: 1000}, baseConfig(), newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered", out.Status)
	}
	if len(run.calls) != 1 {
		t.Fatalf("dispatched %v, want exactly one (paraphrased repeats dropped, not run)", run.calls)
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 1 {
		t.Fatalf("repeat-challenge messages = %d, want 1 (a single nudge, not accumulated)", n)
	}
}

func TestRepeatLoopHardStops(t *testing.T) {
	// A stubborn paraphrase loop that never backs down: repeats are dropped, and
	// after maxRepeats (default 3) consecutive drops the frame hard-stops. The
	// action dispatched exactly ONCE; one nudge; terminates well under the turn cap.
	dec := &paraphrasingDecider{action: "chat/message"}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "chat/message"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}
	cfg := baseConfig() // MaxRepeats 0 → default 3

	out, err := Run(context.Background(), Job{Budget: 1000}, cfg, newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusFailed || out.Reason != "repeat-loop" {
		t.Fatalf("outcome = %+v, want failed/repeat-loop", out)
	}
	if len(run.calls) != 1 {
		t.Fatalf("dispatched %d, want 1 (only the first call; repeats never dispatch)", len(run.calls))
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 1 {
		t.Fatalf("repeat-challenge messages = %d, want 1 (single nudge, not accumulated)", n)
	}
	if out.Turns >= cfg.MaxTurns {
		t.Fatalf("turns = %d, want well under the cap %d (terminated on repeat-loop)", out.Turns, cfg.MaxTurns)
	}
}

func TestRepeatChangeOfCourseResets(t *testing.T) {
	// After the nudge the model picks a DIFFERENT action; that is progress, so the
	// repeat streak resets and there is no hard-stop.
	dec := &scriptedDecider{script: []Selection{
		{Action: "a", Args: map[string]any{"x": 1}},
		{Action: "a", Args: map[string]any{"x": 1}}, // repeat → challenged
		{Action: "b", Args: map[string]any{"y": 2}}, // change of course
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "a"}, {Ref: "b"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, err := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered {
		t.Fatalf("outcome = %+v, want answered (no repeat-loop)", out)
	}
	if len(run.calls) != 2 || run.calls[0] != "a" || run.calls[1] != "b" {
		t.Fatalf("dispatched %v, want [a b] (one a, challenge, then b)", run.calls)
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 1 {
		t.Fatalf("repeat-challenge messages = %d, want 1", n)
	}
}

func TestAlternationIsNotARepeat(t *testing.T) {
	// A, B, A, B is not a repeat loop — the guard is consecutive-only, so no
	// challenge is ever issued.
	dec := &scriptedDecider{script: []Selection{
		{Action: "a", Args: map[string]any{"x": 1}},
		{Action: "b", Args: map[string]any{"x": 1}},
		{Action: "a", Args: map[string]any{"x": 1}},
		{Action: "b", Args: map[string]any{"x": 1}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "a"}, {Ref: "b"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}

	out, _ := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))
	if out.Status != StatusAnswered {
		t.Fatalf("outcome = %+v, want answered", out)
	}
	if len(run.calls) != 4 {
		t.Fatalf("dispatched %d, want 4 (no challenge inserted)", len(run.calls))
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 0 {
		t.Fatalf("repeat-challenge messages = %d, want 0 (alternation is not a repeat)", n)
	}
}

func TestReplyExemptFromRepeatGuard(t *testing.T) {
	// reply is the escape hatch; it terminates on first selection and is never
	// challenged as a repeat.
	dec := &scriptedDecider{script: []Selection{
		{Action: "reply", Args: map[string]any{"answer": "done"}},
		{Action: "reply", Args: map[string]any{"answer": "done"}},
	}}
	tr := &memTranscript{}
	out, _ := Run(context.Background(), Job{Budget: 100}, baseConfig(), newDeps(dec, &fakeCatalog{}, &recordingRunner{}, tr))
	if out.Status != StatusAnswered || out.Turns != 1 {
		t.Fatalf("outcome = %+v, want answered on turn 1", out)
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 0 {
		t.Fatalf("repeat-challenge messages = %d, want 0", n)
	}
}

// bracketRepeatDecider opens a subtask once, loops a free action inside the
// bracket, and replies at the root as soon as it sees the subtask rollup.
type bracketRepeatDecider struct{ opened bool }

func (d *bracketRepeatDecider) Decide(_ context.Context, msgs []Message, _ []Candidate, _ []Tool) (*Selection, error) {
	for _, m := range msgs {
		if m.Role == RoleTool && m.Action == "subtask" {
			return &Selection{Action: "reply", Args: map[string]any{"answer": "handled"}}, nil
		}
	}
	if !d.opened {
		d.opened = true
		return &Selection{Action: "subtask", Args: map[string]any{"instruction": "loop"}}, nil
	}
	return &Selection{Action: "sys@k/time", Args: map[string]any{"text": "same"}}, nil
}

func TestBracketRepeatIsolation(t *testing.T) {
	// A repeat inside a bracket hard-stops the CHILD frame (rollup error), without
	// tripping the parent's own guard. The parent recovers and answers.
	dec := &bracketRepeatDecider{}
	cfg := subtaskConfig()
	cfg.MaxRepeats = 1 // child hard-stops after 1 dropped repeat
	run := &recordingRunner{}
	tr := &memTranscript{}

	cat := &fakeCatalog{seed: []Candidate{{Ref: "sys@k/time"}}}
	out, err := Run(context.Background(), Job{Budget: 1000}, cfg, newDeps(dec, cat, run, tr))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAnswered || out.Answer["answer"] != "handled" {
		t.Fatalf("outcome = %+v, want answered/handled (parent recovered)", out)
	}
	if len(run.calls) != 1 {
		t.Fatalf("child dispatched %d, want 1 (only the first call; repeats dropped)", len(run.calls))
	}
	// The parent's rolled-up subtask result carries the child's repeat-loop error.
	var rollup map[string]any
	for _, m := range tr.msgs {
		if m.Role == RoleTool && m.Action == "subtask" {
			rollup = m.Result
		}
	}
	if rollup == nil || rollup["error"] != "repeat-loop" {
		t.Fatalf("subtask rollup = %v, want {error: repeat-loop}", rollup)
	}
}

func TestRepeatGuardDisabled(t *testing.T) {
	// MaxRepeats < 0 disables the guard: a looping action runs every turn until the
	// turn cap, exactly as before, with no challenge inserted.
	dec := &paraphrasingDecider{action: "sys@k/time"}
	cat := &fakeCatalog{seed: []Candidate{{Ref: "sys@k/time"}}}
	run := &recordingRunner{}
	tr := &memTranscript{}
	cfg := baseConfig()
	cfg.MaxTurns = 5
	cfg.MaxRepeats = -1

	out, _ := Run(context.Background(), Job{Budget: 1000}, cfg, newDeps(dec, cat, run, tr))
	if out.Status != StatusFailed || out.Reason != "turn-cap" {
		t.Fatalf("outcome = %+v, want failed/turn-cap", out)
	}
	if len(run.calls) != 5 {
		t.Fatalf("dispatched %d, want 5 (every turn, guard disabled)", len(run.calls))
	}
	if n := countKind(tr.msgs, "repeat-challenge"); n != 0 {
		t.Fatalf("repeat-challenge messages = %d, want 0 (guard disabled)", n)
	}
}
