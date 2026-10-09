package main

import (
	"context"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
	"github.com/daios-ai/juice-agent/juice"
)

// Runtime is the daemon's core: a bounded worker pool consuming tasks from a
// queue that channels and the maker feed. It routes each finished task's outcome
// back through its originating channel and drains cleanly on shutdown.
type Runtime struct {
	exec  *Executor
	chans map[string]channels.Channel
	queue chan agent.Job
	log   func(string, map[string]any)

	wg      sync.WaitGroup
	mu      sync.RWMutex
	closing bool
}

// NewRuntime wires an executor to a set of channels with the given worker count.
func NewRuntime(exec *Executor, chans []channels.Channel, concurrency int, log func(string, map[string]any)) *Runtime {
	if concurrency <= 0 {
		concurrency = 4
	}
	chm := map[string]channels.Channel{}
	for _, ch := range chans {
		chm[ch.Name()] = ch
	}
	return &Runtime{
		exec:  exec,
		chans: chm,
		queue: make(chan agent.Job, concurrency*8),
		log:   log,
	}
}

// submit enqueues a task unless the runtime is shutting down. The maker's Enqueue
// and every channel's producer funnel through here. A full queue drops with a log
// line rather than blocking a worker (which would risk deadlock on shutdown).
func (r *Runtime) submit(t agent.Job) {
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
	for _, ch := range r.chans {
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

func (r *Runtime) run(ctx context.Context, t agent.Job) {
	ch := r.chans[t.Channel]
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
	for _, ch := range r.chans {
		ch.Stop()
	}
	r.wg.Wait()
}

// kernelAdapter implements channels.Kernel over the juice client — the single
// bridge from the kernel-free channels packages to the kernel. Every channel
// receives it: the task channel as a channels.TaskSource (the embedded subset),
// chat as the full channels.Kernel. It is the only place juice types are mapped
// into channel terms, so adding a channel needs no new kernel code here.
type kernelAdapter struct{ cl *juice.Client }

func (a kernelAdapter) WaitingTasks(ctx context.Context) ([]channels.TaskInfo, error) {
	tasks, err := a.cl.WaitingTasks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]channels.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, channels.TaskInfo{
			ID:             t.ID,
			Owner:          t.Owner,
			RequiredCaller: t.RequiredCaller,
			ActionRef:      t.ActionRef,
			PartialArgs:    t.PartialArgs,
			AllowedInput:   t.AllowedInput,
			CreatedBy:      t.CreatedBy,
		})
	}
	return out, nil
}

func (a kernelAdapter) CompleteTask(ctx context.Context, id string, args map[string]any) error {
	return a.cl.CompleteTask(ctx, id, args)
}

func (a kernelAdapter) Run(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	res, err := a.cl.Run(ctx, action, args, "", "")
	if err != nil {
		return nil, err
	}
	return res.Result, nil
}

// Connectors projects the agent's grant tree (GET /v1/me) to the directory strings
// a channel checks for provisioning idempotency.
func (a kernelAdapter) Connectors(ctx context.Context) ([]string, error) {
	self, err := a.cl.Me(ctx)
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(self.Connectors))
	for _, c := range self.Connectors {
		dirs = append(dirs, c.Directory)
	}
	return dirs, nil
}

func (a kernelAdapter) ConnectGrant(ctx context.Context, selector, token string) error {
	return a.cl.ConnectGrant(ctx, selector, token)
}

func (a kernelAdapter) Transient(err error) bool     { return juice.IsRetryable(err) }
func (a kernelAdapter) GrantRequired(err error) bool { return juice.IsGrantRequired(err) }
