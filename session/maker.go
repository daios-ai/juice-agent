package session

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
)

// Embedder produces embedding vectors (for repetition detection). Implemented
// by the juice client over @sys/llm/embed.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// Publisher registers, tests, and publishes WASM actions. Implemented by the
// juice client. It is the effect surface of the @agent/publish intrinsic.
type Publisher interface {
	CreateWasmAction(ctx context.Context, name, description string, price int, in, out map[string]any, artifactB64 string) (id, ref string, err error)
	EnableAction(ctx context.Context, id string) error
	SetPublic(ctx context.Context, id string) error
	DeleteAction(ctx context.Context, id string) error
	Run(ctx context.Context, action string, args map[string]any) (agent.RunResult, error)
}

// MakerConfig governs self-extension.
type MakerConfig struct {
	Enabled    bool
	MinRepeats int     // similar prompts within the window that trigger synthesis
	Window     int     // how many recent tasks to consider
	Budget     int     // credit budget for a synthesis task
	Threshold  float64 // cosine similarity counting as "the same kind of request"
}

// DefaultMakerConfig is a conservative starting policy.
func DefaultMakerConfig() MakerConfig {
	return MakerConfig{Enabled: false, MinRepeats: 3, Window: 50, Budget: 40, Threshold: 0.85}
}

// taskRecord is one line of the legible task index (data_dir/tasks.jsonl).
type taskRecord struct {
	ID     string    `json:"id"`
	Prompt string    `json:"prompt"`
	Status string    `json:"status"`
	Spent  int       `json:"spent"`
	Embed  []float64 `json:"embed,omitempty"`
}

// Maker observes completed tasks, records them in a legible index, and — when a
// request recurs without catalog coverage — proposes a synthesis task to build
// a WASM action that automates it. It also implements the @agent/publish
// intrinsic used during synthesis.
type Maker struct {
	cfg      MakerConfig
	path     string
	embedder Embedder
	pub      Publisher
	log      func(string, map[string]any)

	mu     sync.Mutex
	recent []taskRecord
}

// NewMaker builds a Maker persisting its index under dir. embedder and pub may
// be nil when the maker is disabled.
func NewMaker(dir string, cfg MakerConfig, embedder Embedder, pub Publisher, log func(string, map[string]any)) (*Maker, error) {
	if log == nil {
		log = func(string, map[string]any) {}
	}
	m := &Maker{cfg: cfg, path: filepath.Join(dir, "tasks.jsonl"), embedder: embedder, pub: pub, log: log}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m.recent, _ = readTaskIndex(m.path, cfg.Window)
	return m, nil
}

// Observe records a finished task and returns a synthesis task to enqueue when
// the repetition trigger fires, or nil. Synthesis tasks (channel "self") are
// themselves recorded but never re-trigger the maker.
func (m *Maker) Observe(ctx context.Context, task agent.Task, out agent.Outcome) *agent.Task {
	prompt := taskPrompt(task)
	rec := taskRecord{ID: task.ID, Prompt: prompt, Status: string(out.Status), Spent: out.Spent}
	if m.cfg.Enabled && m.embedder != nil && task.Channel != "self" {
		if v, err := m.embedder.Embed(ctx, prompt); err == nil {
			rec.Embed = v
		}
	}
	m.mu.Lock()
	m.recent = append(m.recent, rec)
	if len(m.recent) > m.cfg.Window {
		m.recent = m.recent[len(m.recent)-m.cfg.Window:]
	}
	sim := m.countSimilarLocked(rec)
	m.mu.Unlock()
	_ = appendJSONL(m.path, rec)

	if !m.cfg.Enabled || task.Channel == "self" || rec.Embed == nil {
		return nil
	}
	if sim < m.cfg.MinRepeats {
		return nil
	}
	m.log("maker_trigger", map[string]any{"prompt": trunc(prompt, 80), "similar": sim})
	return m.synthesisTask(task.ID, prompt)
}

// countSimilarLocked counts prior records (excluding rec itself) whose embedding
// is within the similarity threshold of rec. Must hold m.mu.
func (m *Maker) countSimilarLocked(rec taskRecord) int {
	if rec.Embed == nil {
		return 0
	}
	n := 0
	for _, r := range m.recent {
		if r.ID == rec.ID || r.Embed == nil {
			continue
		}
		if cosine(rec.Embed, r.Embed) >= m.cfg.Threshold {
			n++
		}
	}
	return n + 1 // include the current occurrence
}

// synthesisTask constructs the internal task that asks the agent to build and
// publish an action automating the recurring request.
func (m *Maker) synthesisTask(origin, prompt string) *agent.Task {
	instr := "A request of this kind keeps recurring and no existing action serves it well:\n\n" +
		prompt + "\n\nBuild a reusable Juice WASM action that handles this class of request. " +
		"Write a TinyGo Handle, compile it with @sys/tinygo/compile, then call @agent/publish with " +
		"the compiled artifact, a kebab-case name, a description, typed input/output schemas, and a " +
		"few distinct test_inputs. Publish only once it passes its tests, then finish with @agent/reply."
	return &agent.Task{
		ID:       "synth-" + origin,
		Channel:  "self",
		Budget:   m.cfg.Budget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: instr}},
	}
}

// PublishIntrinsic returns the @agent/publish handler: build.py's transactional
// publish as a harness intrinsic — create private, enable, run declared
// test_inputs, then make public on pass or delete and report on failure.
func (m *Maker) PublishIntrinsic() agent.Intrinsic {
	return agent.Intrinsic{Handle: func(ctx context.Context, args map[string]any) (map[string]any, bool, error) {
		if m.pub == nil {
			return map[string]any{"status": "failure", "error": "publishing not configured"}, false, nil
		}
		return m.publish(ctx, args)
	}}
}

func (m *Maker) publish(ctx context.Context, args map[string]any) (map[string]any, bool, error) {
	name, _ := args["name"].(string)
	artifact, _ := args["artifact"].(string)
	if name == "" || artifact == "" {
		return map[string]any{"status": "failure", "error": "name and artifact are required"}, false, nil
	}
	desc, _ := args["description"].(string)
	inSchema, _ := args["input_schema"].(map[string]any)
	outSchema, _ := args["output_schema"].(map[string]any)
	price := 0
	if p, ok := toInt(args["price"]); ok {
		price = p
	}

	id, ref, err := m.pub.CreateWasmAction(ctx, name, desc, price, inSchema, outSchema, artifact)
	if err != nil {
		return map[string]any{"status": "failure", "error": "create failed: " + err.Error()}, false, nil
	}
	if err := m.pub.EnableAction(ctx, id); err != nil {
		_ = m.pub.DeleteAction(ctx, id)
		return map[string]any{"status": "failure", "error": "enable failed: " + err.Error()}, false, nil
	}
	// Validate against declared test inputs (output schema now active).
	tests, _ := args["test_inputs"].([]any)
	passed := 0
	for _, t := range tests {
		input, _ := t.(map[string]any)
		if _, err := m.pub.Run(ctx, ref, input); err != nil {
			_ = m.pub.DeleteAction(ctx, id)
			return map[string]any{"status": "failure", "error": "test failed on input; fix and recompile",
				"detail": err.Error()}, false, nil
		}
		passed++
	}
	if err := m.pub.SetPublic(ctx, id); err != nil {
		return map[string]any{"status": "failure", "error": "publish (make public) failed: " + err.Error()}, false, nil
	}
	m.log("maker_published", map[string]any{"action": ref, "tests": passed})
	return map[string]any{"status": "success", "action": ref, "tests_passed": passed}, false, nil
}

// --- helpers ---------------------------------------------------------------

func taskPrompt(t agent.Task) string {
	var b strings.Builder
	for _, m := range t.Messages {
		if (m.Role == agent.RoleUser || m.Role == agent.RoleSystem) && m.Content != "" {
			b.WriteString(m.Content)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func readTaskIndex(path string, limit int) ([]taskRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []taskRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r taskRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			recs = append(recs, r)
		}
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[len(recs)-limit:]
	}
	return recs, sc.Err()
}

func appendJSONL(path string, v any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
