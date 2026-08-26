package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

type fakeChatter struct{ reply string }

func (c fakeChatter) Chat(_ context.Context, _ []agent.Message) (string, error) {
	return c.reply, nil
}

func TestTranscriptPersistsAndReturnsMessages(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir, "t1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr.Append(context.Background(), agent.Message{Role: agent.RoleUser, Content: "hello"})
	tr.Append(context.Background(), agent.Message{Role: agent.RoleAssistant, Action: "sys/time"})
	if got := tr.Messages(); len(got) != 2 {
		t.Fatalf("messages = %d, want 2", len(got))
	}
	tr.Close()

	// The JSONL file must contain both records, one per line.
	data, _ := os.ReadFile(filepath.Join(dir, "t1.jsonl"))
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 2 {
		t.Fatalf("file has %d lines, want 2:\n%s", lines, data)
	}
}

func TestCompactionReplacesOldTurnsWithSummary(t *testing.T) {
	dir := t.TempDir()
	// Small byte budget so a few messages trigger compaction.
	tr, err := OpenTranscript(dir, "t2", 200, fakeChatter{reply: "## Goal\nsummarised"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		tr.Append(context.Background(), agent.Message{Role: agent.RoleUser,
			Content: "a fairly long message number that adds up in bytes over many turns"})
	}
	msgs := tr.Messages()
	// After compaction the in-memory view must be shorter than 20 and contain a
	// summary record.
	if len(msgs) >= 20 {
		t.Fatalf("view not compacted: %d messages", len(msgs))
	}
	foundSummary := false
	for _, m := range msgs {
		if m.Kind == "summary" {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatal("no summary message after compaction")
	}
	tr.Close()
}

func TestRecoverMarksInterruptedIdempotently(t *testing.T) {
	dir := t.TempDir()
	// t3: mid-flight (last record is a tool call, not terminal).
	tr, _ := OpenTranscript(dir, "t3", 0, nil)
	tr.Append(context.Background(), agent.Message{Role: agent.RoleUser, Content: "do it"})
	tr.Append(context.Background(), agent.Message{Role: agent.RoleAssistant, Action: "sys/time"})
	tr.Close()

	// t4: finished (terminal outcome record).
	tr2, _ := OpenTranscript(dir, "t4", 0, nil)
	tr2.Append(context.Background(), agent.Message{Role: agent.RoleUser, Content: "done"})
	tr2.Append(context.Background(), agent.Message{Role: agent.RoleSystem, Kind: "outcome", Content: "answered"})
	tr2.Close()

	marked, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 1 || marked[0] != "t3" {
		t.Fatalf("marked = %v, want [t3]", marked)
	}
	last, _ := lastRecord(filepath.Join(dir, "t3.jsonl"))
	if last == nil || last.Kind != "interrupted" {
		t.Fatalf("t3 last record = %+v, want interrupted", last)
	}

	// Second run: t3 is now terminal (interrupted); nothing new marked.
	marked2, _ := Recover(dir)
	if len(marked2) != 0 {
		t.Fatalf("second Recover marked %v, want none (idempotent)", marked2)
	}
}

func TestTranscriptChildIsolatesViewButSharesRecord(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir, "tb", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr.Append(ctx, agent.Message{Role: agent.RoleUser, Content: "root"})
	child, err := tr.Child(ctx, []agent.Message{{Role: agent.RoleUser, Content: "seed"}})
	if err != nil {
		t.Fatal(err)
	}
	child.Append(ctx, agent.Message{Role: agent.RoleAssistant, Action: "sys/time"})
	tr.Append(ctx, agent.Message{Role: agent.RoleTool, Action: "subtask", Result: map[string]any{"answer": "x"}})

	// Parent view excludes the bracket's turns.
	for _, m := range tr.Messages() {
		if m.Bracket != "" {
			t.Fatalf("bracket turn leaked into parent view: %+v", m)
		}
	}
	// The durable record has everything, and the bracket turns are tagged.
	all := tr.All()
	if len(all) != 4 { // root, seed(b1), sys/time(b1), subtask-rollup
		t.Fatalf("All() = %d records, want 4", len(all))
	}
	tagged := 0
	for _, m := range all {
		if m.Bracket == "b1" {
			tagged++
		}
	}
	if tagged != 2 {
		t.Fatalf("bracket-tagged records = %d, want 2", tagged)
	}
	tr.Close()
}
