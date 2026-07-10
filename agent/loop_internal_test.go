package agent

import (
	"context"
	"strings"
	"testing"
)

func TestWorkingSetEvictsLowestValue(t *testing.T) {
	ws := newWorkingSet(2)
	// proven-good
	ws.put(Candidate{Ref: "@good", Stats: ActionStats{Known: true, Uses: 10, Successes: 10, RatingCount: 5, RatingEstimate: 1.0}})
	// persistent failer
	ws.put(Candidate{Ref: "@bad", Stats: ActionStats{Known: true, Uses: 10, Successes: 0, RatingCount: 5, RatingEstimate: 0.0}})
	// untested (neutral) — adding this exceeds the cap of 2, so the lowest-value
	// candidate (@bad) must be evicted.
	ws.put(Candidate{Ref: "@new"})

	if _, ok := ws.by["@bad"]; ok {
		t.Fatal("@bad (lowest value) should have been evicted")
	}
	if _, ok := ws.by["@good"]; !ok {
		t.Fatal("@good (highest value) should be retained")
	}
	if len(ws.by) != 2 {
		t.Fatalf("size = %d, want 2", len(ws.by))
	}
}

func TestStatsMessageRanksProvenFirstAndMarksUntested(t *testing.T) {
	ws := newWorkingSet(10)
	ws.put(Candidate{Ref: "@proven", Price: 5, Stats: ActionStats{Known: true, Uses: 8, Successes: 8, RatingCount: 3, RatingEstimate: 0.9, LatencyEstimate: 12}})
	ws.put(Candidate{Ref: "@fresh", Price: 2})

	msg, ok := ws.statsMessage()
	if !ok {
		t.Fatal("expected a stats message")
	}
	if msg.Role != RoleSystem {
		t.Fatalf("role = %q, want system", msg.Role)
	}
	pi := strings.Index(msg.Content, "@proven")
	fi := strings.Index(msg.Content, "@fresh")
	if pi < 0 || fi < 0 || pi > fi {
		t.Fatalf("proven should be listed before fresh:\n%s", msg.Content)
	}
	if !strings.Contains(msg.Content, "success=100%") {
		t.Fatalf("missing success rate:\n%s", msg.Content)
	}
	if !strings.Contains(msg.Content, "untested") {
		t.Fatalf("fresh action should be marked untested:\n%s", msg.Content)
	}
}

func TestParseLookup(t *testing.T) {
	res := map[string]any{"results": []any{
		map[string]any{"action": "@sys/time", "description": "current time", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"description": "no action ref — skipped"},
		map[string]any{"action": "@sys/web", "description": "fetch a page"},
	}}
	got := parseLookup(res)
	if len(got) != 2 {
		t.Fatalf("parsed %d candidates, want 2", len(got))
	}
	if got[0].Ref != "@sys/time" || got[1].Ref != "@sys/web" {
		t.Fatalf("refs = %q,%q", got[0].Ref, got[1].Ref)
	}
}

func TestMergeEnrichesWithMeta(t *testing.T) {
	ws := newWorkingSet(10)
	cat := &fakeCatalog{meta: map[string]struct {
		price int
		stats ActionStats
	}{"@sys/web": {price: 3, stats: ActionStats{Known: true, Uses: 4, Successes: 3}}}}
	deps := Deps{Catalog: cat}
	mergeCandidates(context.Background(), deps, ws, []Candidate{{Ref: "@sys/web", Description: "fetch"}})

	c := ws.by["@sys/web"]
	if c.Price != 3 {
		t.Fatalf("price = %d, want 3 (from Meta)", c.Price)
	}
	if !c.Stats.Known || c.Stats.Uses != 4 {
		t.Fatalf("stats not enriched: %+v", c.Stats)
	}
}

func TestLookupResultMergesIntoWorkingSet(t *testing.T) {
	// A lookup dispatch must expand the offered refs on the next turn.
	dec := &refCapturingDecider{script: []Selection{
		{Action: "@sys/lookup", Args: map[string]any{"query": "x"}},
		{Action: "@agent/reply", Args: map[string]any{"answer": "done"}},
	}}
	cat := &fakeCatalog{seed: nil}
	run := &recordingRunner{results: map[string]RunResult{
		"@sys/lookup": {Result: map[string]any{"results": []any{
			map[string]any{"action": "@sys/web", "description": "fetch"},
		}}},
	}}
	tr := &memTranscript{}
	Run(context.Background(), Task{Budget: 100}, baseConfig(), newDeps(dec, cat, run, tr))

	if !dec.everOffered("@sys/web") {
		t.Fatalf("@sys/web from lookup was never offered to decide; offered=%v", dec.allRefs)
	}
}

type refCapturingDecider struct {
	script  []Selection
	i       int
	allRefs [][]string
}

func (d *refCapturingDecider) Decide(_ context.Context, _ []Message, actions []string) (*Selection, error) {
	d.allRefs = append(d.allRefs, actions)
	if d.i >= len(d.script) {
		return &Selection{Action: "@agent/reply", Args: map[string]any{"answer": "fallback"}}, nil
	}
	s := d.script[d.i]
	d.i++
	return &s, nil
}

func (d *refCapturingDecider) everOffered(ref string) bool {
	for _, refs := range d.allRefs {
		for _, r := range refs {
			if r == ref {
				return true
			}
		}
	}
	return false
}
