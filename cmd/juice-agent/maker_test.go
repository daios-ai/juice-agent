package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

// fixedEmbedder returns a canned vector per prompt substring, so similar
// prompts map to identical (cosine=1) vectors.
type fixedEmbedder struct{ vecs map[string][]float64 }

func (e fixedEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	for k, v := range e.vecs {
		if contains(text, k) {
			return v, nil
		}
	}
	return []float64{0, 0, 1}, nil
}

func contains(s, sub string) bool { return len(sub) > 0 && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func makeTask(id, prompt string) agent.Job {
	return agent.Job{ID: id, Channel: "http", Budget: 100,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}}}
}

func TestMakerTriggersAfterRepeats(t *testing.T) {
	dir := t.TempDir()
	emb := fixedEmbedder{vecs: map[string][]float64{"pirate": {1, 0, 0}}}
	cfg := MakerConfig{Enabled: true, MinRepeats: 3, Window: 50, Budget: 40, Threshold: 0.9}
	m, err := NewMaker(dir, cfg, emb, nil, "sys@k/tinygo/compile", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := agent.Outcome{Status: agent.StatusAnswered}

	if s := m.Observe(ctx, makeTask("1", "translate to pirate speak please"), out); s != nil {
		t.Fatal("should not trigger on first occurrence")
	}
	if s := m.Observe(ctx, makeTask("2", "make this pirate style"), out); s != nil {
		t.Fatal("should not trigger on second occurrence")
	}
	synth := m.Observe(ctx, makeTask("3", "give me pirate talk"), out)
	if synth == nil {
		t.Fatal("expected synthesis task on the third similar request")
	}
	if synth.Channel != "self" {
		t.Fatalf("synthesis channel = %q, want self", synth.Channel)
	}
	if synth.Budget != 40 {
		t.Fatalf("synthesis budget = %d, want 40", synth.Budget)
	}
	// A self-channel task must never itself re-trigger the maker.
	if s := m.Observe(ctx, *synth, out); s != nil {
		t.Fatal("self-channel task must not trigger the maker")
	}
}

func TestMakerDisabledNeverTriggers(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewMaker(dir, MakerConfig{Enabled: false, MinRepeats: 1}, fixedEmbedder{}, nil, "sys@k/tinygo/compile", nil)
	if s := m.Observe(context.Background(), makeTask("1", "x"), agent.Outcome{}); s != nil {
		t.Fatal("disabled maker must not trigger")
	}
}

// fakePublisher records the publish lifecycle.
type fakePublisher struct {
	created, enabled, madePublic, deleted bool
	runErr                                error
	runs                                  int
}

func (p *fakePublisher) CreateWasmAction(_ context.Context, _, _ string, _ int, _, _ map[string]any, _ string) (string, string, error) {
	p.created = true
	return "act-1", "agent/thing", nil
}
func (p *fakePublisher) EnableAction(_ context.Context, _ string) error { p.enabled = true; return nil }
func (p *fakePublisher) SetPublic(_ context.Context, _ string) error    { p.madePublic = true; return nil }
func (p *fakePublisher) DeleteAction(_ context.Context, _ string) error { p.deleted = true; return nil }
func (p *fakePublisher) Run(_ context.Context, _ string, _ map[string]any, _, _ string) (agent.RunResult, error) {
	p.runs++
	return agent.RunResult{}, p.runErr
}

func TestPublishIntrinsicSuccess(t *testing.T) {
	dir := t.TempDir()
	pub := &fakePublisher{}
	m, _ := NewMaker(dir, MakerConfig{Enabled: true}, fixedEmbedder{}, pub, "sys@k/tinygo/compile", nil)
	intr := m.PublishIntrinsic()
	res, terminal, err := intr.Handle(context.Background(), map[string]any{
		"name": "thing", "artifact": "BASE64", "description": "d",
		"test_inputs": []any{map[string]any{"x": 1.0}, map[string]any{"x": 2.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		t.Fatal("publish must not terminate the run")
	}
	if res["status"] != "success" {
		t.Fatalf("status = %v, want success", res["status"])
	}
	if !pub.created || !pub.enabled || !pub.madePublic {
		t.Fatalf("lifecycle incomplete: %+v", pub)
	}
	if pub.runs != 2 {
		t.Fatalf("ran %d tests, want 2", pub.runs)
	}
	if pub.deleted {
		t.Fatal("should not delete on success")
	}
}

func TestPublishIntrinsicDeletesOnTestFailure(t *testing.T) {
	dir := t.TempDir()
	pub := &fakePublisher{runErr: errors.New("schema violation")}
	m, _ := NewMaker(dir, MakerConfig{Enabled: true}, fixedEmbedder{}, pub, "sys@k/tinygo/compile", nil)
	intr := m.PublishIntrinsic()
	res, _, _ := intr.Handle(context.Background(), map[string]any{
		"name": "thing", "artifact": "B64",
		"test_inputs": []any{map[string]any{"x": 1.0}},
	})
	if res["status"] != "failure" {
		t.Fatalf("status = %v, want failure", res["status"])
	}
	if !pub.deleted {
		t.Fatal("a failing action must be deleted (transactional publish)")
	}
	if pub.madePublic {
		t.Fatal("a failing action must not be made public")
	}
}

func TestMakerConcurrentObserveNoCorruption(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewMaker(dir, MakerConfig{Enabled: false, Window: 1000}, fixedEmbedder{}, nil, "sys@k/tinygo/compile", nil)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Observe(context.Background(), makeTask("t"+strconv.Itoa(i), "prompt"), agent.Outcome{Status: agent.StatusAnswered})
		}(i)
	}
	wg.Wait()
	// Every append must be one intact JSON line — no interleaving.
	data, _ := os.ReadFile(filepath.Join(dir, "tasks.jsonl"))
	lines := 0
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r taskRecord
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("corrupted line: %q (%v)", ln, err)
		}
		lines++
	}
	if lines != n {
		t.Fatalf("wrote %d intact lines, want %d", lines, n)
	}
}
