package main

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/daios-ai/juice-agent/agent"
)

// Rater judges and records tool-call quality. Implemented by the job's jobKernel
// (the reasoning affordance to judge, paid from the job's budget; POST /rate to
// record).
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
}

// RateTask grades every dispatched call in a finished transcript and records a
// 0/1 rating with a one-line note for each — the agent's durable, kernel-held
// memory of tool quality. A kernel-failed call is a mechanical 0; successful
// calls are judged together by one structured reasoning call over the transcript. This
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
		calls = append(calls, dispatched{index: len(calls), action: m.Action, txID: m.TxID, isError: m.IsError})
	}
	if len(calls) == 0 {
		return
	}

	// Mechanical 0 for failed calls; collect the rest for the judge. The note is
	// public (anyone who can read the action lists it) and an error text can echo
	// the call's arguments, so a failure's note is fixed.
	var toJudge []dispatched
	for _, c := range calls {
		if c.isError {
			if err := r.Rate(ctx, c.txID, 0, "call failed"); err != nil {
				log("rate_error", map[string]any{"tx_id": c.txID, "error": err.Error()})
			}
			continue
		}
		toJudge = append(toJudge, c)
	}
	if len(toJudge) == 0 {
		return
	}

	judgments, err := judge(ctx, messages, toJudge, r)
	if err != nil {
		// Unjudged calls stay unrated; a judge the job's budget cannot pay is skipped.
		log("judge_error", map[string]any{"error": err.Error()})
	}
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
// index -> judgment; on a failure it returns an empty map (calls stay unrated)
// and the error.
func judge(ctx context.Context, messages []agent.Message, calls []dispatched, r Rater) (map[int]judgment, error) {
	prompt := "You are grading how well each tool call below served the task, judging in hindsight " +
		"from the full transcript. For each call return rating 1 (helpful, correct, worth reusing) or " +
		"0 (unhelpful, wrong, or wasteful) and a one-line note. " + notePolicy + " Calls to grade:\n"
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
						"note":   map[string]any{"type": "string", "description": "one line on the tool's performance only; no request content, no names or addresses"},
					},
					"required": []any{"index", "rating", "note"},
				},
			},
		},
		"required": []any{"ratings"},
	}
	val, err := r.ChatJSON(ctx, msgs, schema)
	if err != nil {
		return map[int]judgment{}, err
	}
	return parseJudgments(val), nil
}

// notePolicy tells the judge that its notes are public. It reduces the risk of
// disclosure; it cannot guarantee it, since the model's compliance is unverified.
const notePolicy = "Each note is published with its rating, readable by anyone. Describe only how the " +
	"tool performed: whether its result was correct, useful, or wasteful. Never quote or paraphrase the " +
	"request, the tool's input or output, or the agent's instructions, and never name or address any " +
	"person, account, room, or conversation."

// parseJudgments keeps only well-formed judgments: an integral index, a rating of
// exactly 0 or 1, and a string note. A rating is permanent (kernel D11), so a
// malformed one leaves its call unrated rather than recording false evidence.
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
		idx, okIdx := toInt(m["index"])
		rating, okRating := toInt(m["rating"])
		note, okNote := m["note"].(string)
		if !okIdx || !okRating || !okNote || (rating != 0 && rating != 1) {
			continue
		}
		out[idx] = judgment{rating: rating, note: note}
	}
	return out
}

// explainFailure asks the model, once, to account for a job that failed: what it
// attempted, what went wrong, and what would be needed. The text is delivered to
// the requester in place of an answer. It runs after the loop, in the policy
// layer, and is paid from the job's budget like the rating pass; a call the
// budget cannot pay, or that fails, leaves the explanation empty and the job
// delivers its reason code alone.
func explainFailure(ctx context.Context, messages []agent.Message, reason string, c Chatter, log func(string, map[string]any)) string {
	if log == nil {
		log = func(string, map[string]any) {}
	}
	ask := agent.Message{Role: agent.RoleUser, Content: "You could not complete this task (reason: " + reason +
		"). In two or three sentences, tell the requester what you attempted, what went wrong, and what " +
		"would be needed to succeed. Do not select a tool; reply in plain text."}
	text, err := c.Chat(ctx, append(append([]agent.Message{}, messages...), ask))
	if err != nil {
		log("explain_error", map[string]any{"reason": reason, "error": err.Error()})
		return ""
	}
	return strings.TrimSpace(text)
}

// toInt reads a JSON number as an int, refusing a fractional one.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == math.Trunc(n)
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
