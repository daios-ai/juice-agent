package agent

import (
	"context"
	"errors"
	"testing"
)

// --- fakes -----------------------------------------------------------------

// scriptedDecider returns a fixed sequence of selections, one per turn, then
// errors if asked beyond the script. It records the refs it was offered.
type scriptedDecider struct {
	script    []Selection
	i         int
	lastRefs  []string
	lastMsgs  []Message
	decideErr error
	errAtTurn int
}

func (d *scriptedDecider) Decide(_ context.Context, msgs []Message, actions []string) (*Selection, error) {
	d.lastRefs, d.lastMsgs = actions, msgs
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

// memTranscript is an in-memory Transcript.
type memTranscript struct{ msgs []Message }

func (t *memTranscript) Append(_ context.Context, m Message) error {
	t.msgs = append(t.msgs, m)
	return nil
}
func (t *memTranscript) Messages() []Message { return t.msgs }

// replyIntrinsic is the terminal echo intrinsic used across tests.
func replyIntrinsic() map[string]Intrinsic {
	return map[string]Intrinsic{
		"@agent/reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil
		}},
	}
}

func baseConfig() Config {
	return Config{
		ReplyAction:    "@agent/reply",
		LookupAction:   "@sys/lookup",
		AlwaysRefs:     []string{"@sys/lookup"},
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
		{Action: "@agent/reply", Args: map[string]any{"answer": "it is noon"}},
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
		{Action: "@agent/reply", Args: map[string]any{"answer": "gave up on expensive"}},
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
		{Action: "@agent/reply", Args: map[string]any{"answer": "done"}},
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

func (d *loopingDecider) Decide(_ context.Context, _ []Message, _ []string) (*Selection, error) {
	return &Selection{Action: d.action}, nil
}

// alwaysErrorDecider fails every Decide call.
type alwaysErrorDecider struct{ calls int }

func (d *alwaysErrorDecider) Decide(context.Context, []Message, []string) (*Selection, error) {
	d.calls++
	return nil, errors.New("model produced no valid selection")
}

func TestPersistentDecideErrorEndsRunAfterCap(t *testing.T) {
	dec := &alwaysErrorDecider{}
	cfg := baseConfig()
	cfg.MaxDecideErrors = 2
	out, err := Run(context.Background(), Task{Budget: 100}, cfg, newDeps(dec, &fakeCatalog{}, &recordingRunner{}, &memTranscript{}))
	if err == nil {
		t.Fatal("expected error after the decide-error cap")
	}
	if out.Status != StatusFailed || out.Reason == "" {
		t.Fatalf("status=%q reason=%q, want failed with reason", out.Status, out.Reason)
	}
	// One past the cap: attempts 1,2 fed back, 3rd exceeds cap and ends.
	if dec.calls != 3 {
		t.Fatalf("decide calls = %d, want 3 (cap 2 + 1)", dec.calls)
	}
}

// errorThenReplyDecider fails once, then selects reply — modelling a transient
// fumble the loop recovers from by feeding the error back.
type errorThenReplyDecider struct{ calls int }

func (d *errorThenReplyDecider) Decide(_ context.Context, msgs []Message, _ []string) (*Selection, error) {
	d.calls++
	if d.calls == 1 {
		return nil, errors.New("malformed args")
	}
	return &Selection{Action: "@agent/reply", Args: map[string]any{"answer": "recovered"}}, nil
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
		{Action: "@agent/reply", Args: map[string]any{"answer": "ok"}},
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
