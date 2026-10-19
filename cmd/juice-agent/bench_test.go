package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

func TestCheckTypes(t *testing.T) {
	cases := []struct {
		check  Check
		answer map[string]any
		want   bool
	}{
		{Check{Type: "equals", Value: "391"}, map[string]any{"answer": "391"}, true},
		{Check{Type: "equals", Value: "391"}, map[string]any{"answer": " 391 "}, true}, // trimmed
		{Check{Type: "equals", Value: "391"}, map[string]any{"answer": "392"}, false},
		{Check{Type: "contains", Value: "buenos"}, map[string]any{"answer": "Buenos días"}, true},
		{Check{Type: "regex", Value: `^\d+$`}, map[string]any{"answer": "42"}, true},
		{Check{Type: "regex", Value: `^\d+$`}, map[string]any{"answer": "4a2"}, false},
		{Check{Type: "contains", Field: "city", Value: "web"}, map[string]any{"city": "sys@k/web"}, true},
	}
	for i, c := range cases {
		if got := c.check.Pass(c.answer); got != c.want {
			t.Errorf("case %d: Pass = %v, want %v", i, got, c.want)
		}
	}
}

func TestRunAggregatesMetrics(t *testing.T) {
	suite := Suite{Version: "vT", Tasks: []BenchTask{
		{ID: "a", Prompt: "p", Budget: 10, Check: Check{Type: "equals", Value: "ok"}},
	}}
	// Deterministic clock: each now() call advances 100ms.
	var tick int64
	now := func() time.Time { tick++; return time.Unix(0, tick*int64(100*time.Millisecond)) }

	// run alternates success/failure to give a 0.5 rate over 4 reps.
	call := 0
	run := func(_ context.Context, tk agent.Job) (agent.Outcome, error) {
		call++
		ans := "no"
		if call%2 == 0 {
			ans = "ok"
		}
		return agent.Outcome{Status: agent.StatusAnswered, Answer: map[string]any{"answer": ans},
			Spent: 3, Turns: 2}, nil
	}
	rep := RunSuite(context.Background(), suite, 4, "test-1", run, now)
	if len(rep.Tasks) != 1 {
		t.Fatalf("tasks = %d", len(rep.Tasks))
	}
	if rep.Tasks[0].SuccessRate != 0.5 {
		t.Fatalf("success rate = %v, want 0.5", rep.Tasks[0].SuccessRate)
	}
	if rep.Tasks[0].MeanSpent != 3 || rep.Tasks[0].MeanTurns != 2 {
		t.Fatalf("means = spent %v turns %v", rep.Tasks[0].MeanSpent, rep.Tasks[0].MeanTurns)
	}
	if rep.SuccessRate != 0.5 {
		t.Fatalf("aggregate success = %v, want 0.5", rep.SuccessRate)
	}
	if rep.AgentVersion != "test-1" || rep.SuiteVersion != "vT" {
		t.Fatalf("versions = %s / %s", rep.AgentVersion, rep.SuiteVersion)
	}
}

// Two runs started within one second get distinct personas.
func TestRunsWithinOneSecondUseDistinctPersonas(t *testing.T) {
	suite := Suite{Version: "vT", Tasks: []BenchTask{{ID: "a", Prompt: "p", Budget: 10}}}
	personas := map[string]int{}
	run := func(_ context.Context, j agent.Job) (agent.Outcome, error) {
		personas[j.Persona]++
		return agent.Outcome{Status: agent.StatusAnswered}, nil
	}
	for _, at := range []int64{1, 2} {
		now := func() time.Time { return time.Unix(100, at*int64(time.Millisecond)) }
		RunSuite(context.Background(), suite, 2, "test-1", run, now)
	}
	if len(personas) != 4 {
		t.Fatalf("personas = %v, want four distinct (two runs x two reps)", personas)
	}
}

func TestLoadSuiteAndWriteReport(t *testing.T) {
	// The committed starter suite must parse.
	s, err := LoadSuite("../../bench/tasks.json")
	if err != nil {
		t.Fatalf("load starter suite: %v", err)
	}
	if s.Version == "" || len(s.Tasks) == 0 {
		t.Fatalf("starter suite empty: %+v", s)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "results", "r.json")
	if err := WriteReport(path, Report{AgentVersion: "v", SuiteVersion: s.Version, SuccessRate: 1}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("report not written: %v", err)
	}
}
