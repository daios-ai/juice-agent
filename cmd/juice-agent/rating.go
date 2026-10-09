package main

import (
	"context"
	"strconv"

	"github.com/daios-ai/juice-agent/agent"
)

// Rater judges and records tool-call quality. Implemented by the juice client
// (llm/json to judge, POST /rate to record).
type Rater interface {
	ChatJSON(ctx context.Context, messages []agent.Message, schema map[string]any) (any, error)
	Rate(ctx context.Context, txID string, rating int, note string) error
}

// dispatched is one real (non-intrinsic) tool call the agent made and owns.
type dispatched struct {
	index   int
	action  string
	txID    string
	isError bool
	errText string
}

// RateTask grades every dispatched call in a finished transcript and records a
// 0/1 rating with a one-line note for each — the agent's durable, kernel-held
// memory of tool quality. A kernel-failed call is a mechanical 0; successful
// calls are judged together by one llm/json call over the transcript. This
// runs after the loop, never inside it (the kernel law: execution must not
// rate). Rating is best-effort: a judging failure leaves calls unrated rather
// than failing the task.
func RateTask(ctx context.Context, messages []agent.Message, r Rater, intrinsics map[string]agent.Intrinsic, log func(string, map[string]any)) {
	if log == nil {
		log = func(string, map[string]any) {}
	}
	var calls []dispatched
	for _, m := range messages {
		if m.Role != agent.RoleTool || m.Action == "" || m.TxID == "" {
			continue
		}
		if _, isIntrinsic := intrinsics[m.Action]; isIntrinsic {
			continue
		}
		calls = append(calls, dispatched{index: len(calls), action: m.Action, txID: m.TxID, isError: m.IsError, errText: m.Content})
	}
	if len(calls) == 0 {
		return
	}

	// Mechanical 0 for failed calls; collect the rest for the judge.
	var toJudge []dispatched
	for _, c := range calls {
		if c.isError {
			note := "call failed"
			if c.errText != "" {
				note = "call failed: " + trunc(c.errText, 120)
			}
			if err := r.Rate(ctx, c.txID, 0, note); err != nil {
				log("rate_error", map[string]any{"tx_id": c.txID, "error": err.Error()})
			}
			continue
		}
		toJudge = append(toJudge, c)
	}
	if len(toJudge) == 0 {
		return
	}

	judgments := judge(ctx, messages, toJudge, r)
	for _, c := range toJudge {
		j, ok := judgments[c.index]
		if !ok {
			continue // judge did not cover it; leave unrated
		}
		if err := r.Rate(ctx, c.txID, j.rating, j.note); err != nil {
			log("rate_error", map[string]any{"tx_id": c.txID, "error": err.Error()})
		}
	}
}

type judgment struct {
	rating int
	note   string
}

// judge asks the LLM to grade each dispatched call by index. It returns a map
// index -> judgment; on any failure it returns an empty map (calls stay
// unrated).
func judge(ctx context.Context, messages []agent.Message, calls []dispatched, r Rater) map[int]judgment {
	prompt := "You are grading how well each tool call below served the task, judging in hindsight " +
		"from the full transcript. For each call return rating 1 (helpful, correct, worth reusing) or " +
		"0 (unhelpful, wrong, or wasteful) and a one-line note. Calls to grade:\n"
	for _, c := range calls {
		prompt += "  index=" + strconv.Itoa(c.index) + " action=" + c.action + "\n"
	}
	msgs := append(append([]agent.Message{}, messages...), agent.Message{Role: agent.RoleUser, Content: prompt})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ratings": map[string]any{
				"type":        "array",
				"description": "one judgment per graded call",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"index":  map[string]any{"type": "integer", "description": "the call index"},
						"rating": map[string]any{"type": "integer", "description": "0 or 1"},
						"note":   map[string]any{"type": "string", "description": "one-line justification"},
					},
					"required": []any{"index", "rating", "note"},
				},
			},
		},
		"required": []any{"ratings"},
	}
	val, err := r.ChatJSON(ctx, msgs, schema)
	if err != nil {
		return map[int]judgment{}
	}
	return parseJudgments(val)
}

func parseJudgments(val any) map[int]judgment {
	out := map[int]judgment{}
	obj, ok := val.(map[string]any)
	if !ok {
		return out
	}
	arr, ok := obj["ratings"].([]any)
	if !ok {
		return out
	}
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		idx, ok := toInt(m["index"])
		if !ok {
			continue
		}
		rating, _ := toInt(m["rating"])
		if rating != 0 && rating != 1 {
			rating = 0
		}
		note, _ := m["note"].(string)
		out[idx] = judgment{rating: rating, note: note}
	}
	return out
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
