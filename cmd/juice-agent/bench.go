package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// runBench executes the experiment suite against a live agent and writes a
// versioned JSON report. It is excluded from `go test ./...` (needs a real
// kernel and LLM); it is the instrument for tracking competence across versions.
func runBench(args []string, handle string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: juice-agent bench <suite.json> [reps]")
	}
	suitePath := args[0]
	reps := 5
	if len(args) >= 2 {
		if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
			reps = n
		}
	}
	suite, err := LoadSuite(suitePath)
	if err != nil {
		return fmt.Errorf("loading suite: %w", err)
	}

	log := newLogger()
	ctx := context.Background()
	e, _, _, _, err := setup(ctx, log, handle)
	if err != nil {
		return err
	}
	// Bench tasks should not spawn maker synthesis; rating is orthogonal and left
	// on so runs exercise the real post-task path.
	e.Maker = nil

	run := func(ctx context.Context, t agent.Task) (agent.Outcome, error) {
		return e.Execute(ctx, t, nil)
	}
	rep := RunSuite(ctx, suite, reps, agentVersion(), run, time.Now)

	// Report to stdout and to a committed results file.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(rep)

	out := "bench/results/" + rep.SuiteVersion + "_" + strings.ReplaceAll(rep.AgentVersion, "/", "-") + ".json"
	if err := WriteReport(out, rep); err != nil {
		log.event("bench_write_error", map[string]any{"error": err.Error()})
	} else {
		log.event("bench_report", map[string]any{"path": out, "success_rate": rep.SuccessRate})
	}
	return nil
}

// agentVersion is the build-injected version (Makefile -ldflags). It falls back
// to `git describe` only for un-injected builds (go run / go test), so a bench
// report is always keyed to an identifiable revision.
func agentVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	return version
}

// Check is a machine-checkable success criterion over a task's final answer.
type Check struct {
	Type  string `json:"type"`            // equals | contains | regex
	Field string `json:"field,omitempty"` // answer field to test (default "answer")
	Value string `json:"value"`
}

// field returns the answer field this check targets.
func (c Check) field() string {
	if c.Field == "" {
		return "answer"
	}
	return c.Field
}

// Pass reports whether answer satisfies the check.
func (c Check) Pass(answer map[string]any) bool {
	got := stringify(answer[c.field()])
	switch c.Type {
	case "equals":
		return strings.TrimSpace(got) == strings.TrimSpace(c.Value)
	case "contains":
		return strings.Contains(strings.ToLower(got), strings.ToLower(c.Value))
	case "regex":
		re, err := regexp.Compile(c.Value)
		return err == nil && re.MatchString(got)
	default:
		return false
	}
}

func stringify(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		b, _ := json.Marshal(s)
		return string(b)
	}
}

// Task is one benchmark item.
type BenchTask struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Budget int    `json:"budget"`
	Check  Check  `json:"check"`
}

// Suite is the versioned task set.
type Suite struct {
	Version string      `json:"suite_version"`
	Tasks   []BenchTask `json:"tasks"`
}

// TaskResult is the aggregate over N repetitions of one task.
type TaskResult struct {
	ID            string  `json:"id"`
	Reps          int     `json:"reps"`
	SuccessRate   float64 `json:"success_rate"`
	MeanSpent     float64 `json:"mean_spent"`
	MeanTurns     float64 `json:"mean_turns"`
	MeanLatencyMs float64 `json:"mean_latency_ms"`
}

// Report is one full run of the suite against one agent version.
type Report struct {
	AgentVersion string       `json:"agent_version"`
	SuiteVersion string       `json:"suite_version"`
	Generated    string       `json:"generated"`
	Reps         int          `json:"reps"`
	SuccessRate  float64      `json:"success_rate"` // headline: mean over all task reps
	Tasks        []TaskResult `json:"tasks"`
}

// RunFunc executes one task and returns its outcome — supplied by the caller so
// this stays decoupled from the juice client.
type RunFunc func(ctx context.Context, t agent.Task) (agent.Outcome, error)

// LoadSuite reads a suite JSON file.
func LoadSuite(path string) (Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, err
	}
	var s Suite
	err = json.Unmarshal(raw, &s)
	return s, err
}

// Run executes every task reps times via run, grading each answer against its
// check, and returns the aggregated report. now supplies timestamps (injectable
// for tests).
func RunSuite(ctx context.Context, s Suite, reps int, agentVersion string, run RunFunc, now func() time.Time) Report {
	if reps <= 0 {
		reps = 1
	}
	rep := Report{AgentVersion: agentVersion, SuiteVersion: s.Version, Reps: reps,
		Generated: now().UTC().Format(time.RFC3339)}
	var totalSucc, totalReps int
	for _, task := range s.Tasks {
		tr := TaskResult{ID: task.ID, Reps: reps}
		var succ, spent, turns int
		var latency time.Duration
		for i := 0; i < reps; i++ {
			at := agent.Task{ID: task.ID + "-r" + strconv.Itoa(i), Channel: "bench", Budget: task.Budget,
				Messages: []agent.Message{{Role: agent.RoleUser, Content: task.Prompt}}}
			start := now()
			out, err := run(ctx, at)
			latency += now().Sub(start)
			spent += out.Spent
			turns += out.Turns
			if err == nil && out.Status == agent.StatusAnswered && task.Check.Pass(out.Answer) {
				succ++
			}
		}
		tr.SuccessRate = float64(succ) / float64(reps)
		tr.MeanSpent = float64(spent) / float64(reps)
		tr.MeanTurns = float64(turns) / float64(reps)
		tr.MeanLatencyMs = float64(latency.Milliseconds()) / float64(reps)
		rep.Tasks = append(rep.Tasks, tr)
		totalSucc += succ
		totalReps += reps
	}
	if totalReps > 0 {
		rep.SuccessRate = float64(totalSucc) / float64(totalReps)
	}
	return rep
}

// WriteReport writes rep as indented JSON to path, creating parent dirs.
func WriteReport(path string, rep Report) error {
	if err := os.MkdirAll(dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func dir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}
