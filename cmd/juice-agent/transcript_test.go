package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	tr.Append(context.Background(), agent.Message{Role: agent.RoleAssistant, Action: "sys@k/time"})
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
	tr.Append(context.Background(), agent.Message{Role: agent.RoleAssistant, Action: "sys@k/time"})
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
	child.Append(ctx, agent.Message{Role: agent.RoleAssistant, Action: "sys@k/time"})
	tr.Append(ctx, agent.Message{Role: agent.RoleTool, Action: "subtask", Result: map[string]any{"answer": "x"}})

	// Parent view excludes the bracket's turns.
	for _, m := range tr.Messages() {
		if m.Bracket != "" {
			t.Fatalf("bracket turn leaked into parent view: %+v", m)
		}
	}
	// The durable record has everything, and the bracket turns are tagged.
	all := tr.All()
	if len(all) != 4 { // root, seed(b1), sys@k/time(b1), subtask-rollup
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

// essence is what the model is shown of a record; replay must reproduce it.
func essence(msgs []agent.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Role) + "|" + m.Kind + "|" + m.Action + "|" + m.Content
	}
	return out
}

// runJob appends a job's records as the executor does: a system prompt first,
// then n distinct turns, then an outcome.
func runJob(t *testing.T, dir, persona string, compact int, name string, n int) []agent.Message {
	t.Helper()
	ctx := context.Background()
	tr, err := OpenTranscript(dir, persona, compact, fakeChatter{reply: "summary of " + name})
	if err != nil {
		t.Fatal(err)
	}
	tr.Append(ctx, agent.Message{Role: agent.RoleSystem, Kind: "system", Content: "prompt " + name})
	for i := 0; i < n; i++ {
		tr.Append(ctx, agent.Message{Role: agent.RoleUser, Content: fmt.Sprintf("%s turn %02d with enough text to count", name, i)})
	}
	tr.Append(ctx, agent.Message{Role: agent.RoleSystem, Kind: "outcome", Content: "answered"})
	tr.Close()
	return tr.Messages() // the live view as the job left it, the outcome's own compaction included
}

func TestReopenContinuesThePersonaWithoutJobScopedRecords(t *testing.T) {
	dir := t.TempDir()
	runJob(t, dir, "p", 0, "one", 3)
	tr, err := OpenTranscript(dir, "p", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	got := tr.Messages()
	if tr.Loaded() != 3 || len(got) != 3 || got[0].Content != "one turn 00 with enough text to count" {
		t.Fatalf("loaded %d, view %v; want the three turns only", tr.Loaded(), essence(got))
	}
	if len(tr.All()) != 0 {
		t.Fatalf("All() = %d records at open, want none: earlier jobs are not this job's", len(tr.All()))
	}
	tr.Append(context.Background(), agent.Message{Role: agent.RoleUser, Content: "new"})
	if all := tr.All(); len(all) != 1 || all[0].Content != "new" {
		t.Fatalf("All() = %v, want only this job's record", essence(all))
	}
}

// Compaction writes its summary after the tail it keeps; replay must still
// rebuild exactly what the live view held, across two compacting jobs.
func TestReplayAfterCompactionReproducesTheLiveView(t *testing.T) {
	dir := t.TempDir()
	runJob(t, dir, "p", 300, "one", 12)
	live := runJob(t, dir, "p", 300, "two", 12)
	want := essence(withoutJobScoped(live))

	data, _ := os.ReadFile(filepath.Join(dir, "p.jsonl"))
	if strings.Count(string(data), `"kind":"summary"`) < 2 || !strings.Contains(string(data), `"kept":`) {
		t.Fatalf("expected both jobs to compact and record what they kept:\n%s", data)
	}
	// Reopen with compaction off, so only replay shapes the view.
	tr, err := OpenTranscript(dir, "p", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	got := essence(tr.Messages())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("replayed view differs from the live view\n got: %q\nwant: %q", got, want)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "two turn 11") || !strings.HasPrefix(got[0], "system|summary") {
		t.Fatalf("the kept tail was lost: last = %q", last)
	}
}

// A subtask's records, including a summary its own compaction writes, never
// enter the root view; and a later job's subtask takes a fresh bracket id.
func TestReplayExcludesBracketsAndContinuesTheirNumbering(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tr, _ := OpenTranscript(dir, "p", 200, fakeChatter{reply: "child summary"})
	tr.Append(ctx, agent.Message{Role: agent.RoleUser, Content: "root"})
	child, _ := tr.Child(ctx, nil)
	for i := 0; i < 10; i++ {
		child.Append(ctx, agent.Message{Role: agent.RoleUser, Content: fmt.Sprintf("child turn %d with enough text to compact", i)})
	}
	tr.Close()

	tr2, _ := OpenTranscript(dir, "p", 0, nil)
	if got := essence(tr2.Messages()); len(got) != 1 || !strings.HasSuffix(got[0], "|root") {
		t.Fatalf("root view = %q, want only the root turn", got)
	}
	c2, _ := tr2.Child(ctx, []agent.Message{{Role: agent.RoleUser, Content: "seed"}})
	_ = c2
	if all := tr2.All(); len(all) != 1 || all[0].Bracket != "b2" {
		t.Fatalf("second job's subtask = %+v, want bracket b2", all)
	}
	tr2.Close()
}

func TestLongPersonaNamesAreHashedAndHeaded(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("x", maxPersonaName+1)
	runJob(t, dir, name, 0, "one", 1)
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".jsonl")
	recs, err := readRecords(path)
	if err != nil || len(recs) == 0 || recs[0].Kind != "persona" || recs[0].Content != name {
		t.Fatalf("hashed file %s: first record %+v, %v; want the persona header", path, recs, err)
	}
	tr, _ := OpenTranscript(dir, name, 0, nil)
	defer tr.Close()
	if got := tr.Messages(); len(got) != 1 || got[0].Kind == "persona" {
		t.Fatalf("view = %q, want the one turn and no header", essence(got))
	}
}
