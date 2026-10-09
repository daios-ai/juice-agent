package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestHTTPChannelSubmitsAndDelivers(t *testing.T) {
	h := NewHTTP(":0", 50) // addr unused; we drive handlers directly
	var got agent.Job
	var wg sync.WaitGroup
	wg.Add(1)
	h.submit = func(tk agent.Job) { got = tk; wg.Done() }
	h.outcomes = map[string]agent.Outcome{}
	h.done = map[string]chan struct{}{}

	// POST /task
	body, _ := json.Marshal(map[string]any{"prompt": "do the thing", "budget": 30})
	req := httptest.NewRequest(http.MethodPost, "/task", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleTask(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var resp struct {
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	wg.Wait()

	if got.Channel != "http" || got.Budget != 30 {
		t.Fatalf("task = %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "do the thing" {
		t.Fatalf("messages = %+v", got.Messages)
	}

	// GET /task/{id} before delivery -> running
	rec = httptest.NewRecorder()
	h.handleOutcome(rec, httptest.NewRequest(http.MethodGet, "/task/"+resp.TaskID, nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pre-delivery status = %d, want 202", rec.Code)
	}

	// Deliver, then GET -> outcome
	h.Deliver(resp.TaskID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "did it"}})
	rec = httptest.NewRecorder()
	h.handleOutcome(rec, httptest.NewRequest(http.MethodGet, "/task/"+resp.TaskID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-delivery status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "did it") {
		t.Fatalf("outcome body = %s", rec.Body.String())
	}
}

func TestHTTPMissingPromptRejected(t *testing.T) {
	h := NewHTTP(":0", 50)
	h.done = map[string]chan struct{}{}
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"budget": 10})
	h.handleTask(rec, httptest.NewRequest(http.MethodPost, "/task", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
