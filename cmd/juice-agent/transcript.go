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

// record is the shared durable store for one task: the JSONL file, the full
// in-memory record across all frames, and a bracket-id counter. A task's root
// Transcript and every bracket frame share one record.
type record struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	all []agent.Message
	seq int
}

func (r *record) write(m agent.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		return err
	}
	r.all = append(r.all, m)
	return r.w.Flush()
}

func (r *record) snapshot() []agent.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agent.Message, len(r.all))
	copy(out, r.all)
	return out
}

func (r *record) nextBracket() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return fmt.Sprintf("b%d", r.seq)
}

// Transcript is one frame's view over a shared record (§4b). Append persists a
// record (tagged with this frame's bracket) and adds it to the frame's view,
// compacting the view when it grows past a byte budget. Child opens a nested
// bracket frame; All returns the whole durable record. It implements
// agent.Transcript.
type Transcript struct {
	rec          *record
	bracket      string
	chatter      Chatter
	compactBytes int
	msgs         []agent.Message // this frame's (possibly compacted) view
	nbytes       int
}

// TerminalKinds are the record kinds that mark a transcript as finished; their
// presence as the last record means the task was not interrupted.
var TerminalKinds = map[string]bool{"outcome": true, "interrupted": true}

// OpenTranscript creates (or truncates) the JSONL file for taskID under dir and
// returns its root frame. compactBytes ≤ 0 disables compaction.
func OpenTranscript(dir, taskID string, compactBytes int, chatter Chatter) (*Transcript, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, taskID+".jsonl"))
	if err != nil {
		return nil, err
	}
	return &Transcript{rec: &record{f: f, w: bufio.NewWriter(f)}, chatter: chatter, compactBytes: compactBytes}, nil
}

// Append persists m (tagged with this frame's bracket) and adds it to the view,
// compacting if the view has grown past the byte budget.
func (t *Transcript) Append(ctx context.Context, m agent.Message) error {
	m.Bracket = t.bracket
	if err := t.rec.write(m); err != nil {
		return err
	}
	t.msgs = append(t.msgs, m)
	t.nbytes += size(m)
	if t.compactBytes > 0 && t.nbytes > t.compactBytes && t.chatter != nil {
		t.compact(ctx)
	}
	return nil
}

// Messages returns a copy of this frame's working view.
func (t *Transcript) Messages() []agent.Message {
	out := make([]agent.Message, len(t.msgs))
	copy(out, t.msgs)
	return out
}

// All returns the whole durable record across every frame (for rating and
// memory, which must see bracketed calls too).
func (t *Transcript) All() []agent.Message { return t.rec.snapshot() }

// Child opens a nested bracket frame sharing this task's durable record: a fresh
// view seeded with `seed`, tagged with a new bracket id.
func (t *Transcript) Child(ctx context.Context, seed []agent.Message) (agent.Transcript, error) {
	child := &Transcript{rec: t.rec, bracket: t.rec.nextBracket(), chatter: t.chatter, compactBytes: t.compactBytes}
	for _, m := range seed {
		if err := child.Append(ctx, m); err != nil {
			return nil, err
		}
	}
	return child, nil
}

// Close flushes and closes the underlying file.
func (t *Transcript) Close() error {
	t.rec.mu.Lock()
	defer t.rec.mu.Unlock()
	if t.rec.w != nil {
		t.rec.w.Flush()
	}
	if t.rec.f != nil {
		return t.rec.f.Close()
	}
	return nil
}

// compact replaces the older half of the view with a single summary message,
// keeping a recent tail verbatim and cutting only at a turn boundary. The full
// history stays on disk; only the in-memory view shrinks.
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
	_ = t.rec.write(sm) // record the compaction in the durable log
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
