package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestSystemPromptIncludesAgentFile(t *testing.T) {
	msg := systemPrompt("research@k", "reply", "Be terse.\nJuice actions are owner@kernel/name.")
	if msg.Role != agent.RoleSystem || msg.Kind != "system" {
		t.Fatalf("role/kind = %q/%q", msg.Role, msg.Kind)
	}
	for _, want := range []string{"research@k", `"reply"`, "# Agent", "Be terse.", "owner@kernel/name", "# Operating"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg.Content)
		}
	}
}

func TestSystemPromptOmitsEmptyAgentFile(t *testing.T) {
	msg := systemPrompt("a@k", "reply", "  \n")
	if strings.Contains(msg.Content, "# Agent") || !strings.Contains(msg.Content, "# Operating") {
		t.Fatalf("an empty agent.md must add no section:\n%s", msg.Content)
	}
}

func TestReadFileOrEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.md")
	if got := readFileOrEmpty(p); got != "" {
		t.Fatalf("missing file = %q, want empty", got)
	}
	os.WriteFile(p, []byte("speak like a pirate"), 0o600)
	if got := readFileOrEmpty(p); got != "speak like a pirate" {
		t.Fatalf("read = %q", got)
	}
}
