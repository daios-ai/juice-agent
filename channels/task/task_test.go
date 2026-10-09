package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
)

// errTransient is the failure the fake classifies as worth retrying.
var errTransient = errors.New("connection reset")

// fakeTaskSource serves canned waiting tasks and records completions. fail queues
// errors the next completion attempts return instead of completing.
type fakeTaskSource struct {
	mu        sync.Mutex
	tasks     []channels.TaskInfo
	completed map[string]map[string]any
	fail      []error
	attempts  int
}

func (f *fakeTaskSource) WaitingTasks(context.Context) ([]channels.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks, nil
}
func (f *fakeTaskSource) CompleteTask(_ context.Context, id string, args map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if len(f.fail) > 0 {
		err := f.fail[0]
		f.fail = f.fail[1:]
		return err
	}
	if f.completed == nil {
		f.completed = map[string]map[string]any{}
	}
	f.completed[id] = args
	kept := f.tasks[:0] // the completed task leaves the waiting list (done kernel-side)
	for _, s := range f.tasks {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	f.tasks = kept
	return nil
}
func (f *fakeTaskSource) Transient(err error) bool { return errors.Is(err, errTransient) }

func TestTaskChannelSubmitsAndCompletes(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{
		{ID: "s1", Owner: "chat@k", RequiredCaller: "agent@k",
			PartialArgs: map[string]any{"message": "please summarise X"}, CreatedBy: "chat@k"},
	}}
	ch := New(src, 5*time.Millisecond, 20, "agent@k")

	var got agent.Job
	var wg sync.WaitGroup
	wg.Add(1)
	submitted := false
	var smu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Job) {
		smu.Lock()
		defer smu.Unlock()
		if submitted {
			return
		}
		submitted = true
		got = tk
		wg.Done()
	})
	wg.Wait()
	ch.Stop()

	if got.Channel != "task" || got.Budget != 20 {
		t.Fatalf("task = %+v", got)
	}
	if got.Messages[0].Content != "please summarise X" {
		t.Fatalf("prompt = %q, want the task message", got.Messages[0].Content)
	}

	// Deliver -> the task is completed with the answer.
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "summary"}})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["answer"] != "summary" {
		t.Fatalf("task s1 completed with %v, want answer=summary", src.completed["s1"])
	}
}

func TestTaskChannelDoesNotResubmit(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{{ID: "s1", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "x"}}}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k")
	var count int
	var mu sync.Mutex
	ch.Start(context.Background(), func(agent.Job) { mu.Lock(); count++; mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // several poll cycles
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("submitted %d times, want 1 (no re-submit across polls)", count)
	}
}

// A task the agent merely owns (addressed to someone else) is not its work.
func TestTaskChannelSkipsForeignRequiredCaller(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{
		{ID: "mine", Owner: "alice@k", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "for me"}},
		{ID: "theirs", Owner: "agent@k", RequiredCaller: "bob@k", PartialArgs: map[string]any{"message": "for bob"}},
	}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Job) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond)
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || ids[0] != "task-mine" {
		t.Fatalf("submitted %v, want only [task-mine] (foreign task skipped)", ids)
	}
}

// A task whose sender (owner) is claimed by a dedicated channel is left for
// that channel, even though it is addressed to the agent.
func TestTaskChannelExcludesClaimedSource(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{
		{ID: "generic", Owner: "alice@k", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "hi"}},
		{ID: "chat@k", Owner: "chat@k", RequiredCaller: "agent@k", PartialArgs: map[string]any{"room": "r1"}},
	}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k", "chat@k")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Job) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond)
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || ids[0] != "task-generic" {
		t.Fatalf("submitted %v, want only [task-generic] (chat task left for the chat channel)", ids)
	}
}

func TestSessionKey(t *testing.T) {
	base := channels.TaskInfo{Owner: "sms", PartialArgs: map[string]any{"message": "hi"}}
	if got := sessionKey(base); got != "sms" {
		t.Fatalf("no-thread session = %q, want sms", got)
	}
	threaded := channels.TaskInfo{Owner: "sms", PartialArgs: map[string]any{"thread": "+1555", "message": "hi"}}
	if got := sessionKey(threaded); got != "sms\x1f+1555" {
		t.Fatalf("threaded session = %q", got)
	}
	// distinct threads from one sender are distinct sessions
	if sessionKey(base) == sessionKey(threaded) {
		t.Fatalf("threaded and unthreaded sessions must differ")
	}
}

// Two tasks in one conversation run one at a time; the second is picked up only
// after the first is delivered.
func TestTaskChannelSerializesPerSession(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{
		{ID: "a", Owner: "sms", RequiredCaller: "agent@k", PartialArgs: map[string]any{"thread": "t1", "message": "1"}},
		{ID: "b", Owner: "sms", RequiredCaller: "agent@k", PartialArgs: map[string]any{"thread": "t1", "message": "2"}},
	}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Job) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // several polls: only the first should be active
	mu.Lock()
	n1, first := len(ids), ""
	if n1 > 0 {
		first = ids[0]
	}
	mu.Unlock()
	if n1 != 1 || first != "task-a" {
		t.Fatalf("one session should have one active task; got %v", ids)
	}
	ch.Deliver(first, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "ok"}})
	time.Sleep(20 * time.Millisecond) // session freed: the second is now picked up
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[1] != "task-b" {
		t.Fatalf("after delivering the first, the second should run; got %v", ids)
	}
}

func TestShapeReply(t *testing.T) {
	answer := map[string]any{"answer": "hello"}
	strProp := map[string]any{"type": "string"}
	cases := []struct {
		name    string
		allowed map[string]any
		wantKey string
	}{
		{"sink/unconstrained", nil, "answer"},
		{"delivery action with text", map[string]any{"properties": map[string]any{"text": strProp}}, "text"},
		{"delivery action with message", map[string]any{"properties": map[string]any{"message": strProp}}, "message"},
		{"sole string prop", map[string]any{"properties": map[string]any{"blurb": strProp}}, "blurb"},
	}
	for _, c := range cases {
		got := shapeReply(answer, c.allowed)
		if got[c.wantKey] != "hello" {
			t.Fatalf("%s: shapeReply = %v, want %s=hello", c.name, got, c.wantKey)
		}
	}
	// pre-bound routing + a single reply field: the answer lands under the field,
	// the routing is supplied by partial_args at completion (not here).
	deliver := map[string]any{"properties": map[string]any{"text": strProp}}
	if shapeReply(answer, deliver)["text"] != "hello" {
		t.Fatal("delivery-action shaping failed")
	}
}

func TestTaskChannelDeliverShapesToDeliveryAction(t *testing.T) {
	// A task whose onward action declares a "text" field: the agent's answer must be
	// delivered as {text: ...} so the delivery action (e.g. a bridge) sends it.
	src := &fakeTaskSource{tasks: []channels.TaskInfo{{
		ID: "s1", Owner: "smsbridge", RequiredCaller: "agent@k",
		ActionRef:    "smsbridge/deliver",
		PartialArgs:  map[string]any{"to": "+1555", "incoming": "hi"},
		AllowedInput: map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k")
	var got agent.Job
	var wg sync.WaitGroup
	wg.Add(1)
	var once sync.Once
	ch.Start(context.Background(), func(tk agent.Job) { once.Do(func() { got = tk; wg.Done() }) })
	wg.Wait()
	ch.Stop()
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "bonjour"}})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["text"] != "bonjour" {
		t.Fatalf("completed with %v, want text=bonjour (shaped to the delivery action)", src.completed["s1"])
	}
}

func TestTaskChannelDeliverFailedStillCompletes(t *testing.T) {
	src := &fakeTaskSource{tasks: []channels.TaskInfo{{ID: "s1", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "x"}}}}
	ch := New(src, 2*time.Millisecond, 20, "agent@k")
	var got agent.Job
	var wg sync.WaitGroup
	wg.Add(1)
	var once sync.Once
	ch.Start(context.Background(), func(tk agent.Job) { once.Do(func() { got = tk; wg.Done() }) })
	wg.Wait()
	ch.Stop()
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusFailed, Reason: "turn-cap"})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"] == nil || src.completed["s1"]["status"] != "failed" {
		t.Fatalf("failed task must still complete (ack) the task, got %v", src.completed["s1"])
	}
	if src.completed["s1"]["reason"] != "turn-cap" {
		t.Fatalf("ack should carry the failure reason, got %v", src.completed["s1"])
	}
}

// runOnce starts ch, delivers the first job it submits with answer, lets several
// polls pass, and reports how many jobs were submitted.
func runOnce(t *testing.T, ch *Channel, answer string) int {
	t.Helper()
	var mu sync.Mutex
	var jobs []agent.Job
	ch.Start(context.Background(), func(j agent.Job) { mu.Lock(); jobs = append(jobs, j); mu.Unlock() })
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		mu.Lock()
		n := len(jobs)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no job submitted")
		}
	}
	ch.Deliver(jobs[0].ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": answer}})
	time.Sleep(30 * time.Millisecond) // several polls
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	return len(jobs)
}

func TestTaskChannelResendsCompletionAfterTransientFailure(t *testing.T) {
	src := &fakeTaskSource{fail: []error{errTransient, errTransient},
		tasks: []channels.TaskInfo{{ID: "s1", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "x"}}}}
	jobs := runOnce(t, New(src, 2*time.Millisecond, 20, "agent@k"), "done")
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["answer"] != "done" || src.attempts != 3 || jobs != 1 {
		t.Fatalf("completed=%v attempts=%d jobs=%d, want the same answer delivered on the third attempt by one job",
			src.completed, src.attempts, jobs)
	}
}

func TestTaskChannelDropsPermanentlyRefusedCompletion(t *testing.T) {
	src := &fakeTaskSource{fail: []error{errors.New("schema violation")},
		tasks: []channels.TaskInfo{{ID: "s1", RequiredCaller: "agent@k", PartialArgs: map[string]any{"message": "x"}}}}
	jobs := runOnce(t, New(src, 2*time.Millisecond, 20, "agent@k"), "done")
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.attempts != 1 || src.completed != nil || jobs != 1 {
		t.Fatalf("attempts=%d completed=%v jobs=%d, want one refused attempt and no rerun", src.attempts, src.completed, jobs)
	}
}
