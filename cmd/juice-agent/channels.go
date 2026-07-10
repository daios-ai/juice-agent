package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
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
