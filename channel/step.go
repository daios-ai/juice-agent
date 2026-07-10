package channel

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// StepInfo is a waiting step addressed to the agent, projected to what the
// channel needs. cmd/juice-agent adapts juice.Step into this, keeping the channel
// package independent of the kernel client.
type StepInfo struct {
	ID          string
	PartialArgs map[string]any
	CreatedBy   string
}

// StepSource lists and completes the agent's waiting steps. Implemented over the
// juice client (via an adapter); faked in tests.
type StepSource interface {
	WaitingSteps(ctx context.Context) ([]StepInfo, error)
	CompleteStep(ctx context.Context, id string, args map[string]any) error
}

// Step is the kernel-native intake surface: it polls the kernel for waiting
// steps addressed to the agent, turns each into a task, and delivers the outcome
// by completing the step. This is how any other agent (e.g. @chat) or a human,
// via @sys/message, hands work to the agent with zero pairwise integration.
type Step struct {
	src      StepSource
	interval time.Duration
	budget   int

	submit func(agent.Task)
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	stepFor map[string]string // taskID -> stepID
	seen    map[string]bool   // stepIDs already submitted (avoid re-submit between polls)
}

// NewStep builds a step channel polling src every interval, stamping tasks with
// budget.
func NewStep(src StepSource, interval time.Duration, budget int) *Step {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Step{src: src, interval: interval, budget: budget, stop: make(chan struct{}),
		stepFor: map[string]string{}, seen: map[string]bool{}}
}

func (s *Step) Name() string { return "step" }

func (s *Step) Start(ctx context.Context, submit func(agent.Task)) error {
	s.submit = submit
	go s.poll(ctx)
	return nil
}

func (s *Step) poll(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.scan(ctx)
		}
	}
}

func (s *Step) scan(ctx context.Context) {
	steps, err := s.src.WaitingSteps(ctx)
	if err != nil {
		return
	}
	for _, st := range steps {
		s.mu.Lock()
		dup := s.seen[st.ID]
		if !dup {
			s.seen[st.ID] = true
		}
		s.mu.Unlock()
		if dup {
			continue
		}
		taskID := "step-" + st.ID
		s.mu.Lock()
		s.stepFor[taskID] = st.ID
		s.mu.Unlock()
		s.submit(agent.Task{ID: taskID, Channel: "step", Budget: s.budget,
			Messages: []agent.Message{{Role: agent.RoleUser, Content: promptFromStep(st)}}})
	}
}

// promptFromStep renders a step's parked request into the task prompt. A
// "message" field (the @sys/message convention) is used directly; otherwise the
// whole partial_args is rendered.
func promptFromStep(st StepInfo) string {
	if msg, ok := st.PartialArgs["message"].(string); ok && msg != "" {
		return msg
	}
	if req, ok := st.PartialArgs["request"].(string); ok && req != "" {
		return req
	}
	return renderArgs(st.PartialArgs)
}

// Deliver completes the originating step with the task's answer, settling it
// under the role law.
func (s *Step) Deliver(taskID string, out agent.Outcome) {
	s.mu.Lock()
	stepID, ok := s.stepFor[taskID]
	delete(s.stepFor, taskID)
	s.mu.Unlock()
	if !ok {
		return
	}
	args := out.Answer
	if args == nil {
		args = map[string]any{"status": string(out.Status)}
	}
	_ = s.src.CompleteStep(context.Background(), stepID, args)
}

// Steering is unused by the step channel in v1.
func (s *Step) Steering(string) []agent.Message { return nil }

func (s *Step) Stop() { s.once.Do(func() { close(s.stop) }) }
