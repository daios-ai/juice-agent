package main

import (
	"context"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// Runtime is the daemon's core: a bounded worker pool consuming tasks from a
// queue that channels and the maker feed. It routes each finished task's outcome
// back through its originating channel and drains cleanly on shutdown.
type Runtime struct {
	exec     *Executor
	channels map[string]Channel
	queue    chan agent.Task
	log      func(string, map[string]any)

	wg      sync.WaitGroup
	mu      sync.RWMutex
	closing bool
}

// NewRuntime wires an executor to a set of channels with the given worker count.
func NewRuntime(exec *Executor, channels []Channel, concurrency int, log func(string, map[string]any)) *Runtime {
	if concurrency <= 0 {
		concurrency = 4
	}
	chm := map[string]Channel{}
	for _, ch := range channels {
		chm[ch.Name()] = ch
	}
	return &Runtime{
		exec:     exec,
		channels: chm,
		queue:    make(chan agent.Task, concurrency*8),
		log:      log,
	}
}

// submit enqueues a task unless the runtime is shutting down. The maker's
// Enqueue and every channel's producer funnel through here. A full queue drops
// with a log line rather than blocking a worker (which would risk deadlock on
// shutdown).
func (r *Runtime) submit(t agent.Task) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closing {
		r.log("task_dropped", map[string]any{"task_id": t.ID, "reason": "shutting down"})
		return
	}
	select {
	case r.queue <- t:
	default:
		r.log("task_dropped", map[string]any{"task_id": t.ID, "reason": "queue full"})
	}
}

// Start launches the workers and every channel's producer. runCtx bounds task
// execution; it is not cancelled on graceful shutdown (in-flight tasks drain).
func (r *Runtime) Start(runCtx context.Context, concurrency int) error {
	r.exec.Enqueue = r.submit
	for i := 0; i < concurrency; i++ {
		r.wg.Add(1)
		go r.worker(runCtx)
	}
	for _, ch := range r.channels {
		if err := ch.Start(runCtx, r.submit); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) worker(ctx context.Context) {
	defer r.wg.Done()
	for t := range r.queue {
		r.run(ctx, t)
	}
}

func (r *Runtime) run(ctx context.Context, t agent.Task) {
	ch := r.channels[t.Channel]
	var steering func() []agent.Message
	if ch != nil {
		steering = func() []agent.Message { return ch.Steering(t.ID) }
	}
	out, _ := r.exec.Execute(ctx, t, steering)
	if ch != nil {
		ch.Deliver(t.ID, out)
	}
}

// Shutdown stops accepting new tasks, halts channel producers, and waits for
// in-flight tasks to finish (pi's drain semantics).
func (r *Runtime) Shutdown() {
	r.mu.Lock()
	if !r.closing {
		r.closing = true
		close(r.queue)
	}
	r.mu.Unlock()
	for _, ch := range r.channels {
		ch.Stop()
	}
	r.wg.Wait()
}

// stepAdapter bridges the juice client to the channel package's StepSource,
// keeping channel independent of the kernel client.
type stepAdapter struct{ cl *juice.Client }

func (a stepAdapter) WaitingSteps(ctx context.Context) ([]StepInfo, error) {
	steps, err := a.cl.WaitingSteps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]StepInfo, 0, len(steps))
	for _, s := range steps {
		out = append(out, StepInfo{ID: s.ID, PartialArgs: s.PartialArgs, CreatedBy: s.CreatedBy})
	}
	return out, nil
}

func (a stepAdapter) CompleteStep(ctx context.Context, id string, args map[string]any) error {
	_, err := a.cl.CompleteStep(ctx, id, args)
	return err
}

// Channel is one task-intake surface. Start begins producing tasks (submitting
// each via the callback) and returns once producers are running; Deliver reports
// a finished task's outcome; Steering supplies optional mid-task input; Stop
// halts production.
type Channel interface {
	Name() string
	Start(ctx context.Context, submit func(agent.Task)) error
	Deliver(taskID string, out agent.Outcome)
	Steering(taskID string) []agent.Message
	Stop()
}
