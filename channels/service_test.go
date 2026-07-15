package channels

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// fakeService is an in-memory ServiceActions: notification steps to hand out,
// per-conversation message logs, and records of what was posted and completed. It
// keys a conversation by the step's partial_args["room"] — a stand-in for any
// service's session grammar, so these tests double as the generalization check
// (the driver is exercised with an adapter that is not chat).
type fakeService struct {
	mu        sync.Mutex
	steps     []StepInfo
	rooms     map[string][]Msg
	posted    map[string][]string
	completed map[string]bool

	readErr   error // returned by Read when set
	postErr   error // returned by Post when set
	transient bool  // classification the driver sees for readErr/postErr
}

func newFakeService() *fakeService {
	return &fakeService{rooms: map[string][]Msg{}, posted: map[string][]string{}, completed: map[string]bool{}}
}

func (f *fakeService) Pending(context.Context) ([]StepInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StepInfo, len(f.steps))
	copy(out, f.steps)
	return out, nil
}

func (f *fakeService) SessionKey(s StepInfo) string {
	room, _ := s.PartialArgs["room"].(string)
	return room
}

func (f *fakeService) Read(_ context.Context, room string, after int64) ([]Msg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	var out []Msg
	for _, m := range f.rooms[room] {
		if m.Seq > after {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeService) Post(_ context.Context, room, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	f.posted[room] = append(f.posted[room], text)
	return nil
}

func (f *fakeService) Complete(_ context.Context, stepID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed[stepID] = true
	kept := f.steps[:0]
	for _, s := range f.steps {
		if s.ID != stepID {
			kept = append(kept, s)
		}
	}
	f.steps = kept
	return nil
}

func (f *fakeService) Transient(error) bool { return f.transient }

func step(id, room string) StepInfo {
	return StepInfo{ID: id, OwnerHandle: "@svc", RequiredCaller: "@agent",
		PartialArgs: map[string]any{"room": room}}
}

// newSvc wires a Service to fs with a capturing submit; returns the channel and a
// pointer to the captured tasks.
func newSvc(t *testing.T, fs *fakeService, loop LoopConfig, now func() time.Time) (*Service, *[]agent.Task) {
	t.Helper()
	s := NewService(fs, ServiceConfig{Name: "svc", Handle: "@agent", Interval: time.Hour,
		Budget: 20, DataDir: t.TempDir(), Loop: loop, Now: now})
	var tasks []agent.Task
	var mu sync.Mutex
	s.submit = func(tk agent.Task) { mu.Lock(); tasks = append(tasks, tk); mu.Unlock() }
	return s, &tasks
}

func answered(text string) agent.Outcome {
	return agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": text}}
}

func TestServiceReadsSubmitsAndDelivers(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "what time is it?"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)

	s.scan(context.Background())
	if len(*tasks) != 1 || (*tasks)[0].Channel != "svc" {
		t.Fatalf("tasks = %+v, want 1 svc task", *tasks)
	}
	if got := (*tasks)[0].Messages[0].Content; got != "@alice: what time is it?" {
		t.Fatalf("prompt = %q", got)
	}
	// The cursor must NOT advance until the reply is delivered (at-least-once).
	if got := s.cursors.get("r1"); got != 0 {
		t.Fatalf("cursor advanced before delivery: %d", got)
	}
	s.Deliver((*tasks)[0].ID, answered("12:00"))
	if len(fs.posted["r1"]) != 1 || fs.posted["r1"][0] != "12:00" {
		t.Fatalf("posted = %v, want [12:00]", fs.posted["r1"])
	}
	if !fs.completed["d1"] {
		t.Fatal("doorbell d1 must be completed after delivery")
	}
	if got := s.cursors.get("r1"); got != 1 {
		t.Fatalf("cursor = %d, want 1 after delivery", got)
	}
}

func TestServiceSkipsOwnMessages(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@agent", Text: "my own post"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)

	s.scan(context.Background())
	if len(*tasks) != 0 {
		t.Fatalf("submitted %d tasks, want 0 (only the agent's own message)", len(*tasks))
	}
	if !fs.completed["d1"] {
		t.Fatal("a step with nothing to answer must still be completed")
	}
	if got := s.cursors.get("r1"); got != 1 {
		t.Fatalf("cursor = %d, want 1 (advanced past own message)", got)
	}
}

func TestServiceEmptyReadCompletesStep(t *testing.T) {
	fs := newFakeService()
	fs.steps = []StepInfo{step("d1", "r1")} // no messages in r1
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	if len(*tasks) != 0 || !fs.completed["d1"] {
		t.Fatalf("empty read: want 0 tasks and d1 completed; tasks=%d completed=%v", len(*tasks), fs.completed["d1"])
	}
}

func TestServiceMalformedStepCompleted(t *testing.T) {
	fs := newFakeService()
	fs.steps = []StepInfo{{ID: "d1", OwnerHandle: "@svc", PartialArgs: map[string]any{}}} // no room
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	if len(*tasks) != 0 || !fs.completed["d1"] {
		t.Fatalf("malformed step must be completed with no task; tasks=%d completed=%v", len(*tasks), fs.completed["d1"])
	}
}

func TestServiceReadTransientLeavesStep(t *testing.T) {
	fs := newFakeService()
	fs.steps = []StepInfo{step("d1", "r1")}
	fs.readErr = errors.New("boom")
	fs.transient = true
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	if len(*tasks) != 0 {
		t.Fatalf("transient read error must submit nothing, got %d", len(*tasks))
	}
	if fs.completed["d1"] {
		t.Fatal("transient read error must leave the step waiting for retry")
	}
}

func TestServiceReadPermanentCompletesStep(t *testing.T) {
	fs := newFakeService()
	fs.steps = []StepInfo{step("d1", "r1")}
	fs.readErr = errors.New("room gone")
	fs.transient = false
	s, _ := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	if !fs.completed["d1"] {
		t.Fatal("permanent read error must complete (give up on) the step")
	}
}

func TestServicePostTransientLeavesStepAndCursor(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "x"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	fs.postErr = errors.New("post down")
	fs.transient = true
	s.Deliver((*tasks)[0].ID, answered("hi"))
	if fs.completed["d1"] {
		t.Fatal("transient post failure must leave the step for retry")
	}
	if got := s.cursors.get("r1"); got != 0 {
		t.Fatalf("cursor advanced despite failed post: %d", got)
	}
	// Recovery: post works on the next attempt and the message is answered.
	fs.postErr = nil
	fs.steps = []StepInfo{step("d1", "r1")}
	s.scan(context.Background())
	s.Deliver((*tasks)[len(*tasks)-1].ID, answered("hi"))
	if len(fs.posted["r1"]) != 1 || !fs.completed["d1"] {
		t.Fatalf("after recovery want 1 post and completed step; posted=%v completed=%v", fs.posted["r1"], fs.completed["d1"])
	}
}

func TestServicePostPermanentGivesUp(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "x"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	fs.postErr = errors.New("forbidden")
	fs.transient = false
	s.Deliver((*tasks)[0].ID, answered("hi"))
	if !fs.completed["d1"] {
		t.Fatal("permanent post failure must complete the step (give up)")
	}
	if got := s.cursors.get("r1"); got != 1 {
		t.Fatalf("cursor = %d, want 1 (advanced past the abandoned message)", got)
	}
}

func TestServiceFailedTaskRepliesWithFallback(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "x"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	s.Deliver((*tasks)[0].ID, agent.Outcome{Status: agent.StatusFailed, Reason: "turn-cap"})
	if len(fs.posted["r1"]) != 1 || fs.posted["r1"][0] != failureReply {
		t.Fatalf("failed task should post the fallback, got %v", fs.posted["r1"])
	}
	if !fs.completed["d1"] {
		t.Fatal("a failed task must still complete the step")
	}
}

func TestServiceEmptyAnswerRepliesWithFallback(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "x"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)
	s.scan(context.Background())
	s.Deliver((*tasks)[0].ID, answered("   "))
	if len(fs.posted["r1"]) != 1 || fs.posted["r1"][0] != failureReply {
		t.Fatalf("empty answer should post the fallback, got %v", fs.posted["r1"])
	}
}

func TestServiceOneActiveTaskPerConversation(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@alice", Text: "one"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s, tasks := newSvc(t, fs, LoopConfig{}, nil)

	s.scan(context.Background()) // submits, marks r1 active
	fs.rooms["r1"] = append(fs.rooms["r1"], Msg{Seq: 2, From: "@alice", Text: "two"})
	fs.steps = append(fs.steps, step("d2", "r1"))
	s.scan(context.Background()) // r1 busy -> nothing new
	if len(*tasks) != 1 {
		t.Fatalf("submitted %d, want 1 while a task is active for r1", len(*tasks))
	}
	s.Deliver((*tasks)[0].ID, answered("ok"))
	s.scan(context.Background()) // r1 free -> the newer step runs
	if len(*tasks) != 2 {
		t.Fatalf("after delivery, want 2 tasks, got %d", len(*tasks))
	}
}

func TestServiceCursorPersistsAcrossReopen(t *testing.T) {
	fs := newFakeService()
	fs.rooms["r1"] = []Msg{{Seq: 5, From: "@alice", Text: "hi"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	dir := t.TempDir()
	s := NewService(fs, ServiceConfig{Name: "svc", Handle: "@agent", Interval: time.Hour, Budget: 20, DataDir: dir})
	var tasks []agent.Task
	s.submit = func(tk agent.Task) { tasks = append(tasks, tk) }
	s.scan(context.Background())
	s.Deliver(tasks[0].ID, answered("hey"))
	if got := s.cursors.get("r1"); got != 5 {
		t.Fatalf("cursor = %d, want 5", got)
	}
	// A fresh cursor store over the same file sees the persisted value.
	reopened := newCursorStore(dir + "/svc-cursors.json")
	if got := reopened.get("r1"); got != 5 {
		t.Fatalf("persisted cursor = %d, want 5", got)
	}
}

func TestServiceLoopGuardSuppresses(t *testing.T) {
	fs := newFakeService()
	clk := newClock()
	// Budget of 1 reply per window to @flooder.
	s, tasks := newSvc(t, fs, LoopConfig{Enabled: true, Max: 1, Window: time.Minute, Cooldown: time.Minute}, clk.now)

	// First message from @flooder is answered.
	fs.rooms["r1"] = []Msg{{Seq: 1, From: "@flooder", Text: "a"}}
	fs.steps = []StepInfo{step("d1", "r1")}
	s.scan(context.Background())
	if len(*tasks) != 1 {
		t.Fatalf("first message must be answered, tasks=%d", len(*tasks))
	}
	s.Deliver((*tasks)[0].ID, answered("ok"))

	// A second message from @flooder within the window is suppressed: no task, but
	// the step is completed (unread cleared) and the cursor advances.
	fs.rooms["r1"] = append(fs.rooms["r1"], Msg{Seq: 2, From: "@flooder", Text: "b"})
	fs.steps = []StepInfo{step("d2", "r1")}
	s.scan(context.Background())
	if len(*tasks) != 1 {
		t.Fatalf("second message must be suppressed, tasks=%d", len(*tasks))
	}
	if !fs.completed["d2"] {
		t.Fatal("a suppressed step must still be completed")
	}
	if got := s.cursors.get("r1"); got != 2 {
		t.Fatalf("cursor = %d, want 2 after suppression", got)
	}
}
