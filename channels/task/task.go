// Package task is the kernel-native intake channel: it polls the kernel for
// waiting tasks (kernel D6) addressed to the agent, turns each into a job, and
// delivers the outcome by completing the task. This is how any other agent, a
// human via sys@kernel/message, or a message-in-task bridge (SMS/Slack) hands work
// to the agent with zero pairwise integration. It speaks the kernel only through
// channels.TaskSource, staying kernel-free like the loop.
package task

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
)

// Channel is the kernel-native intake surface. It satisfies channels.Channel.
//
// One active job per conversation: a session is (owner, thread); a task for a
// session already in flight is deferred until that session's job settles, so a
// sender's messages are processed in order and never interleave. The trade is up to
// one poll interval of latency for a queued follow-up.
//
// Recovery needs no persisted claim state: the kernel's task status is the durable
// guard. `seen` (in-memory) only prevents re-submitting a task between polls within
// one run. On restart it resets, so a still-`waiting` task (the agent crashed before
// completing it) is re-polled and redone, while a completed task is `done`
// kernel-side and never returned again — no double-delivery. A crash mid-job may
// repeat the loop's side effects (the at-least-once caveat); the kernel settles the
// interrupted trace as failed independently.
type Channel struct {
	src      channels.TaskSource
	interval time.Duration
	budget   int
	address  string          // this agent's address; only tasks it must complete are its work
	exclude  map[string]bool // owners claimed by a dedicated channel (e.g. chat); skipped here

	submit func(agent.Job)
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	pending map[string]taskMeta       // jobID -> provenance (kept out of Job so the loop stays channel-blind)
	seen    map[string]bool           // task ids already submitted (avoid re-submit between polls)
	active  map[string]bool           // sessions with a job in flight (one active job per conversation)
	unsent  map[string]map[string]any // task id -> completion args a transient failure left undelivered
}

// taskMeta is a job's channel provenance: the originating task, its conversation
// session, and the reply contract. Kept in the channel, never in the Job.
type taskMeta struct {
	taskID       string
	session      string
	allowedInput map[string]any
}

// New builds a task channel polling src every interval, stamping jobs with
// budget. address is the agent's own address: only tasks whose required caller is
// this agent are its work (a task it merely funded is skipped). exclude names
// owners a dedicated channel claims (e.g. chat@work) — their tasks are left for
// that channel, so the generic and dedicated channels partition the agent's tasks
// without overlap.
func New(src channels.TaskSource, interval time.Duration, budget int, address string, exclude ...string) *Channel {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ex := map[string]bool{}
	for _, h := range exclude {
		ex[h] = true
	}
	return &Channel{src: src, interval: interval, budget: budget, address: address, exclude: ex, stop: make(chan struct{}),
		pending: map[string]taskMeta{}, seen: map[string]bool{}, active: map[string]bool{}, unsent: map[string]map[string]any{}}
}

func (s *Channel) Name() string { return "task" }

func (s *Channel) Start(ctx context.Context, submit func(agent.Job)) error {
	s.submit = submit
	go s.poll(ctx)
	return nil
}

func (s *Channel) poll(ctx context.Context) {
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

func (s *Channel) scan(ctx context.Context) {
	s.mu.Lock()
	unsent := s.unsent
	s.unsent = map[string]map[string]any{}
	s.mu.Unlock()
	for id, args := range unsent {
		s.complete(ctx, id, args) // the job is done; only its answer is owed
	}
	tasks, err := s.src.WaitingTasks(ctx)
	if err != nil {
		return
	}
	for _, st := range tasks {
		if !s.addressedToMe(st) {
			continue // a task the agent funded but must not complete is not its work
		}
		session := sessionKey(st)
		s.mu.Lock()
		// One active job per conversation: a task for a busy session is deferred
		// (not marked seen) so a later poll re-evaluates it once the session frees.
		if s.seen[st.ID] || s.active[session] {
			s.mu.Unlock()
			continue
		}
		s.seen[st.ID] = true
		s.active[session] = true
		jobID := "task-" + st.ID
		s.pending[jobID] = taskMeta{taskID: st.ID, session: session, allowedInput: st.AllowedInput}
		s.mu.Unlock()
		s.submit(agent.Job{ID: jobID, Channel: "task", Budget: s.budget,
			Messages: []agent.Message{{Role: agent.RoleUser, Content: promptFromTask(st)}}})
	}
}

// addressedToMe reports whether a waiting task is this agent's to do: its
// required caller is this agent, and its owner is not claimed by a dedicated
// channel.
func (s *Channel) addressedToMe(t channels.TaskInfo) bool {
	return !s.exclude[t.Owner] && t.RequiredCaller == s.address
}

// sessionKey groups a sender's tasks into one conversation: the owner plus an
// optional thread carried in the envelope. Follow-ups from the same owner/thread
// share a session.
func sessionKey(st channels.TaskInfo) string {
	sender := st.Owner
	if thread, ok := st.PartialArgs["thread"].(string); ok && thread != "" {
		return sender + "\x1f" + thread
	}
	return sender
}

// promptFromTask renders a task's parked request into the job prompt. A "message"
// field (the message native's convention) is used directly; otherwise the whole
// partial_args is rendered.
func promptFromTask(st channels.TaskInfo) string {
	if msg, ok := st.PartialArgs["message"].(string); ok && msg != "" {
		return msg
	}
	if req, ok := st.PartialArgs["request"].(string); ok && req != "" {
		return req
	}
	return renderArgs(st.PartialArgs)
}

// Deliver completes the originating task with the job's answer. Completing runs
// the task's onward action, which delivers the reply (for a sink the reply rides in
// the completion transaction's args, readable by the sender). A failed or
// interrupted job still completes (ack), so the sender's process is released and
// the exchange never dangles. A completion that fails transiently is resent on
// later polls until the kernel accepts or refuses it; the job is not run again.
func (s *Channel) Deliver(jobID string, out agent.Outcome) {
	s.mu.Lock()
	meta, ok := s.pending[jobID]
	delete(s.pending, jobID)
	if ok {
		delete(s.active, meta.session) // free the conversation for its next queued task
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
	s.complete(context.Background(), meta.taskID, args)
}

// complete delivers a task's completion, keeping it for the next poll when the
// failure is transient. A permanent refusal ends the attempt: the task is no
// longer waiting, or it stays waiting in the kernel, visible to the operator.
func (s *Channel) complete(ctx context.Context, id string, args map[string]any) {
	if err := s.src.CompleteTask(ctx, id, args); err != nil && s.src.Transient(err) {
		s.mu.Lock()
		s.unsent[id] = args
		s.mu.Unlock()
	}
}

// shapeReply maps the loop's reply (the reply tool's {answer: text}) onto the onward
// action's reply contract. For an unconstrained onward (e.g. a sink) the reply
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

// Steering is unused by the task channel in v1.
func (s *Channel) Steering(string) []agent.Message { return nil }

func (s *Channel) Stop() { s.once.Do(func() { close(s.stop) }) }

// renderArgs marshals a request envelope to compact JSON for the job prompt.
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
