package step

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
)

// fakeStepSource serves canned waiting steps and records completions.
type fakeStepSource struct {
	mu        sync.Mutex
	steps     []channels.StepInfo
	completed map[string]map[string]any
}

func (f *fakeStepSource) WaitingSteps(context.Context) ([]channels.StepInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps, nil
}
func (f *fakeStepSource) CompleteStep(_ context.Context, id string, args map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completed == nil {
		f.completed = map[string]map[string]any{}
	}
	f.completed[id] = args
	kept := f.steps[:0] // the completed step leaves the waiting list (done kernel-side)
	for _, s := range f.steps {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	f.steps = kept
	return nil
}

func TestStepChannelSubmitsAndCompletes(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{
		{ID: "s1", OwnerHandle: "@chat", RequiredCaller: "@agent",
			PartialArgs: map[string]any{"message": "please summarise X"}, CreatedBy: "@chat"},
	}}
	ch := NewStep(src, 5*time.Millisecond, 20, "@agent")

	var got agent.Task
	var wg sync.WaitGroup
	wg.Add(1)
	submitted := false
	var smu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Task) {
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

	if got.Channel != "step" || got.Budget != 20 {
		t.Fatalf("task = %+v", got)
	}
	if got.Messages[0].Content != "please summarise X" {
		t.Fatalf("prompt = %q, want the step message", got.Messages[0].Content)
	}

	// Deliver -> the step is completed with the answer.
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "summary"}})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["answer"] != "summary" {
		t.Fatalf("step s1 completed with %v, want answer=summary", src.completed["s1"])
	}
}

func TestStepChannelDoesNotResubmit(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{{ID: "s1", RequiredCaller: "@agent", PartialArgs: map[string]any{"message": "x"}}}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent")
	var count int
	var mu sync.Mutex
	ch.Start(context.Background(), func(agent.Task) { mu.Lock(); count++; mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // several poll cycles
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("submitted %d times, want 1 (no re-submit across polls)", count)
	}
}

// A step the agent merely owns (addressed to someone else) is not its work.
func TestStepChannelSkipsForeignRequiredCaller(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{
		{ID: "mine", OwnerHandle: "@alice", RequiredCaller: "@agent", PartialArgs: map[string]any{"message": "for me"}},
		{ID: "theirs", OwnerHandle: "@agent", RequiredCaller: "@bob", PartialArgs: map[string]any{"message": "for bob"}},
	}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Task) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond)
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || ids[0] != "step-mine" {
		t.Fatalf("submitted %v, want only [step-mine] (foreign step skipped)", ids)
	}
}

// A step whose sender (owner_handle) is claimed by a dedicated channel is left for
// that channel, even though it is addressed to the agent.
func TestStepChannelExcludesClaimedSource(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{
		{ID: "generic", OwnerHandle: "@alice", RequiredCaller: "@agent", PartialArgs: map[string]any{"message": "hi"}},
		{ID: "chat", OwnerHandle: "@chat", RequiredCaller: "@agent", PartialArgs: map[string]any{"room": "r1"}},
	}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent", "@chat")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Task) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond)
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || ids[0] != "step-generic" {
		t.Fatalf("submitted %v, want only [step-generic] (@chat step left for the chat channel)", ids)
	}
}

func TestSessionKey(t *testing.T) {
	base := channels.StepInfo{OwnerHandle: "@sms", PartialArgs: map[string]any{"message": "hi"}}
	if got := sessionKey(base); got != "@sms" {
		t.Fatalf("no-thread session = %q, want @sms", got)
	}
	threaded := channels.StepInfo{OwnerHandle: "@sms", PartialArgs: map[string]any{"thread": "+1555", "message": "hi"}}
	if got := sessionKey(threaded); got != "@sms\x1f+1555" {
		t.Fatalf("threaded session = %q", got)
	}
	// distinct threads from one sender are distinct sessions
	if sessionKey(base) == sessionKey(threaded) {
		t.Fatalf("threaded and unthreaded sessions must differ")
	}
}

// Two steps in one conversation run one at a time; the second is picked up only
// after the first is delivered.
func TestStepChannelSerializesPerSession(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{
		{ID: "a", OwnerHandle: "@sms", RequiredCaller: "@agent", PartialArgs: map[string]any{"thread": "t1", "message": "1"}},
		{ID: "b", OwnerHandle: "@sms", RequiredCaller: "@agent", PartialArgs: map[string]any{"thread": "t1", "message": "2"}},
	}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent")
	var ids []string
	var mu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Task) { mu.Lock(); ids = append(ids, tk.ID); mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // several polls: only the first should be active
	mu.Lock()
	n1, first := len(ids), ""
	if n1 > 0 {
		first = ids[0]
	}
	mu.Unlock()
	if n1 != 1 || first != "step-a" {
		t.Fatalf("one session should have one active task; got %v", ids)
	}
	ch.Deliver(first, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "ok"}})
	time.Sleep(20 * time.Millisecond) // session freed: the second is now picked up
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[1] != "step-b" {
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

func TestStepChannelDeliverShapesToDeliveryAction(t *testing.T) {
	// A step whose onward action declares a "text" field: the agent's answer must be
	// delivered as {text: ...} so the delivery action (e.g. a bridge) sends it.
	src := &fakeStepSource{steps: []channels.StepInfo{{
		ID: "s1", OwnerHandle: "@smsbridge", RequiredCaller: "@agent",
		ActionRef:    "@smsbridge/deliver",
		PartialArgs:  map[string]any{"to": "+1555", "incoming": "hi"},
		AllowedInput: map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent")
	var got agent.Task
	var wg sync.WaitGroup
	wg.Add(1)
	var once sync.Once
	ch.Start(context.Background(), func(tk agent.Task) { once.Do(func() { got = tk; wg.Done() }) })
	wg.Wait()
	ch.Stop()
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "bonjour"}})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["text"] != "bonjour" {
		t.Fatalf("completed with %v, want text=bonjour (shaped to the delivery action)", src.completed["s1"])
	}
}

func TestStepChannelDeliverFailedStillCompletes(t *testing.T) {
	src := &fakeStepSource{steps: []channels.StepInfo{{ID: "s1", RequiredCaller: "@agent", PartialArgs: map[string]any{"message": "x"}}}}
	ch := NewStep(src, 2*time.Millisecond, 20, "@agent")
	var got agent.Task
	var wg sync.WaitGroup
	wg.Add(1)
	var once sync.Once
	ch.Start(context.Background(), func(tk agent.Task) { once.Do(func() { got = tk; wg.Done() }) })
	wg.Wait()
	ch.Stop()
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusFailed, Reason: "turn-cap"})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"] == nil || src.completed["s1"]["status"] != "failed" {
		t.Fatalf("failed task must still complete (ack) the step, got %v", src.completed["s1"])
	}
	if src.completed["s1"]["reason"] != "turn-cap" {
		t.Fatalf("ack should carry the failure reason, got %v", src.completed["s1"])
	}
}
