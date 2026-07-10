// Package bench is the experiment suite: a versioned set of tasks with
// machine-checkable success criteria, run repeatedly against a live agent to
// produce quantitative performance metrics. Unlike unit tests and flows (binary,
// deterministic), competence is graded and stochastic, so the suite measures
// success rate, cost (credits, from the kernel ledger via the outcome), latency,
// and turns — a diffable time series across agent versions.
//
// The runner is excluded from `go test ./...`: it needs a live kernel and LLM.
package bench

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

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
type Task struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Budget int    `json:"budget"`
	Check  Check  `json:"check"`
}

// Suite is the versioned task set.
type Suite struct {
	Version string `json:"suite_version"`
	Tasks   []Task `json:"tasks"`
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
// bench stays decoupled from the session/juice layers.
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
func Run(ctx context.Context, s Suite, reps int, agentVersion string, run RunFunc, now func() time.Time) Report {
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
