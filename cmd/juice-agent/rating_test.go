package main

import (
	"context"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

type fakeRater struct {
	judgment any
	asked    []agent.Message // what the judge was sent
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

func (r *fakeRater) ChatJSON(_ context.Context, msgs []agent.Message, _ map[string]any) (any, error) {
	r.asked = msgs
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
		{Role: agent.RoleTool, Action: "bad/thing", TxID: "tx-bad", IsError: true, Content: "no account alice@k"},
	}
	r := newFakeRater(map[string]any{"ratings": []any{}})
	RateTask(context.Background(), msgs, r, nil, nil)
	// The note is public, so the error text, which can name a person, stays out.
	if got := r.rated["tx-bad"]; got.rating != 0 || got.note != "call failed" {
		t.Fatalf("failed call rated %+v, want 0 with the fixed note", got)
	}
}

// The judge is told its notes are public.
func TestJudgeIsToldNotesArePublic(t *testing.T) {
	r := newFakeRater(map[string]any{"ratings": []any{}})
	RateTask(context.Background(), []agent.Message{{Role: agent.RoleTool, Action: "sys@k/time", TxID: "tx-0"}}, r, nil, nil)
	if len(r.asked) == 0 || !strings.Contains(r.asked[len(r.asked)-1].Content, notePolicy) {
		t.Fatal("the judge prompt must carry the note policy")
	}
}

// A malformed judgment leaves its call unrated; a valid one beside it is kept.
func TestRateTaskSkipsMalformedJudgments(t *testing.T) {
	for _, bad := range []map[string]any{
		{"index": float64(1), "note": "no rating"},
		{"index": float64(1), "rating": 0.7, "note": "fractional"},
		{"index": float64(1), "rating": float64(2), "note": "out of range"},
		{"index": float64(1), "rating": float64(1), "note": 5},
		{"index": 0.5, "rating": float64(1), "note": "fractional index"},
	} {
		msgs := []agent.Message{
			{Role: agent.RoleTool, Action: "sys@k/time", TxID: "tx-0"},
			{Role: agent.RoleTool, Action: "sys@k/web", TxID: "tx-1"},
		}
		r := newFakeRater(map[string]any{"ratings": []any{
			map[string]any{"index": float64(0), "rating": float64(1), "note": "fine"}, bad}})
		RateTask(context.Background(), msgs, r, nil, nil)
		if _, rated := r.rated["tx-1"]; rated || r.rated["tx-0"].rating != 1 {
			t.Errorf("judgment %v: rated=%v, want tx-1 unrated and tx-0 rated 1", bad, r.rated)
		}
	}
}

func TestRateTaskJudgesSuccessfulCalls(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleTool, Action: "sys@k/time", TxID: "tx-0"},
		{Role: agent.RoleTool, Action: "sys@k/web", TxID: "tx-1"},
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
		{Role: agent.RoleTool, Action: "reply", TxID: "tx-reply"},
	}
	r := newFakeRater(map[string]any{"ratings": []any{}})
	RateTask(context.Background(), msgs, r, map[string]agent.Intrinsic{"reply": {}}, nil)
	if _, ok := r.rated["tx-reply"]; ok {
		t.Fatal("intrinsic call should not be rated")
	}
}
