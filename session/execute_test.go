package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

// fakeKernel implements the full session.Kernel interface with scripted
// behaviour, so the executor is exercised end to end without a real kernel.
type fakeKernel struct {
	script []agent.Selection
	i      int
	rated  map[string]int
}

func (k *fakeKernel) Decide(_ context.Context, _ []agent.Message, _ []string) (*agent.Selection, error) {
	if k.i >= len(k.script) {
		return &agent.Selection{Action: "@agent/reply", Args: map[string]any{"answer": "fallback"}}, nil
	}
	s := k.script[k.i]
	k.i++
	return &s, nil
}
func (k *fakeKernel) Lookup(context.Context, string, int) ([]agent.Candidate, error) {
	return nil, nil
}
func (k *fakeKernel) Meta(context.Context, string) (int, agent.ActionStats, error) {
	return 0, agent.ActionStats{}, nil
}
func (k *fakeKernel) Run(_ context.Context, action string, _ map[string]any) (agent.RunResult, error) {
	return agent.RunResult{Result: map[string]any{"ran": action}, TxID: "tx-" + action}, nil
}
func (k *fakeKernel) Chat(context.Context, []agent.Message) (string, error) { return "summary", nil }
func (k *fakeKernel) ChatJSON(context.Context, []agent.Message, map[string]any) (any, error) {
	return map[string]any{"ratings": []any{map[string]any{"index": float64(0), "rating": float64(1), "note": "good"}}}, nil
}
func (k *fakeKernel) Embed(context.Context, string) ([]float64, error) { return []float64{1, 0}, nil }
func (k *fakeKernel) Rate(_ context.Context, txID string, rating int, _ string) error {
	if k.rated == nil {
		k.rated = map[string]int{}
	}
	k.rated[txID] = rating
	return nil
}

func replyIntrinsics() map[string]agent.Intrinsic {
	return map[string]agent.Intrinsic{
		"@agent/reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil
		}},
	}
}

func TestExecutorRunsRatesAndRecordsOutcome(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{script: []agent.Selection{
		{Action: "@sys/time"},
		{Action: "@agent/reply", Args: map[string]any{"answer": "noon"}},
	}}
	e := &Executor{
		Kernel:  k,
		DataDir: dir,
		Retry:   RetryPolicy{MaxRetries: 0},
		RetryOK: func(error) bool { return false },
		Loop: agent.Config{
			ReplyAction: "@agent/reply", LookupAction: "@sys/lookup",
			AlwaysRefs: []string{"@sys/lookup"}, MaxTurns: 10, Intrinsics: replyIntrinsics(),
		},
		RateTasks: true,
	}
	task := agent.Task{ID: "t1", Channel: "test", Budget: 100,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "what time is it?"}}}

	out, err := e.Execute(context.Background(), task, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Status != agent.StatusAnswered || out.Answer["answer"] != "noon" {
		t.Fatalf("outcome = %+v", out)
	}
	// The dispatched @sys/time call must have been rated.
	if k.rated["tx-@sys/time"] != 1 {
		t.Fatalf("expected @sys/time rated 1, got %v", k.rated)
	}
	// The transcript file must end with a terminal outcome record.
	data, _ := os.ReadFile(filepath.Join(dir, "t1.jsonl"))
	if !strings.Contains(string(data), `"kind":"outcome"`) {
		t.Fatalf("transcript missing outcome record:\n%s", data)
	}
}

func TestExecutorEnqueuesMakerSynthesis(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{}
	maker, _ := NewMaker(dir, MakerConfig{Enabled: true, MinRepeats: 1, Window: 10, Budget: 40, Threshold: 0.5},
		k, nil, nil)
	var enqueued []agent.Task
	e := &Executor{
		Kernel: k, DataDir: dir, RetryOK: func(error) bool { return false },
		Loop: agent.Config{ReplyAction: "@agent/reply", LookupAction: "@sys/lookup",
			MaxTurns: 5, Intrinsics: replyIntrinsics()},
		Maker:   maker,
		Enqueue: func(tk agent.Task) { enqueued = append(enqueued, tk) },
	}
	// min_repeats=1 so a single occurrence triggers synthesis.
	task := agent.Task{ID: "t1", Channel: "http", Budget: 50,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "do a recurring thing"}}}
	if _, err := e.Execute(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	if len(enqueued) != 1 || enqueued[0].Channel != "self" {
		t.Fatalf("expected one self synthesis task, got %+v", enqueued)
	}
}
