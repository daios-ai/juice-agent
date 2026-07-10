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

// publishTool is the maker's transactional publish of a compiled WASM action.
func publishTool() agent.Tool {
	return agent.Tool{
		Name:        "publish",
		Description: "register a compiled WASM action so it becomes reusable",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":          map[string]any{"type": "string", "description": "kebab-case action name"},
				"description":   map[string]any{"type": "string", "description": "what the action does"},
				"artifact":      map[string]any{"type": "string", "description": "base64 WASM artifact from @sys/tinygo/compile"},
				"input_schema":  map[string]any{"type": "object", "description": "the action's input schema"},
				"output_schema": map[string]any{"type": "object", "description": "the action's output schema"},
				"test_inputs":   map[string]any{"type": "array", "description": "distinct inputs to validate before publishing", "items": map[string]any{"type": "object"}},
			},
			"required": []any{"name", "artifact"},
		},
	}
}

// lookupCandidate is @sys/lookup as an always-offered catalog candidate, so the
// agent can always search even before its first lookup.
func lookupCandidate() agent.Candidate {
	return agent.Candidate{
		Ref:         "@sys/lookup",
		Description: "Semantic search over active actions; returns matching @owner/name actions",
		Price:       0,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}},
			"required":   []any{"query"},
		},
	}
}
