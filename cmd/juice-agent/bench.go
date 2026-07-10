package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/bench"
)

// runBench executes the experiment suite against a live agent and writes a
// versioned JSON report. It is excluded from `go test ./...` (needs a real
// kernel and LLM); it is the instrument for tracking competence across versions.
func runBench(args []string) error {
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
	suite, err := bench.LoadSuite(suitePath)
	if err != nil {
		return fmt.Errorf("loading suite: %w", err)
	}

	log := newLogger()
	ctx := context.Background()
	e, _, _, _, err := setup(ctx, log)
	if err != nil {
		return err
	}
	// Bench tasks should not spawn maker synthesis; rating is orthogonal and left
	// on so runs exercise the real post-task path.
	e.Maker = nil

	run := func(ctx context.Context, t agent.Task) (agent.Outcome, error) {
		return e.Execute(ctx, t, nil)
	}
	rep := bench.Run(ctx, suite, reps, agentVersion(), run, time.Now)

	// Report to stdout and to a committed results file.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(rep)

	out := "bench/results/" + rep.SuiteVersion + "_" + strings.ReplaceAll(rep.AgentVersion, "/", "-") + ".json"
	if err := bench.WriteReport(out, rep); err != nil {
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
