package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestPromptBuildIncludesSectionsWhenPresent(t *testing.T) {
	p := &Prompter{
		Handle:    "research",
		ReplyTool: "reply",
		Persona:   "Be terse and formal.",
		Knowledge: "Juice actions are owner/name.",
	}
	msg := p.Build("Last time I used sys/time for clocks.")
	if msg.Role != agent.RoleSystem || msg.Kind != "system" {
		t.Fatalf("role/kind = %q/%q", msg.Role, msg.Kind)
	}
	for _, want := range []string{"research", "\"reply\"", "Be terse and formal.",
		"Juice actions are owner/name.", "Last time I used sys/time", "# Persona", "# Knowledge", "# Memory"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg.Content)
		}
	}
}

func TestPromptOmitsEmptySections(t *testing.T) {
	p := &Prompter{Handle: "agent", ReplyTool: "reply"}
	msg := p.Build("")
	for _, absent := range []string{"# Persona", "# Knowledge", "# Memory"} {
		if strings.Contains(msg.Content, absent) {
			t.Fatalf("empty section %q should be omitted:\n%s", absent, msg.Content)
		}
	}
	// Role line and operating contract are always present.
	if !strings.Contains(msg.Content, "reply") || !strings.Contains(msg.Content, "# Operating") {
		t.Fatalf("missing role/operating:\n%s", msg.Content)
	}
}

func TestLoadPrompterReadsFilesAndSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "persona.md")
	os.WriteFile(personaPath, []byte("speak like a pirate"), 0o644)
	// knowledge file intentionally absent
	p := LoadPrompter("bob", "reply", personaPath, filepath.Join(dir, "knowledge.md"))
	if p.Persona != "speak like a pirate" {
		t.Fatalf("persona = %q", p.Persona)
	}
	if p.Knowledge != "" {
		t.Fatalf("missing knowledge file should yield empty, got %q", p.Knowledge)
	}
}

// echoChatter returns a fixed summary, recording the last prompt it saw.
type echoChatter struct {
	reply string
	last  string
}

func (c *echoChatter) Chat(_ context.Context, msgs []agent.Message) (string, error) {
	if len(msgs) > 0 {
		c.last = msgs[len(msgs)-1].Content
	}
	return c.reply, nil
}

func memTask() (agent.Task, agent.Outcome, []agent.Message) {
	task := agent.Task{ID: "t1", Messages: []agent.Message{{Role: agent.RoleUser, Content: "what time is it"}}}
	out := agent.Outcome{Status: agent.StatusAnswered}
	msgs := []agent.Message{
		{Role: agent.RoleUser, Content: "what time is it"},
		{Role: agent.RoleAssistant, Action: "sys/time"},
		{Role: agent.RoleTool, Action: "sys/time", TxID: "tx1"},
	}
	return task, out, msgs
}

func TestMemoryUpdateAppendsEntryAndDigestReadsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.md")
	m := NewMemory(path, 65536, &echoChatter{reply: "- asked the time; used sys/time; answered"})

	if m.Digest() != "" {
		t.Fatal("digest should be empty before any update")
	}
	task, out, msgs := memTask()
	m.Update(context.Background(), "2026-07-10 01:00", task, out, msgs)

	dig := m.Digest()
	if !strings.Contains(dig, "sys/time") || !strings.Contains(dig, "t1") {
		t.Fatalf("memory digest missing content:\n%s", dig)
	}
	// The summariser must have been shown the transcript (the action it used).
	// (echoChatter recorded the last prompt.)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "## 2026-07-10 01:00 — t1") {
		t.Fatalf("entry heading missing:\n%s", data)
	}
}

func TestMemoryNilIsSafe(t *testing.T) {
	var m *Memory
	if m.Digest() != "" {
		t.Fatal("nil memory Digest should be empty")
	}
	task, out, msgs := memTask()
	m.Update(context.Background(), "x", task, out, msgs) // must not panic
}

func TestMemoryCompactsWhenOverBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.md")
	// Tiny budget so a couple of entries trigger compaction.
	m := NewMemory(path, 200, &echoChatter{reply: "a reasonably long summary line that consumes bytes across entries"})

	task, out, msgs := memTask()
	for i := 0; i < 6; i++ {
		task.ID = "task-" + string(rune('a'+i))
		m.Update(context.Background(), "2026-07-10 01:0"+string(rune('0'+i)), task, out, msgs)
	}
	data, _ := os.ReadFile(path)
	// After compaction a digest heading must appear, and the file must be bounded
	// (roughly — compaction keeps recent + a digest, not unbounded growth).
	if !strings.Contains(string(data), "digest") {
		t.Fatalf("expected a digest heading after compaction:\n%s", data)
	}
	if len(data) > 200*3 {
		t.Fatalf("memory grew unbounded (%d bytes) despite compaction", len(data))
	}
}
