package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
)

// record is the shared durable store for one job: the persona's JSONL file, the
// records this job wrote across all frames, and a bracket-id counter that
// continues the persona's. A job's root Transcript and every bracket frame share
// one record.
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
// bracket frame; All returns the records this job wrote. It implements
// agent.Transcript.
type Transcript struct {
	rec          *record
	bracket      string
	chatter      Chatter
	compactBytes int
	msgs         []agent.Message // this frame's (possibly compacted) view
	nbytes       int
	loaded       int // records the root view carried in from earlier jobs
}

// TerminalKinds are the record kinds that mark a job as finished; their
// presence as the last record means the job was not interrupted.
var TerminalKinds = map[string]bool{"outcome": true, "interrupted": true}

// jobScoped are the record kinds that belong to one job and are dropped from a
// persona's view when the next job begins: the job's system prompt, its outcome,
// an interruption marker, and the persona-name header. Every other root record
// (the exchange itself, and compaction summaries) carries over.
var jobScoped = map[string]bool{"system": true, "outcome": true, "interrupted": true, "persona": true}

// maxPersonaName bounds a persona file name; a longer name is hashed and written
// as the file's first record, so the file still says whose it is.
const maxPersonaName = 200

// OpenTranscript opens the persona's JSONL file under dir for append and returns
// its root frame, whose view continues the persona's history: the root records
// of earlier jobs, replayed as the live views held them (see replay), without
// their job-scoped records. compactBytes ≤ 0 disables compaction.
func OpenTranscript(dir, persona string, compactBytes int, chatter Chatter) (*Transcript, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := persona
	if len(name) > maxPersonaName {
		sum := sha256.Sum256([]byte(persona))
		name = hex.EncodeToString(sum[:])
	}
	path := filepath.Join(dir, name+".jsonl")
	prior, err := readRecords(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	t := &Transcript{rec: &record{f: f, w: bufio.NewWriter(f), seq: lastBracket(prior)}, chatter: chatter, compactBytes: compactBytes}
	if len(prior) == 0 && name != persona {
		if err := t.rec.write(agent.Message{Role: agent.RoleSystem, Kind: "persona", Content: persona}); err != nil {
			f.Close()
			return nil, err
		}
		t.rec.all = nil // the header is the file's, not this job's
	}
	t.msgs = withoutJobScoped(replay(prior))
	t.loaded = len(t.msgs)
	for _, m := range t.msgs {
		t.nbytes += size(m)
	}
	return t, nil
}

// replay rebuilds the root view a persona's file describes, mirroring what each
// live job held: a job's system prompt begins a job, so the job-scoped records
// before it are dropped exactly as OpenTranscript dropped them, and it heads the
// view exactly as Append placed it; a summary
// replaces all but the kept tail, exactly as compact did. Bracket records never
// enter a root view.
func replay(recs []agent.Message) []agent.Message {
	var view []agent.Message
	for _, m := range recs {
		switch {
		case m.Bracket != "" || m.Kind == "persona" || m.Kind == "interrupted":
			// never held by a live root view
		case m.Kind == "system":
			view = append([]agent.Message{m}, withoutJobScoped(view)...)
		case m.Kind == "summary":
			kept, _ := toInt(m.Result["kept"])
			kept = min(max(kept, 0), len(view))
			view = append([]agent.Message{m}, view[len(view)-kept:]...)
		default:
			view = append(view, m)
		}
	}
	return view
}

func withoutJobScoped(msgs []agent.Message) []agent.Message {
	out := make([]agent.Message, 0, len(msgs))
	for _, m := range msgs {
		if !jobScoped[m.Kind] {
			out = append(out, m)
		}
	}
	return out
}

// lastBracket returns the highest bracket number in recs, so a job's subtasks
// never reuse an earlier job's bracket id.
func lastBracket(recs []agent.Message) int {
	n := 0
	for _, m := range recs {
		if b, err := strconv.Atoi(strings.TrimPrefix(m.Bracket, "b")); err == nil && strings.HasPrefix(m.Bracket, "b") {
			n = max(n, b)
		}
	}
	return n
}

// Loaded is the number of records the view carried in from earlier jobs.
func (t *Transcript) Loaded() int { return t.loaded }

// Append persists m (tagged with this frame's bracket) and adds it to the view,
// compacting if the view has grown past the byte budget.
func (t *Transcript) Append(ctx context.Context, m agent.Message) error {
	m.Bracket = t.bracket
	if err := t.rec.write(m); err != nil {
		return err
	}
	if m.Kind == "system" {
		t.msgs = append([]agent.Message{m}, t.msgs...) // a job's system prompt heads its view
	} else {
		t.msgs = append(t.msgs, m)
	}
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

// All returns every record this job wrote, across every frame (for rating, which
// must see bracketed calls too and never an earlier job's).
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
	// The summary is written after the tail it keeps, so it records how many
	// records it kept: replay rebuilds this view exactly from the file.
	sm := agent.Message{Role: agent.RoleSystem, Kind: "summary", Content: summary,
		Result: map[string]any{"kept": len(tail)}, Bracket: t.bracket}
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

// Recover scans dir for persona files whose last record is not terminal — jobs
// that were mid-flight at shutdown — and appends an "interrupted" marker to each, the agent-side mirror of the kernel's interrupted-trace recovery. It
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
			Content: "job interrupted by shutdown; not resumed"}); err == nil {
			marked = append(marked, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	return marked, nil
}

func lastRecord(path string) (*agent.Message, error) {
	recs, err := readRecords(path)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return &recs[len(recs)-1], nil
}

// readRecords reads a JSONL file's records, skipping blank or unparseable lines.
func readRecords(path string) ([]agent.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []agent.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var m agent.Message
		if line := strings.TrimSpace(sc.Text()); line != "" && json.Unmarshal([]byte(line), &m) == nil {
			recs = append(recs, m)
		}
	}
	return recs, sc.Err()
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
