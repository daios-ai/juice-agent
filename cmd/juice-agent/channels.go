package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// HTTP is a Juice-independent intake surface: a local HTTP endpoint that accepts
// tasks (POST /task) and serves their outcomes (GET /task/{id}). It exists so
// the agent can be driven without going through the kernel at all.
type HTTP struct {
	addr          string
	defaultBudget int

	srv    *http.Server
	submit func(agent.Task)
	seq    int64

	// Addr, once Start returns, is the actually-bound address (useful when addr
	// requested port 0). Read-only after Start.
	Addr string

	mu       sync.Mutex
	outcomes map[string]agent.Outcome
	done     map[string]chan struct{}
}

// NewHTTP builds an HTTP channel bound to addr, stamping tasks with
// defaultBudget when a request omits one.
func NewHTTP(addr string, defaultBudget int) *HTTP {
	return &HTTP{addr: addr, defaultBudget: defaultBudget,
		outcomes: map[string]agent.Outcome{}, done: map[string]chan struct{}{}}
}

func (h *HTTP) Name() string { return "http" }

func (h *HTTP) Start(_ context.Context, submit func(agent.Task)) error {
	h.submit = submit
	mux := http.NewServeMux()
	mux.HandleFunc("/task", h.handleTask)
	mux.HandleFunc("/task/", h.handleOutcome)
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}
	h.Addr = ln.Addr().String()
	h.srv = &http.Server{Handler: mux}
	go h.srv.Serve(ln)
	return nil
}

func (h *HTTP) handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
		Budget int    `json:"budget"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	budget := req.Budget
	if budget <= 0 {
		budget = h.defaultBudget
	}
	id := "http-" + strconv.FormatInt(atomic.AddInt64(&h.seq, 1), 10)
	h.mu.Lock()
	h.done[id] = make(chan struct{})
	h.mu.Unlock()

	h.submit(agent.Task{ID: id, Channel: "http", Budget: budget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: req.Prompt}}})

	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": id})
}

func (h *HTTP) handleOutcome(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/task/"):]
	h.mu.Lock()
	out, ok := h.outcomes[id]
	_, known := h.done[id]
	h.mu.Unlock()
	if ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if known {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
		return
	}
	http.Error(w, "unknown task", http.StatusNotFound)
}

// Deliver records a task's outcome and wakes any waiter.
func (h *HTTP) Deliver(taskID string, out agent.Outcome) {
	h.mu.Lock()
	h.outcomes[taskID] = out
	if ch, ok := h.done[taskID]; ok {
		close(ch)
		delete(h.done, taskID)
	}
	h.mu.Unlock()
}

// Steering is unused by the HTTP channel in v1.
func (h *HTTP) Steering(string) []agent.Message { return nil }

func (h *HTTP) Stop() {
	if h.srv != nil {
		h.srv.Shutdown(context.Background())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

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
//
// The step is the message: OwnerHandle is the sender (the channel/session
// identity), PartialArgs is the envelope, ActionRef is the onward action the
// reply is delivered through (its completion), and AllowedInput is the reply
// contract (the onward action's input_schema minus the pre-bound keys).
type StepInfo struct {
	ID             string
	OwnerHandle    string         // sender/payer — the channel and session key
	RequiredCaller string         // whom the step is addressed to; ours to do iff it is us
	ActionRef      string         // the onward (completion-target) action
	PartialArgs    map[string]any // the request envelope
	AllowedInput   map[string]any // reply contract: input_schema \ keys(partial_args)
	CreatedBy      string
}

// StepSource lists and completes the agent's waiting steps. Implemented over the
// juice client (via an adapter); faked in tests.
type StepSource interface {
	WaitingSteps(ctx context.Context) ([]StepInfo, error)
	CompleteStep(ctx context.Context, id string, args map[string]any) error
}

// Step is the kernel-native intake surface: it polls the kernel for waiting
// steps addressed to the agent, turns each into a task, and delivers the outcome
// by completing the step. This is how any other agent (e.g. @chat), a human via
// @sys/message, or a channel bridge hands work to the agent with zero pairwise
// integration.
//
// One active task per conversation: a session is (owner_handle, thread); a step
// for a session already in flight is deferred until that session's task settles,
// so a sender's messages are processed in order and never interleave. The trade
// is up to one poll interval of latency for a queued follow-up.
//
// Recovery needs no persisted claim state: the kernel's step status is the
// durable guard. `seen` (in-memory) only prevents re-submitting a step between
// polls within one run. On restart it resets, so a still-`waiting` step (the
// agent crashed before completing it) is re-polled and redone, while a completed
// step is `done` kernel-side and never returned again — no double-delivery. A
// crash mid-task may repeat the loop's side effects (the at-least-once caveat);
// the kernel settles the interrupted trace as failed independently (§5).
type Step struct {
	src      StepSource
	interval time.Duration
	budget   int
	handle   string // this agent's handle; only steps addressed to it are its work

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
// budget. handle is the agent's own handle: only steps whose required caller is
// this agent are its work (a step it merely owns is skipped).
func NewStep(src StepSource, interval time.Duration, budget int, handle string) *Step {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Step{src: src, interval: interval, budget: budget, handle: handle, stop: make(chan struct{}),
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

// addressedToMe reports whether a waiting step is this agent's to do: its
// required caller must be this agent. An empty required caller (only test fakes
// produce that; the kernel always sets it) is treated as addressed here.
func (s *Step) addressedToMe(st StepInfo) bool {
	return st.RequiredCaller == "" || sameHandle(st.RequiredCaller, s.handle)
}

// sessionKey groups a sender's steps into one conversation: the sender
// (owner_handle) plus an optional thread carried in the envelope. Follow-ups
// from the same sender/thread share a session.
func sessionKey(st StepInfo) string {
	sender := st.OwnerHandle
	if thread, ok := st.PartialArgs["thread"].(string); ok && thread != "" {
		return sender + "\x1f" + thread
	}
	return sender
}

// sameHandle compares two @handles ignoring an optional leading '@'.
func sameHandle(a, b string) bool {
	return strings.TrimPrefix(a, "@") == strings.TrimPrefix(b, "@")
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

// Deliver completes the originating step with the task's answer. Completing runs
// the step's onward action, which delivers the reply (for @sys/sink the reply
// rides in the completion transaction's args, readable by the sender). A failed
// or interrupted task still completes (ack), so the sender's process is released
// and the exchange never dangles.
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

// shapeReply maps the loop's reply (the reply tool's {answer: text}) onto the
// onward action's reply contract. For an unconstrained onward (e.g. @sys/sink)
// the reply rides through as {answer: text} (readable in the completion tx args).
// For a delivery action that declares a text-carrying field (text/message/…) the
// answer is placed under it, so a bridge's action shape drives the mapping — the
// agent needs no per-bridge knowledge. When it cannot map confidently, the reply
// passes through unchanged and the kernel validates.
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
