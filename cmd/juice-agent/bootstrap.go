package main

import (
	"context"
	_ "embed"
	"strings"

	"github.com/daios-ai/juice-agent/juice"
)

// replyArtifact is the prebuilt WASM echo action (Handle(in) = (in, nil)),
// compiled once via @sys/tinygo/compile and committed. It backs both intrinsic
// actions: their args are validated and selected by the kernel, but the harness
// intercepts execution, so the artifact's only job is to exist as a valid,
// registrable, schema-bearing action.
//
//go:embed reply.wasm.b64
var replyArtifact string

// replySchema is the terminal answer contract: the model finishes a task by
// selecting @agent/reply with an answer.
func replySchema() (in, out map[string]any) {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string", "description": "the final answer to the task"},
		},
		"required": []any{"answer"},
	}
	return s, s
}

// publishSchema is the @agent/publish contract: the maker's transactional
// publish of a compiled WASM action.
func publishSchema() (in, out map[string]any) {
	in = map[string]any{
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
	}
	out = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "description": "success or failure"},
			"action": map[string]any{"type": "string", "description": "the published action ref, on success"},
		},
	}
	return in, out
}

// ensureAction idempotently registers, enables, and makes public an agent-owned
// WASM action from the embedded artifact. If it already exists it is left as is
// (bootstrap is idempotent, like the kernel's native-action bootstrap).
func ensureAction(ctx context.Context, cl *juice.Client, name string, in, out map[string]any) (string, error) {
	ref := "" // resolved below
	handle := ""
	if self, err := cl.Me(ctx); err == nil {
		handle = self.Handle
	}
	ref = handle + "/" + name

	id, err := cl.ResolveID(ctx, ref)
	if err != nil {
		return ref, err
	}
	if id != "" {
		return ref, nil // already registered
	}
	artifact := strings.TrimSpace(replyArtifact)
	id, createdRef, err := cl.CreateWasmAction(ctx, name, "agent "+name+" action", 0, in, out, artifact)
	if err != nil {
		return ref, err
	}
	if err := cl.EnableAction(ctx, id); err != nil {
		return ref, err
	}
	if err := cl.SetPublic(ctx, id); err != nil {
		return ref, err
	}
	if createdRef != "" {
		ref = createdRef
	}
	return ref, nil
}
