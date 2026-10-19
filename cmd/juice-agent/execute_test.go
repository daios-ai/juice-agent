package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// fakeKernel implements the full Kernel interface with scripted
// behaviour, so the executor is exercised end to end without a real kernel.
type fakeKernel struct {
	script []agent.Selection
	i      int
	rated  map[string]int
	seen   [][]agent.Message // the view each Decide was given
}

func (k *fakeKernel) Decide(_ context.Context, msgs []agent.Message, _ []agent.Candidate, _ []agent.Tool) (*agent.Selection, error) {
	k.seen = append(k.seen, msgs)
	if k.i >= len(k.script) {
		return &agent.Selection{Action: "reply", Args: map[string]any{"answer": "fallback"}}, nil
	}
	s := k.script[k.i]
	k.i++
	return &s, nil
}

// Lookup offers every action the script selects, so the loop may dispatch it.
func (k *fakeKernel) Lookup(context.Context, string, int) ([]agent.Candidate, error) {
	var out []agent.Candidate
	for _, s := range k.script {
		out = append(out, agent.Candidate{Ref: s.Action})
	}
	return out, nil
}
func (k *fakeKernel) Read(_ context.Context, ref string) (agent.Candidate, error) {
	return agent.Candidate{Ref: ref}, nil
}
func (k *fakeKernel) Run(_ context.Context, action string, _ map[string]any, _, _ string) (agent.RunResult, error) {
	return agent.RunResult{Result: map[string]any{"ran": action}, TxID: "tx-" + action}, nil
}
func (k *fakeKernel) Chat(context.Context, []agent.Message) (string, error) { return "summary", nil }
func (k *fakeKernel) ChatJSON(context.Context, []agent.Message, map[string]any) (any, error) {
	return map[string]any{"ratings": []any{map[string]any{"index": float64(0), "rating": float64(1), "note": "good"}}}, nil
}
func (k *fakeKernel) Rate(_ context.Context, txID string, rating int, _ string) error {
	if k.rated == nil {
		k.rated = map[string]int{}
	}
	k.rated[txID] = rating
	return nil
}

func replyIntrinsics() map[string]agent.Intrinsic {
	return map[string]agent.Intrinsic{
		"reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil
		}},
	}
}

func testExecutor(dir string, k Kernel) *Executor {
	return &Executor{
		Kernel:      k,
		PersonasDir: dir,
		Retry:       RetryPolicy{MaxRetries: 0},
		RetryOK:     func(error) bool { return false },
		Loop: agent.Config{
			ReplyTool: "reply", ActionLookup: "sys@k/lookup",
			Always: []agent.Candidate{{Ref: "sys@k/lookup"}}, MaxTurns: 10, Intrinsics: replyIntrinsics(),
		},
		RateTasks: true,
		Handle:    "agent@k",
		AgentPath: filepath.Join(dir, "agent.md"),
	}
}

func userJob(id, persona, prompt string) agent.Job {
	return agent.Job{ID: id, Persona: persona, Budget: 100, Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}}}
}

func TestExecutorRunsRatesAndRecordsOutcome(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("AGENT-MARKER be terse"), 0o600)
	k := &fakeKernel{script: []agent.Selection{
		{Action: "sys@k/time"},
		{Action: "reply", Args: map[string]any{"answer": "noon"}},
	}}
	out, err := testExecutor(dir, k).Execute(context.Background(), userJob("t1", "p1", "what time is it?"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Status != agent.StatusAnswered || out.Answer["answer"] != "noon" {
		t.Fatalf("outcome = %+v", out)
	}
	if k.rated["tx-sys@k/time"] != 1 {
		t.Fatalf("expected sys@k/time rated 1, got %v", k.rated)
	}
	// The model saw agent.md in the system prompt.
	if first := k.seen[0][0]; first.Kind != "system" || !strings.Contains(first.Content, "AGENT-MARKER") {
		t.Fatalf("first message = %+v, want the system prompt carrying agent.md", first)
	}
	// The persona file ends with a terminal outcome record.
	data, _ := os.ReadFile(filepath.Join(dir, "p1.jsonl"))
	if !strings.Contains(string(data), `"kind":"outcome"`) {
		t.Fatalf("persona file missing outcome record:\n%s", data)
	}
}

// A second job in a persona continues the first's conversation, with one system
// prompt (its own), and rates only its own calls: the first job's transaction was
// rated once already, and the kernel refuses a second rating (kernel D11).
func TestExecutorContinuesPersonaAndRatesOnlyThisJob(t *testing.T) {
	dir := t.TempDir()
	k1 := &fakeKernel{script: []agent.Selection{{Action: "sys@k/time"}, {Action: "reply", Args: map[string]any{"answer": "noon"}}}}
	if _, err := testExecutor(dir, k1).Execute(context.Background(), userJob("j1", "alice", "what time is it?")); err != nil {
		t.Fatal(err)
	}
	k2 := &fakeKernel{script: []agent.Selection{{Action: "sys@k/web"}, {Action: "reply", Args: map[string]any{"answer": "done"}}}}
	if _, err := testExecutor(dir, k2).Execute(context.Background(), userJob("j2", "alice", "and the weather?")); err != nil {
		t.Fatal(err)
	}
	view := k2.seen[0]
	var systems, sawFirst, sawFirstReply bool
	nsys := 0
	for _, m := range view {
		switch {
		case m.Kind == "system":
			nsys++
		case m.Role == agent.RoleUser && m.Content == "what time is it?":
			sawFirst = true
		case m.Role == agent.RoleTool && m.Action == "reply" && m.Result["answer"] == "noon":
			sawFirstReply = true
		case m.Kind == "outcome":
			t.Fatalf("an earlier job's outcome record leaked into the view: %+v", m)
		}
	}
	systems = nsys == 1 && view[0].Kind == "system"
	if !sawFirst || !sawFirstReply || !systems || view[len(view)-1].Content != "and the weather?" {
		t.Fatalf("second job's view does not continue the first: %+v", view)
	}
	if _, again := k2.rated["tx-sys@k/time"]; again || k2.rated["tx-sys@k/web"] != 1 {
		t.Fatalf("second job rated %v, want only tx-sys@k/web", k2.rated)
	}
	// Another persona starts with nothing but its own prompt.
	k3 := &fakeKernel{}
	testExecutor(dir, k3).Execute(context.Background(), userJob("j3", "bob", "hi"))
	if v := k3.seen[0]; len(v) != 2 || v[1].Content != "hi" {
		t.Fatalf("a new persona's view = %+v, want only its system prompt and request", v)
	}
}

type flakyDecider struct {
	failN int // fail this many times before succeeding
	calls int
}

func (d *flakyDecider) Decide(_ context.Context, _ []agent.Message, _ []agent.Candidate, _ []agent.Tool) (*agent.Selection, error) {
	d.calls++
	if d.calls <= d.failN {
		return nil, errors.New("transient")
	}
	return &agent.Selection{Action: "reply"}, nil
}

func alwaysRetry(error) bool { return true }
func neverRetry(error) bool  { return false }

func TestRetryDeciderSucceedsAfterTransientFailures(t *testing.T) {
	d := &flakyDecider{failN: 2}
	wrapped := retryDecider{inner: d, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Decide(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3 (2 fail + 1 success)", d.calls)
	}
}

func TestRetryDeciderStopsOnNonRetryable(t *testing.T) {
	d := &flakyDecider{failN: 5}
	wrapped := retryDecider{inner: d, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: neverRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Decide(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on non-retryable)", d.calls)
	}
}

type flakyRunner struct {
	failN int
	calls int
	keys  []string
}

func (r *flakyRunner) Run(_ context.Context, _ string, _ map[string]any, _, key string) (agent.RunResult, error) {
	r.calls++
	r.keys = append(r.keys, key)
	if r.calls <= r.failN {
		return agent.RunResult{}, errors.New("network")
	}
	return agent.RunResult{Result: map[string]any{"ok": true}}, nil
}

func TestRetryRunnerRetriesTransportUnderOneKey(t *testing.T) {
	r := &flakyRunner{failN: 1}
	wrapped := retryRunner{inner: r, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Run(context.Background(), "x", nil, "", "key-1"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// Both attempts name the same run, so the kernel answers a repeat of a run
	// that already committed with its first outcome rather than running it again.
	if r.calls != 2 || r.keys[0] != "key-1" || r.keys[1] != "key-1" {
		t.Fatalf("calls = %d keys = %q, want 2 attempts under key-1", r.calls, r.keys)
	}
}

func TestRetryRunnerDoesNotRetryParkedRun(t *testing.T) {
	calls := 0
	parked := &juice.KernelError{Status: 504, Code: "timeout"}
	wrapped := retryRunner{inner: runnerFunc(func() error { calls++; return parked }),
		policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond}, retryOK: juice.IsRetryable, sleep: func(time.Duration) {}}
	if _, err := wrapped.Run(context.Background(), "x", nil, "", "key-1"); err != parked || calls != 1 {
		t.Fatalf("err = %v calls = %d, want the parked timeout returned after one attempt", err, calls)
	}
}

// runnerFunc is a Runner answering every call with f's error.
type runnerFunc func() error

func (f runnerFunc) Run(context.Context, string, map[string]any, string, string) (agent.RunResult, error) {
	return agent.RunResult{}, f()
}

func TestBackoffDoubles(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: 2 * time.Second}
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := p.backoff(attempt); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// bracketKernel opens a subtask on the root's first turn, replies inside the
// bracket, then replies at root — exercising the full frame recursion.
type bracketKernel struct{ i int }

func (k *bracketKernel) Decide(_ context.Context, _ []agent.Message, _ []agent.Candidate, _ []agent.Tool) (*agent.Selection, error) {
	k.i++
	switch k.i {
	case 1:
		return &agent.Selection{Action: "subtask", Args: map[string]any{"instruction": "sub"}}, nil
	case 2:
		return &agent.Selection{Action: "reply", Args: map[string]any{"answer": "sub-done"}}, nil
	default:
		return &agent.Selection{Action: "reply", Args: map[string]any{"answer": "root-done"}}, nil
	}
}
func (k *bracketKernel) Lookup(context.Context, string, int) ([]agent.Candidate, error) {
	return nil, nil
}
func (k *bracketKernel) Read(_ context.Context, ref string) (agent.Candidate, error) {
	return agent.Candidate{Ref: ref}, nil
}
func (k *bracketKernel) Run(context.Context, string, map[string]any, string, string) (agent.RunResult, error) {
	return agent.RunResult{}, nil
}
func (k *bracketKernel) Chat(context.Context, []agent.Message) (string, error) { return "", nil }
func (k *bracketKernel) ChatJSON(context.Context, []agent.Message, map[string]any) (any, error) {
	return nil, nil
}
func (k *bracketKernel) Rate(context.Context, string, int, string) error { return nil }

func bracketExecutor(dir string) *Executor {
	return &Executor{
		Kernel:      &bracketKernel{},
		PersonasDir: dir,
		RetryOK:     func(error) bool { return false },
		Loop: agent.Config{
			ReplyTool: "reply", SubtaskTool: "subtask", ActionLookup: "sys@k/lookup", MaxTurns: 10,
			Tools: []agent.Tool{
				{Name: "reply", InputSchema: map[string]any{"type": "object", "required": []any{"answer"}}},
				{Name: "subtask", InputSchema: map[string]any{"type": "object", "required": []any{"instruction"}}},
			},
		},
	}
}

func TestConcurrentBracketTasksDoNotCrossTalk(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make([]agent.Outcome, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := bracketExecutor(dir) // independent kernel/counters per task
			task := agent.Job{ID: "task-" + strconv.Itoa(i), Persona: "task-" + strconv.Itoa(i), Budget: 100,
				Messages: []agent.Message{{Role: agent.RoleUser, Content: "go"}}}
			out, _ := e.Execute(context.Background(), task)
			results[i] = out
		}(i)
	}
	wg.Wait()
	// Every task answered independently with its own root reply; each transcript
	// belongs to exactly one task.
	for i, out := range results {
		if out.Status != agent.StatusAnswered || out.Answer["answer"] != "root-done" {
			t.Fatalf("task %d outcome = %+v", i, out)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "task-"+strconv.Itoa(i)+".jsonl"))
		// Exactly one *root* user message (the bracket inherits the parent's history,
		// so a bracket-tagged copy of "go" is expected and excluded here). A second
		// root "go" would mean another task's run bled into this file.
		if strings.Count(string(data), `{"role":"user","content":"go"}`) != 1 {
			t.Fatalf("task %d transcript has cross-task content:\n%s", i, data)
		}
	}
}
