package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestPromptBuildIncludesSectionsWhenPresent(t *testing.T) {
	p := &Prompter{
		Handle:      "@research",
		ReplyAction: "@research/reply",
		Persona:     "Be terse and formal.",
		Knowledge:   "Juice actions are @owner/name.",
	}
	msg := p.Build("Last time I used @sys/time for clocks.")
	if msg.Role != agent.RoleSystem || msg.Kind != "system" {
		t.Fatalf("role/kind = %q/%q", msg.Role, msg.Kind)
	}
	for _, want := range []string{"@research", "@research/reply", "Be terse and formal.",
		"Juice actions are @owner/name.", "Last time I used @sys/time", "# Persona", "# Knowledge", "# Memory"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg.Content)
		}
	}
}

func TestPromptOmitsEmptySections(t *testing.T) {
	p := &Prompter{Handle: "@agent", ReplyAction: "@agent/reply"}
	msg := p.Build("")
	for _, absent := range []string{"# Persona", "# Knowledge", "# Memory"} {
		if strings.Contains(msg.Content, absent) {
			t.Fatalf("empty section %q should be omitted:\n%s", absent, msg.Content)
		}
	}
	// Role line and operating contract are always present.
	if !strings.Contains(msg.Content, "@agent/reply") || !strings.Contains(msg.Content, "# Operating") {
		t.Fatalf("missing role/operating:\n%s", msg.Content)
	}
}

func TestLoadPrompterReadsFilesAndSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "persona.md")
	os.WriteFile(personaPath, []byte("speak like a pirate"), 0o644)
	// knowledge file intentionally absent
	p := LoadPrompter("@bob", "@bob/reply", personaPath, filepath.Join(dir, "knowledge.md"))
	if p.Persona != "speak like a pirate" {
		t.Fatalf("persona = %q", p.Persona)
	}
	if p.Knowledge != "" {
		t.Fatalf("missing knowledge file should yield empty, got %q", p.Knowledge)
	}
}
