// Package http is a Juice-independent intake channel: a local HTTP endpoint that
// accepts tasks (POST /task) and serves their outcomes (GET /task/{id}). It exists
// so the agent can be driven without going through the kernel at all. It satisfies
// channels.Channel and needs no kernel access.
package http

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/daios-ai/juice-agent/agent"
)

// HTTP is the local HTTP intake channel.
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

// NewHTTP builds an HTTP channel bound to addr, stamping tasks with defaultBudget
// when a request omits one.
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
