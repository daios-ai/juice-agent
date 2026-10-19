package main

import "github.com/daios-ai/juice-agent/agent"

// Harness tools are plain-named affordances the daemon offers the model and
// intercepts itself — they are NOT registered on the platform, so the catalog
// stays a truthful registry of real capabilities. Each has a schema only to
// standardise the args the model must supply.

// replyTool is the terminal answer contract: the model finishes a task by
// selecting "reply" with its answer.
func replyTool() agent.Tool {
	return agent.Tool{
		Name:        "reply",
		Description: "finish the task and deliver your final answer",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string", "description": "the final answer to the task"}},
			"required":   []any{"answer"},
		},
	}
}

// subtaskTool opens a bracket: a focused sub-loop that inherits context and an
// instruction, works in isolation, and rolls up a single result (§4b).
func subtaskTool() agent.Tool {
	return agent.Tool{
		Name:        "subtask",
		Description: "delegate a focused sub-problem to an isolated sub-loop; only its result returns to this chat",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"instruction": map[string]any{"type": "string", "description": "what the subtask must accomplish"},
				"context":     map[string]any{"type": "string", "description": "optional facts the subtask needs from here"},
			},
			"required": []any{"instruction"},
		},
	}
}
