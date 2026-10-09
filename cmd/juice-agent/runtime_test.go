package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
)

// captureChannel records deliveries and can inject one task at Start.
type captureChannel struct {
	name      string
	seed      *agent.Job
	mu        sync.Mutex
	delivered map[string]agent.Outcome
}

func (c *captureChannel) Name() string { return c.name }
func (c *captureChannel) Start(_ context.Context, submit func(agent.Job)) error {
	if c.seed != nil {
		submit(*c.seed)
	}
	return nil
}
func (c *captureChannel) Deliver(id string, out agent.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.delivered == nil {
		c.delivered = map[string]agent.Outcome{}
	}
	c.delivered[id] = out
}
func (c *captureChannel) Steering(string) []agent.Message { return nil }
func (c *captureChannel) Stop()                           {}

// stubKernel is the minimal Kernel that always replies immediately.
type stubKernel struct{}

func (stubKernel) Decide(context.Context, []agent.Message, []agent.Candidate, []agent.Tool) (*agent.Selection, error) {
	return &agent.Selection{Action: "reply", Args: map[string]any{"answer": "done"}}, nil
}
func (stubKernel) Lookup(context.Context, string, int) ([]agent.Candidate, error) { return nil, nil }
func (stubKernel) Read(_ context.Context, ref string) (agent.Candidate, error) {
	return agent.Candidate{Ref: ref}, nil
}
func (stubKernel) Run(context.Context, string, map[string]any, string, string) (agent.RunResult, error) {
	return agent.RunResult{}, nil
}
func (stubKernel) Chat(context.Context, []agent.Message) (string, error) { return "", nil }
func (stubKernel) ChatJSON(context.Context, []agent.Message, map[string]any) (any, error) {
	return nil, nil
}
func (stubKernel) Embed(context.Context, string) ([]float64, error) { return nil, nil }
func (stubKernel) Rate(context.Context, string, int, string) error  { return nil }

func newExecutor(dir string) *Executor {
	return &Executor{
		Kernel:  stubKernel{},
		DataDir: dir,
		RetryOK: func(error) bool { return false },
		Loop: agent.Config{
			ReplyTool: "reply", LookupAction: "sys@k/lookup", MaxTurns: 5,
			Intrinsics: map[string]agent.Intrinsic{"reply": {
				Handle: func(_ context.Context, a map[string]any) (map[string]any, bool, error) { return a, true, nil },
			}},
		},
	}
}

func TestRuntimeRoutesOutcomeBackToChannel(t *testing.T) {
	ch := &captureChannel{name: "test", seed: &agent.Job{ID: "j1", Channel: "test", Budget: 10,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}}}}
	rt := NewRuntime(newExecutor(t.TempDir()), []channels.Channel{ch}, 2, func(string, map[string]any) {})

	ctx := context.Background()
	if err := rt.Start(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// wait for delivery
	deadline := time.After(2 * time.Second)
	for {
		ch.mu.Lock()
		_, ok := ch.delivered["j1"]
		ch.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("outcome not delivered within timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
	rt.Shutdown()

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.delivered["j1"].Status != agent.StatusAnswered {
		t.Fatalf("delivered outcome = %+v", ch.delivered["j1"])
	}
}

func TestRuntimeSubmitDropsWhenClosing(t *testing.T) {
	rt := NewRuntime(newExecutor(t.TempDir()), nil, 1, func(string, map[string]any) {})
	rt.Start(context.Background(), 1)
	rt.Shutdown()
	// Submitting after shutdown must not panic (send on closed channel) and must
	// simply drop.
	rt.submit(agent.Job{ID: "late"})
}
