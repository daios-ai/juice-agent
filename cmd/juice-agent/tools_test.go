package main

import (
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestHarnessToolsArePlainNamedAndRequireTheirKeys(t *testing.T) {
	for _, tc := range []struct {
		tool agent.Tool
		key  string
	}{{replyTool(), "answer"}, {subtaskTool(), "instruction"}} {
		if strings.Contains(tc.tool.Name, "/") {
			t.Fatalf("harness tool %q must not look like an action address", tc.tool.Name)
		}
		if missingKey(tc.tool.InputSchema, tc.key) {
			t.Fatalf("%s does not require %q", tc.tool.Name, tc.key)
		}
	}
}

func missingKey(schema map[string]any, key string) bool {
	req, _ := schema["required"].([]any)
	for _, r := range req {
		if r == key {
			return false
		}
	}
	return true
}
