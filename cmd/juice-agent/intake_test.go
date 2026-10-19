package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

func task(id, requester, owner, createdBy string, args map[string]any) juice.Task {
	return juice.Task{ID: id, Requester: requester, Owner: owner, CreatedBy: createdBy,
		RequiredCaller: "agent@k", PartialArgs: args, Status: "waiting"}
}

// The persona key, for the running example of requirements §10.
func TestPersonaKeyCases(t *testing.T) {
	direct := task("1", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "hi"})
	team := task("2", "rooms@k", "alice@k", "rooms@k/post", map[string]any{"thread_id": "team"})
	carol := task("3", "rooms@k", "carol@k", "rooms@k/post", map[string]any{"thread_id": "team"})
	secret := task("4", "rooms@k", "alice@k", "rooms@k/post", map[string]any{"thread_id": "secret"})
	bare1 := task("5", "rooms@k", "alice@k", "rooms@k/post", nil)
	bare2 := task("6", "rooms@k", "alice@k", "rooms@k/post", nil)
	again := task("7", "alice@k", "alice@k", "sys@k/message", nil)

	if got := personaKey(direct); got != "alice%40k|sys%40k%2Fmessage|" {
		t.Fatalf("direct key = %q", got)
	}
	if got := personaKey(team); got != "rooms%40k|rooms%40k%2Fpost|t%3Ateam" {
		t.Fatalf("threaded key = %q", got)
	}
	if personaKey(direct) != personaKey(again) {
		t.Fatal("a direct sender's messages must share one persona")
	}
	if personaKey(team) != personaKey(carol) {
		t.Fatal("one thread is one persona whoever posts in it")
	}
	if personaKey(team) == personaKey(secret) {
		t.Fatal("two threads of one application must never share a persona")
	}
	if personaKey(bare1) == personaKey(bare2) || personaKey(bare1) == personaKey(team) {
		t.Fatal("a relayed task without thread_id must be its own persona")
	}
	// A thread named by another application never reaches this one's persona.
	other := task("8", "bob@k", "alice@k", "bob@k/post", map[string]any{"thread_id": "secret"})
	if personaKey(other) == personaKey(secret) {
		t.Fatal("thread ids are namespaced by requester and creating action")
	}
}

// Components are escaped before they are joined, so a separator inside a
// component cannot make two keys collide.
func TestPersonaKeyIsUnambiguous(t *testing.T) {
	a := task("1", "r@k", "o@k", "c@k/x|t:a", map[string]any{"thread_id": "b"})
	b := task("2", "r@k", "o@k", "c@k/x", map[string]any{"thread_id": "a|t:b"})
	if personaKey(a) == personaKey(b) {
		t.Fatalf("distinct keys collide: %q", personaKey(a))
	}
}

func TestPromptFromTaskNamesTheSender(t *testing.T) {
	for _, tc := range []struct {
		t    juice.Task
		want string
	}{
		{task("1", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "hello"}), "alice@k: hello"},
		{task("2", "rooms@k", "alice@k", "rooms@k/post", map[string]any{"message": "hi", "thread_id": "team"}), "alice@k via rooms@k: hi"},
		{task("3", "a@k", "a@k", "a@k/x", map[string]any{"request": "do it"}), "a@k: do it"},
		{task("4", "a@k", "a@k", "a@k/x", map[string]any{"n": 2}), `a@k: {"n":2}`},
	} {
		if got := promptFromTask(tc.t); got != tc.want {
			t.Errorf("prompt = %q, want %q", got, tc.want)
		}
	}
}

func TestShapeReply(t *testing.T) {
	ans := map[string]any{"answer": "hi"}
	str := map[string]any{"type": "string"}
	for _, tc := range []struct {
		allowed map[string]any
		key     string
	}{
		{nil, "answer"}, // a sink: the reply rides in the completion's args
		{map[string]any{"properties": map[string]any{"text": str, "to": str}}, "text"},
		{map[string]any{"properties": map[string]any{"body_html": str}}, "body_html"},
	} {
		if got := shapeReply(ans, tc.allowed); got[tc.key] != "hi" || len(got) != 1 {
			t.Errorf("shapeReply(%v) = %v, want {%s: hi}", tc.allowed, got, tc.key)
		}
	}
	two := map[string]any{"properties": map[string]any{"a": str, "b": str}}
	if got := shapeReply(ans, two); got["answer"] != "hi" {
		t.Errorf("ambiguous schema: %v, want the reply passed through", got)
	}
}

// fakeSource is a kernel task list whose completions fail transiently a scripted
// number of times per task.
type fakeSource struct {
	mu        sync.Mutex
	tasks     []juice.Task
	completed map[string]map[string]any
	failures  map[string]int
}

func (f *fakeSource) WaitingTasks(context.Context) ([]juice.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []juice.Task
	for _, t := range f.tasks {
		if _, done := f.completed[t.ID]; !done {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeSource) CompleteTask(_ context.Context, id string, args map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures[id] > 0 {
		f.failures[id]--
		return &juice.KernelError{Status: 503, Message: "busy"}
	}
	f.completed[id] = args
	return nil
}

func (f *fakeSource) done(id string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.completed[id]
	return a, ok
}

// gatedRun blocks each job until released, recording which jobs started.
type gatedRun struct {
	mu      sync.Mutex
	started []agent.Job
	release chan struct{}
}

func (g *gatedRun) run(_ context.Context, j agent.Job) (agent.Outcome, error) {
	g.mu.Lock()
	g.started = append(g.started, j)
	g.mu.Unlock()
	<-g.release
	return agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "re: " + j.Messages[0].Content}}, nil
}

func (g *gatedRun) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.started)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIntakeRunsOneJobPerPersonaAndCompletes(t *testing.T) {
	src := &fakeSource{completed: map[string]map[string]any{}, tasks: []juice.Task{
		task("a1", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "first"}),
		task("a2", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "second"}),
		task("b1", "bob@k", "bob@k", "sys@k/message", map[string]any{"message": "bob here"}),
	}}
	other := task("x1", "alice@k", "alice@k", "sys@k/message", nil)
	other.RequiredCaller = "someone@k" // the agent funded it but must not complete it
	src.tasks = append(src.tasks, other)

	g := &gatedRun{release: make(chan struct{})}
	in := NewIntake(src, g.run, "agent@k", 50, 4, time.Hour, nil)
	ctx := context.Background()
	in.Start(ctx)

	in.scan(ctx)
	waitFor(t, "two jobs to start", func() bool { return g.count() == 2 })
	in.scan(ctx) // alice's second message waits: her persona is busy
	time.Sleep(10 * time.Millisecond)
	if g.count() != 2 {
		t.Fatalf("started %d jobs, want 2: one per persona", g.count())
	}
	for _, j := range g.started {
		if j.Budget != 50 || j.Persona != personaKey(src.tasks[0]) && j.Persona != personaKey(src.tasks[2]) {
			t.Fatalf("job %+v: wrong budget or persona", j)
		}
	}
	g.release <- struct{}{}
	g.release <- struct{}{}
	waitFor(t, "a1 and b1 completed", func() bool { _, a := src.done("a1"); _, b := src.done("b1"); return a && b })

	in.scan(ctx) // alice's persona is free again: her second message runs now
	waitFor(t, "a2 to start", func() bool { return g.count() == 3 })
	if g.started[2].Messages[0].Content != "alice@k: second" {
		t.Fatalf("third job = %+v, want alice's second message", g.started[2])
	}
	close(g.release)
	in.Shutdown()

	if args, _ := src.done("a1"); args["answer"] != "re: alice@k: first" {
		t.Fatalf("a1 completed with %v", args)
	}
	if _, ok := src.done("a2"); !ok {
		t.Fatal("shutdown must drain: a2 not completed")
	}
	if _, ok := src.done("x1"); ok || g.count() != 3 {
		t.Fatal("a task whose required caller is another must be left alone")
	}
}

func TestIntakeResendsTransientCompletionWithoutRerunning(t *testing.T) {
	src := &fakeSource{completed: map[string]map[string]any{},
		tasks:    []juice.Task{task("t1", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "hi"})},
		failures: map[string]int{"t1": 1}}
	g := &gatedRun{release: make(chan struct{})}
	close(g.release)
	in := NewIntake(src, g.run, "agent@k", 10, 1, time.Hour, nil)
	ctx := context.Background()
	in.Start(ctx)
	in.scan(ctx)
	waitFor(t, "the completion to be owed", func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.unsent) == 1
	})
	in.scan(ctx) // resends the owed completion; the job is not run again
	in.Shutdown()
	if _, ok := src.done("t1"); !ok || g.count() != 1 {
		t.Fatalf("completed=%v runs=%d, want the completion resent and one run", ok, g.count())
	}
}

// A persona stays busy while its completion is owed, so its next task can never
// complete ahead of the earlier one.
func TestIntakeHoldsPersonaUntilCompleted(t *testing.T) {
	src := &fakeSource{completed: map[string]map[string]any{}, failures: map[string]int{"a1": 2},
		tasks: []juice.Task{
			task("a1", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "first"}),
			task("a2", "alice@k", "alice@k", "sys@k/message", map[string]any{"message": "second"}),
		}}
	g := &gatedRun{release: make(chan struct{})}
	close(g.release)
	in := NewIntake(src, g.run, "agent@k", 10, 2, time.Hour, nil)
	ctx := context.Background()
	in.Start(ctx)
	owedA1 := func() bool { in.mu.Lock(); defer in.mu.Unlock(); return len(in.unsent) == 1 }

	in.scan(ctx) // a1 runs; its completion fails
	waitFor(t, "a1's completion to be owed", owedA1)
	in.scan(ctx) // the resend fails again: a2 must wait
	time.Sleep(10 * time.Millisecond)
	if g.count() != 1 || !owedA1() {
		t.Fatalf("runs=%d: a2 started while a1's completion was owed", g.count())
	}
	in.scan(ctx) // the resend succeeds, freeing the persona for a2
	waitFor(t, "a2 to complete", func() bool { _, ok := src.done("a2"); return ok })
	in.Shutdown()
	if _, ok := src.done("a1"); !ok || g.count() != 2 {
		t.Fatalf("a1 completed=%v runs=%d, want a1 completed and two runs", ok, g.count())
	}
}

func TestIntakeFailedJobStillCompletes(t *testing.T) {
	src := &fakeSource{completed: map[string]map[string]any{},
		tasks: []juice.Task{task("t1", "alice@k", "alice@k", "sys@k/message", nil)}}
	fail := func(context.Context, agent.Job) (agent.Outcome, error) {
		return agent.Outcome{Status: agent.StatusFailed, Reason: "turn-cap"}, nil
	}
	in := NewIntake(src, fail, "agent@k", 10, 1, time.Hour, nil)
	in.Start(context.Background())
	in.scan(context.Background())
	in.Shutdown()
	if args, ok := src.done("t1"); !ok || args["status"] != "failed" || args["reason"] != "turn-cap" {
		t.Fatalf("completion = %v, %v; want the failure acknowledged", args, ok)
	}
}
