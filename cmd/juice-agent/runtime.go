package main

import (
	"context"
	"fmt"
	"strings"
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
	queue chan agent.Task
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
		queue: make(chan agent.Task, concurrency*8),
		log:   log,
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

func (r *Runtime) run(ctx context.Context, t agent.Task) {
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

// stepAdapter bridges the juice client to the channel package's StepSource,
// keeping channel independent of the kernel client.
type stepAdapter struct{ cl *juice.Client }

func (a stepAdapter) WaitingSteps(ctx context.Context) ([]channels.StepInfo, error) {
	steps, err := a.cl.WaitingSteps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]channels.StepInfo, 0, len(steps))
	for _, s := range steps {
		out = append(out, channels.StepInfo{
			ID:             s.ID,
			OwnerHandle:    s.OwnerHandle,
			RequiredCaller: s.RequiredCallerHandle,
			ActionRef:      s.ActionRef,
			PartialArgs:    s.PartialArgs,
			AllowedInput:   s.AllowedInput,
			CreatedBy:      s.CreatedBy,
		})
	}
	return out, nil
}

func (a stepAdapter) CompleteStep(ctx context.Context, id string, args map[string]any) error {
	_, err := a.cl.CompleteStep(ctx, id, args)
	return err
}

// chatHandle is the juice-ui chat service's user handle; its member actions live
// in the msg/ folder (@chat/msg/read, @chat/msg/message) under one grant selector
// (chatGrant), and it owns the doorbell steps that notify members of new messages.
const chatHandle = "@chat"
const chatGrant = "@chat/msg"

// chatAdapter implements channels.ServiceActions over the juice client, speaking
// @chat's kernel actions so the channels package stays kernel-free. It also
// provisions the @chat/msg grant on demand (ensureGrant), so enabling the chat
// channel is only a config flag: a first-run, absent, or revoked grant self-heals
// lazily on the first grant_required from a read or post. A pointer receiver is
// required — the mutex serializes concurrent provisioning.
type chatAdapter struct {
	cl     *juice.Client
	handle string
	mu     sync.Mutex // serializes ensureGrant across concurrent Read/Post
}

func (a *chatAdapter) Pending(ctx context.Context) ([]channels.StepInfo, error) {
	steps, err := a.cl.WaitingSteps(ctx)
	if err != nil {
		return nil, err
	}
	var out []channels.StepInfo
	for _, s := range steps {
		if s.OwnerHandle == chatHandle { // @chat owns the doorbells; the agent is their required caller
			out = append(out, channels.StepInfo{ID: s.ID, OwnerHandle: s.OwnerHandle,
				RequiredCaller: s.RequiredCallerHandle, PartialArgs: s.PartialArgs})
		}
	}
	return out, nil
}

func (a *chatAdapter) SessionKey(s channels.StepInfo) string {
	room, _ := s.PartialArgs["room"].(string)
	return room
}

// Read fetches a room's new messages, provisioning the grant and retrying once if
// the kernel reports the agent has not yet connected @chat/msg.
func (a *chatAdapter) Read(ctx context.Context, room string, after int64) ([]channels.Msg, error) {
	out, err := a.readOnce(ctx, room, after)
	if juice.IsGrantRequired(err) {
		if gerr := a.ensureGrant(ctx); gerr != nil {
			return nil, err
		}
		out, err = a.readOnce(ctx, room, after)
	}
	return out, err
}

func (a *chatAdapter) readOnce(ctx context.Context, room string, after int64) ([]channels.Msg, error) {
	res, err := a.cl.Run(ctx, chatHandle+"/msg/read", map[string]any{"room": room, "after": after})
	if err != nil {
		return nil, err
	}
	raw, _ := res.Result["messages"].([]any)
	out := make([]channels.Msg, 0, len(raw))
	for _, mi := range raw {
		m, _ := mi.(map[string]any)
		out = append(out, channels.Msg{Seq: asInt64(m["seq"]), From: asString(m["from"]), Text: asString(m["text"])})
	}
	return out, nil
}

// Post delivers a reply, provisioning the grant and retrying once on
// grant_required (the same self-heal as Read).
func (a *chatAdapter) Post(ctx context.Context, room, text string) error {
	err := a.postOnce(ctx, room, text)
	if juice.IsGrantRequired(err) {
		if gerr := a.ensureGrant(ctx); gerr != nil {
			return err
		}
		err = a.postOnce(ctx, room, text)
	}
	return err
}

func (a *chatAdapter) postOnce(ctx context.Context, room, text string) error {
	_, err := a.cl.Run(ctx, chatHandle+"/msg/message", map[string]any{"room": room, "text": text})
	return err
}

func (a *chatAdapter) Complete(ctx context.Context, stepID string) error {
	_, err := a.cl.CompleteStep(ctx, stepID, nil)
	return err
}

func (a *chatAdapter) Transient(err error) bool { return juice.IsRetryable(err) }

// ensureGrant makes sure the agent holds the @chat/msg grant so it can read and
// post. It is idempotent: if /v1/me already lists the @chat/msg connector it
// returns at once. Otherwise it runs the headless path the browser's "Connect to
// chat" button uses — register the agent's handle for a pending key, complete the
// verification step (which the kernel lets only the handle's owner do, proving
// identity), then attach the key as the @chat/msg grant. The mutex ensures only
// one provisioning runs even under concurrent Read/Post.
func (a *chatAdapter) ensureGrant(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	self, err := a.cl.Me(ctx)
	if err != nil {
		return err
	}
	if hasConnector(self, chatGrant) {
		return nil // another caller (or a prior run) already connected it
	}
	reg, err := a.cl.Run(ctx, chatHandle+"/register", map[string]any{"username": a.handle})
	if err != nil {
		return err
	}
	apiKey, stepID := asString(reg.Result["api_key"]), asString(reg.Result["step_id"])
	if apiKey == "" || stepID == "" {
		return fmt.Errorf("chat register: empty api_key or step_id")
	}
	if _, err := a.cl.CompleteStep(ctx, stepID, nil); err != nil { // owner completes -> activates the key
		return err
	}
	return a.cl.ConnectGrant(ctx, chatGrant, apiKey)
}

// hasConnector reports whether self already holds a grant under selector — its
// directory node, or any action beneath it. This is the provisioning idempotency
// check.
func hasConnector(self juice.Self, selector string) bool {
	for _, c := range self.Connectors {
		if c.Directory == selector {
			return true
		}
		for _, ac := range c.Actions {
			if strings.HasPrefix(ac.Action, selector+"/") {
				return true
			}
		}
	}
	return false
}

// asInt64 coerces a JSON number (float64) or integer to int64.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func asString(v any) string { s, _ := v.(string); return s }

