package session

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
)

// Memory is the agent's durable, legible cross-task memory: a Markdown file it
// appends a task summary to when a task settles and reloads into the system
// prompt before the next one. It is byte-bounded; over budget the oldest entries
// are folded into a digest via one @sys/llm/chat call. This is the middle path
// between pi (no automatic cross-task memory) and a semantic store — accumulation
// is automatic, the result stays human-readable.
type Memory struct {
	path    string
	bytes   int
	chatter Chatter

	mu sync.Mutex
}

// NewMemory builds a Memory backed by path with the given byte budget.
func NewMemory(path string, bytes int, chatter Chatter) *Memory {
	if bytes <= 0 {
		bytes = 65536
	}
	return &Memory{path: path, bytes: bytes, chatter: chatter}
}

// Digest returns the current memory contents to inject into the system prompt
// (empty if there is no memory yet). Safe on a nil Memory.
func (m *Memory) Digest() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Update summarises a finished task into a dated entry and appends it, compacting
// if the file grows past the byte budget. Best-effort: any failure is swallowed —
// a memory hiccup must never fail the task. stamp is an injected timestamp label
// (supplied by the caller so this stays deterministic in tests).
func (m *Memory) Update(ctx context.Context, stamp string, task agent.Task, out agent.Outcome, msgs []agent.Message) {
	if m == nil || m.chatter == nil {
		return
	}
	entry := m.summarise(ctx, task, out, msgs)
	if strings.TrimSpace(entry) == "" {
		return
	}
	block := "## " + stamp + " — " + task.ID + "\n" + strings.TrimSpace(entry) + "\n\n"

	m.mu.Lock()
	defer m.mu.Unlock()
	existing, _ := os.ReadFile(m.path)
	combined := string(existing) + block
	if len(combined) > m.bytes {
		combined = m.compact(ctx, combined)
	}
	_ = os.WriteFile(m.path, []byte(combined), 0o644)
}

// summarise asks the model to distil the finished task into a few terse notes.
func (m *Memory) summarise(ctx context.Context, task agent.Task, out agent.Outcome, msgs []agent.Message) string {
	var b strings.Builder
	b.WriteString("Summarise this finished task into 2-4 terse bullet points: what was asked, what " +
		"you did (name the actions used), the outcome, and anything worth remembering for similar " +
		"future tasks. Facts and action names only; no preamble.\n\n")
	b.WriteString("Request: " + taskPrompt(task) + "\n")
	b.WriteString("Outcome: " + string(out.Status) + "\n\nWhat happened:\n")
	for _, mm := range msgs {
		switch {
		case mm.Role == agent.RoleAssistant && mm.Action != "":
			b.WriteString("- selected " + mm.Action + "\n")
		case mm.Role == agent.RoleTool && mm.Action != "":
			status := "ok"
			if mm.IsError {
				status = "error"
			}
			b.WriteString("- " + mm.Action + " → " + status + "\n")
		}
	}
	reply, err := m.chatter.Chat(ctx, []agent.Message{{Role: agent.RoleUser, Content: b.String()}})
	if err != nil {
		return ""
	}
	return reply
}

// compact folds the older half of the memory into a digest, keeping recent
// entries verbatim, when the file exceeds its byte budget. On failure it returns
// the input unchanged (the file may stay over budget until the next update, which
// is preferable to losing memory).
func (m *Memory) compact(ctx context.Context, combined string) string {
	entries := splitEntries(combined)
	if len(entries) <= 1 {
		return combined
	}
	// Keep the most recent entries up to ~half the budget; summarise the rest.
	keep := m.bytes / 2
	acc, cut := 0, len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		acc += len(entries[i])
		if acc >= keep {
			cut = i
			break
		}
	}
	if cut <= 0 {
		return combined
	}
	older := strings.Join(entries[:cut], "")
	recent := strings.Join(entries[cut:], "")
	summary, err := m.chatter.Chat(ctx, []agent.Message{{Role: agent.RoleUser, Content: "Compress " +
		"these older memory notes into a shorter digest, preserving concrete facts, action names, and " +
		"lessons learned. Terse, no preamble.\n\n" + older}})
	if err != nil || strings.TrimSpace(summary) == "" {
		return combined
	}
	return "## digest (older tasks, condensed)\n" + strings.TrimSpace(summary) + "\n\n" + recent
}

// splitEntries splits a memory file into blocks, each beginning at a "## "
// heading (lines before the first heading form their own leading block).
func splitEntries(s string) []string {
	var entries []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			entries = append(entries, cur.String())
			cur.Reset()
		}
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "## ") {
			flush()
		}
		cur.WriteString(ln)
		cur.WriteByte('\n')
	}
	flush()
	if len(entries) == 0 {
		return []string{s}
	}
	return entries
}
