package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
)

// Transcript is a durable, append-only JSONL record of one task and the working
// memory sent to the model. It persists every record as produced (crash
// survivable) and compacts older turns into an LLM summary when the in-memory
// view grows past a byte budget — the kernel reports no token count, so bytes
// are the legible proxy. It implements agent.Transcript.
type Transcript struct {
	path         string
	chatter      Chatter
	compactBytes int

	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	msgs   []agent.Message // current (possibly compacted) view
	nbytes int             // approximate size of msgs
}

// TerminalKinds are the record kinds that mark a transcript as finished; their
// presence as the last record means the task was not interrupted.
var TerminalKinds = map[string]bool{"outcome": true, "interrupted": true}

// OpenTranscript creates (or truncates) the JSONL file for taskID under dir.
// compactBytes ≤ 0 disables compaction. chatter may be nil when compaction is
// off.
func OpenTranscript(dir, taskID string, compactBytes int, chatter Chatter) (*Transcript, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, taskID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Transcript{path: path, chatter: chatter, compactBytes: compactBytes, f: f, w: bufio.NewWriter(f)}, nil
}

// Append persists m and adds it to the working view, compacting if the view has
// grown past the byte budget.
func (t *Transcript) Append(ctx context.Context, m agent.Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.write(m); err != nil {
		return err
	}
	t.msgs = append(t.msgs, m)
	t.nbytes += size(m)
	if t.compactBytes > 0 && t.nbytes > t.compactBytes && t.chatter != nil {
		t.compact(ctx)
	}
	return nil
}

func (t *Transcript) write(m agent.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := t.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return t.w.Flush()
}

// Messages returns a copy of the current working view.
func (t *Transcript) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]agent.Message, len(t.msgs))
	copy(out, t.msgs)
	return out
}

// Close flushes and closes the underlying file.
func (t *Transcript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w != nil {
		t.w.Flush()
	}
	if t.f != nil {
		return t.f.Close()
	}
	return nil
}

// compact replaces the older half of the view with a single summary message,
// keeping a recent tail verbatim and cutting only at a turn boundary. The full
// history stays on disk; only the in-memory view shrinks. Must hold t.mu.
func (t *Transcript) compact(ctx context.Context) {
	keep := t.compactBytes / 2
	// find the cut: walk from the end accumulating bytes until we've kept ~keep.
	cut := len(t.msgs)
	acc := 0
	for i := len(t.msgs) - 1; i >= 0; i-- {
		acc += size(t.msgs[i])
		if acc >= keep {
			cut = i
			break
		}
	}
	if cut <= 0 {
		return // nothing old enough to summarise
	}
	head, tail := t.msgs[:cut], t.msgs[cut:]
	summary, err := t.chatter.Chat(ctx, summaryRequest(head))
	if err != nil || strings.TrimSpace(summary) == "" {
		return // compaction is best-effort; keep the full view on failure
	}
	sm := agent.Message{Role: agent.RoleSystem, Kind: "summary", Content: summary}
	_ = t.write(sm) // record the compaction in the durable log
	t.msgs = append([]agent.Message{sm}, tail...)
	t.nbytes = size(sm)
	for _, m := range tail {
		t.nbytes += size(m)
	}
}

// summaryRequest builds the compaction prompt: pi's fixed 7-section template
// over the head messages being condensed.
func summaryRequest(head []agent.Message) []agent.Message {
	var b strings.Builder
	b.WriteString("Summarise the conversation so far into the sections below, preserving exact " +
		"action names, arguments, results, and error messages. Be terse.\n\n" +
		"## Goal\n## Constraints & Preferences\n## Progress\n### Done\n### In Progress\n### Blocked\n" +
		"## Key Decisions\n## Next Steps\n## Critical Context\n\nConversation:\n")
	for _, m := range head {
		fmt.Fprintf(&b, "[%s] %s", m.Role, m.Content)
		if m.Action != "" {
			fmt.Fprintf(&b, " action=%s", m.Action)
		}
		b.WriteByte('\n')
	}
	return []agent.Message{{Role: agent.RoleUser, Content: b.String()}}
}

func size(m agent.Message) int {
	n := len(m.Content) + len(m.Action) + len(string(m.Role)) + len(m.Kind)
	if b, err := json.Marshal(m.Args); err == nil {
		n += len(b)
	}
	if b, err := json.Marshal(m.Result); err == nil {
		n += len(b)
	}
	return n
}

// Recover scans dir for task transcripts whose last record is not terminal —
// tasks that were mid-flight at shutdown — and appends an "interrupted" marker
// to each, the agent-side mirror of the kernel's interrupted-trace recovery. It
// is idempotent: an already-terminal transcript is left untouched. Returns the
// ids of the transcripts it marked.
func Recover(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var marked []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		last, err := lastRecord(path)
		if err != nil {
			continue
		}
		if last == nil || TerminalKinds[last.Kind] {
			continue
		}
		if err := appendMarker(path, agent.Message{Role: agent.RoleSystem, Kind: "interrupted",
			Content: "task interrupted by shutdown; not resumed"}); err == nil {
			marked = append(marked, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	return marked, nil
}

func lastRecord(path string) (*agent.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last *agent.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m agent.Message
		if json.Unmarshal([]byte(line), &m) == nil {
			mm := m
			last = &mm
		}
	}
	return last, sc.Err()
}

func appendMarker(path string, m agent.Message) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	_, err = f.Write(append(b, '\n'))
	return err
}
