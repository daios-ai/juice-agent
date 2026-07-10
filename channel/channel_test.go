package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

func TestHTTPChannelSubmitsAndDelivers(t *testing.T) {
	h := NewHTTP(":0", 50) // addr unused; we drive handlers directly
	var got agent.Task
	var wg sync.WaitGroup
	wg.Add(1)
	h.submit = func(tk agent.Task) { got = tk; wg.Done() }
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

// fakeStepSource serves canned waiting steps and records completions.
type fakeStepSource struct {
	mu        sync.Mutex
	steps     []StepInfo
	completed map[string]map[string]any
}

func (f *fakeStepSource) WaitingSteps(context.Context) ([]StepInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps, nil
}
func (f *fakeStepSource) CompleteStep(_ context.Context, id string, args map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completed == nil {
		f.completed = map[string]map[string]any{}
	}
	f.completed[id] = args
	f.steps = nil // step leaves the waiting list once completed
	return nil
}

func TestStepChannelSubmitsAndCompletes(t *testing.T) {
	src := &fakeStepSource{steps: []StepInfo{
		{ID: "s1", PartialArgs: map[string]any{"message": "please summarise X"}, CreatedBy: "@chat"},
	}}
	ch := NewStep(src, 5*time.Millisecond, 20)

	var got agent.Task
	var wg sync.WaitGroup
	wg.Add(1)
	submitted := false
	var smu sync.Mutex
	ch.Start(context.Background(), func(tk agent.Task) {
		smu.Lock()
		defer smu.Unlock()
		if submitted {
			return
		}
		submitted = true
		got = tk
		wg.Done()
	})
	wg.Wait()
	ch.Stop()

	if got.Channel != "step" || got.Budget != 20 {
		t.Fatalf("task = %+v", got)
	}
	if got.Messages[0].Content != "please summarise X" {
		t.Fatalf("prompt = %q, want the step message", got.Messages[0].Content)
	}

	// Deliver -> the step is completed with the answer.
	ch.Deliver(got.ID, agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": "summary"}})
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed["s1"]["answer"] != "summary" {
		t.Fatalf("step s1 completed with %v, want answer=summary", src.completed["s1"])
	}
}

func TestStepChannelDoesNotResubmit(t *testing.T) {
	src := &fakeStepSource{steps: []StepInfo{{ID: "s1", PartialArgs: map[string]any{"message": "x"}}}}
	ch := NewStep(src, 2*time.Millisecond, 20)
	var count int
	var mu sync.Mutex
	ch.Start(context.Background(), func(agent.Task) { mu.Lock(); count++; mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // several poll cycles
	ch.Stop()
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("submitted %d times, want 1 (no re-submit across polls)", count)
	}
}
