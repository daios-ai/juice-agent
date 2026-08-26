// Package step is the kernel-native intake channel: it polls the kernel for
// waiting steps addressed to the agent, turns each into a task, and delivers the
// outcome by completing the step. This is how any other agent (e.g. chat), a human
// via sys/message, or a message-in-step bridge (SMS/Slack) hands work to the agent
// with zero pairwise integration. It speaks the kernel only through
// channels.StepSource, staying kernel-free like the loop.
package step

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
)

// Step is the kernel-native intake surface. It satisfies channels.Channel.
//
// One active task per conversation: a session is (owner_handle, thread); a step for
// a session already in flight is deferred until that session's task settles, so a
// sender's messages are processed in order and never interleave. The trade is up to
// one poll interval of latency for a queued follow-up.
//
// Recovery needs no persisted claim state: the kernel's step status is the durable
// guard. `seen` (in-memory) only prevents re-submitting a step between polls within
// one run. On restart it resets, so a still-`waiting` step (the agent crashed before
// completing it) is re-polled and redone, while a completed step is `done`
// kernel-side and never returned again — no double-delivery. A crash mid-task may
// repeat the loop's side effects (the at-least-once caveat); the kernel settles the
// interrupted trace as failed independently.
type Step struct {
	src      channels.StepSource
	interval time.Duration
	budget   int
	handle   string          // this agent's handle; only steps addressed to it are its work
	exclude  map[string]bool // owner_handles claimed by a dedicated channel (e.g. chat); skipped here

	submit func(agent.Task)
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	pending map[string]stepMeta // taskID -> provenance (kept out of Task so the loop stays channel-blind)
	seen    map[string]bool     // stepIDs already submitted (avoid re-submit between polls)
	active  map[string]bool     // sessions with a task in flight (one active task per conversation)
}

// stepMeta is a task's channel provenance: the originating step, its conversation
// session, and the reply contract. Kept in the channel, never in the Task.
type stepMeta struct {
	stepID       string
	session      string
	allowedInput map[string]any
}

// NewStep builds a step channel polling src every interval, stamping tasks with
// budget. handle is the agent's own handle: only steps whose required caller is this
// agent are its work (a step it merely owns is skipped). exclude names owner_handles
// a dedicated channel claims (e.g. "chat") — steps from those senders are left for
// that channel, so the generic and dedicated channels partition the agent's steps
// without overlap.
func NewStep(src channels.StepSource, interval time.Duration, budget int, handle string, exclude ...string) *Step {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ex := map[string]bool{}
	for _, h := range exclude {
		ex[h] = true
	}
	return &Step{src: src, interval: interval, budget: budget, handle: handle, exclude: ex, stop: make(chan struct{}),
		pending: map[string]stepMeta{}, seen: map[string]bool{}, active: map[string]bool{}}
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
		if !s.addressedToMe(st) {
			continue // a step the agent owns but is not the required caller of is not its work
		}
		session := sessionKey(st)
		s.mu.Lock()
		// One active task per conversation: a step for a busy session is deferred
		// (not marked seen) so a later poll re-evaluates it once the session frees.
		if s.seen[st.ID] || s.active[session] {
			s.mu.Unlock()
			continue
		}
		s.seen[st.ID] = true
		s.active[session] = true
		taskID := "step-" + st.ID
		s.pending[taskID] = stepMeta{stepID: st.ID, session: session, allowedInput: st.AllowedInput}
		s.mu.Unlock()
		s.submit(agent.Task{ID: taskID, Channel: "step", Budget: s.budget,
			Messages: []agent.Message{{Role: agent.RoleUser, Content: promptFromStep(st)}}})
	}
}

// addressedToMe reports whether a waiting step is this agent's to do: its required
// caller must be this agent, and its sender must not be claimed by a dedicated
// channel. An empty required caller (only test fakes produce that; the kernel always
// sets it) is treated as addressed here.
func (s *Step) addressedToMe(st channels.StepInfo) bool {
	if s.exclude[st.OwnerHandle] {
		return false // a dedicated channel (e.g. chat) claims this sender
	}
	return st.RequiredCaller == "" || channels.SameHandle(st.RequiredCaller, s.handle)
}

// sessionKey groups a sender's steps into one conversation: the sender
// (owner_handle) plus an optional thread carried in the envelope. Follow-ups from
// the same sender/thread share a session.
func sessionKey(st channels.StepInfo) string {
	sender := st.OwnerHandle
	if thread, ok := st.PartialArgs["thread"].(string); ok && thread != "" {
		return sender + "\x1f" + thread
	}
	return sender
}

// promptFromStep renders a step's parked request into the task prompt. A "message"
// field (the sys/message convention) is used directly; otherwise the whole
// partial_args is rendered.
func promptFromStep(st channels.StepInfo) string {
	if msg, ok := st.PartialArgs["message"].(string); ok && msg != "" {
		return msg
	}
	if req, ok := st.PartialArgs["request"].(string); ok && req != "" {
		return req
	}
	return renderArgs(st.PartialArgs)
}

// Deliver completes the originating step with the task's answer. Completing runs the
// step's onward action, which delivers the reply (for sys/sink the reply rides in
// the completion transaction's args, readable by the sender). A failed or
// interrupted task still completes (ack), so the sender's process is released and the
// exchange never dangles.
func (s *Step) Deliver(taskID string, out agent.Outcome) {
	s.mu.Lock()
	meta, ok := s.pending[taskID]
	delete(s.pending, taskID)
	if ok {
		delete(s.active, meta.session) // free the conversation for its next queued step
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	args := out.Answer
	if args == nil {
		args = map[string]any{"status": string(out.Status)}
		if out.Reason != "" {
			args["reason"] = out.Reason
		}
	} else {
		args = shapeReply(args, meta.allowedInput)
	}
	_ = s.src.CompleteStep(context.Background(), meta.stepID, args)
}

// shapeReply maps the loop's reply (the reply tool's {answer: text}) onto the onward
// action's reply contract. For an unconstrained onward (e.g. sys/sink) the reply
// rides through as {answer: text} (readable in the completion tx args). For a
// delivery action that declares a text-carrying field (text/message/…) the answer is
// placed under it, so a bridge's action shape drives the mapping — the agent needs no
// per-bridge knowledge. When it cannot map confidently, the reply passes through
// unchanged and the kernel validates.
func shapeReply(answer, allowed map[string]any) map[string]any {
	text, ok := answer["answer"].(string)
	if !ok {
		return answer // non-standard reply; pass through
	}
	props, _ := allowed["properties"].(map[string]any)
	if len(props) == 0 {
		return map[string]any{"answer": text} // unconstrained onward (sink): reply rides in tx args
	}
	for _, name := range []string{"text", "message", "answer", "body", "reply", "content"} {
		if isStringProp(props, name) {
			return map[string]any{name: text}
		}
	}
	if only := onlyStringProp(props); only != "" {
		return map[string]any{only: text}
	}
	return answer // cannot map confidently; let the kernel validate
}

func isStringProp(props map[string]any, name string) bool {
	m, ok := props[name].(map[string]any)
	return ok && m["type"] == "string"
}

// onlyStringProp returns the sole string property's name, or "" if there is not
// exactly one.
func onlyStringProp(props map[string]any) string {
	found := ""
	for name, spec := range props {
		if m, ok := spec.(map[string]any); ok && m["type"] == "string" {
			if found != "" {
				return ""
			}
			found = name
		}
	}
	return found
}

// Steering is unused by the step channel in v1.
func (s *Step) Steering(string) []agent.Message { return nil }

func (s *Step) Stop() { s.once.Do(func() { close(s.stop) }) }

// renderArgs marshals a request envelope to compact JSON for the task prompt.
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
