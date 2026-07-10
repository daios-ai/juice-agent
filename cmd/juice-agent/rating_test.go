package main

import (
	"context"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

type fakeRater struct {
	judgment any
	rated    map[string]struct {
		rating int
		note   string
	}
}

func newFakeRater(j any) *fakeRater {
	return &fakeRater{judgment: j, rated: map[string]struct {
		rating int
		note   string
	}{}}
}

func (r *fakeRater) ChatJSON(_ context.Context, _ []agent.Message, _ map[string]any) (any, error) {
	return r.judgment, nil
}
func (r *fakeRater) Rate(_ context.Context, txID string, rating int, note string) error {
	r.rated[txID] = struct {
		rating int
		note   string
	}{rating, note}
	return nil
}

func TestRateTaskMechanicalZeroForFailures(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleTool, Action: "@bad/thing", TxID: "tx-bad", IsError: true, Content: "boom"},
	}
	r := newFakeRater(map[string]any{"ratings": []any{}})
	RateTask(context.Background(), msgs, r, nil, nil)
	got := r.rated["tx-bad"]
	if got.rating != 0 {
		t.Fatalf("failed call rating = %d, want 0", got.rating)
	}
	if got.note == "" {
		t.Fatal("expected a note on the failed call")
	}
}

func TestRateTaskJudgesSuccessfulCalls(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleTool, Action: "@sys/time", TxID: "tx-0"},
		{Role: agent.RoleTool, Action: "@sys/web", TxID: "tx-1"},
	}
	judgment := map[string]any{"ratings": []any{
		map[string]any{"index": float64(0), "rating": float64(1), "note": "gave the time"},
		map[string]any{"index": float64(1), "rating": float64(0), "note": "wrong page"},
	}}
	r := newFakeRater(judgment)
	RateTask(context.Background(), msgs, r, nil, nil)

	if r.rated["tx-0"].rating != 1 || r.rated["tx-0"].note != "gave the time" {
		t.Fatalf("tx-0 = %+v", r.rated["tx-0"])
	}
	if r.rated["tx-1"].rating != 0 {
		t.Fatalf("tx-1 rating = %d, want 0", r.rated["tx-1"].rating)
	}
}

func TestRateTaskSkipsIntrinsics(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleTool, Action: "@agent/reply", TxID: "tx-reply"},
	}
	r := newFakeRater(map[string]any{"ratings": []any{}})
	RateTask(context.Background(), msgs, r, map[string]agent.Intrinsic{"@agent/reply": {}}, nil)
	if _, ok := r.rated["tx-reply"]; ok {
		t.Fatal("intrinsic call should not be rated")
	}
}
