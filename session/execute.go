package session

import (
	"context"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// Kernel is everything the policy shell needs from the kernel client: the three
// agent-core interfaces plus the LLM and rating helpers. The juice client
// satisfies it; tests use a fake.
type Kernel interface {
	agent.Decider
	agent.Catalog
	agent.Runner
	Chatter
	Rater
	Embedder
}

// Executor runs tasks end to end: persistent transcript, retry-wrapped loop,
// outcome record, then the post-task rating pass and maker observation. It is
// the seam cmd/juice-agent drives; the loop itself stays in agent/.
type Executor struct {
	Kernel       Kernel
	DataDir      string
	CompactBytes int
	Retry        RetryPolicy
	RetryOK      func(error) bool
	Loop         agent.Config
	RateTasks    bool
	Maker        *Maker
	Prompter     *Prompter        // assembles the per-task system prompt (persona/knowledge/memory)
	Memory       *Memory          // learned cross-task memory; nil disables it
	Enqueue      func(agent.Task) // where maker-proposed synthesis tasks go
	Log          func(event string, fields map[string]any)
}

// Execute runs one task to completion and returns its outcome. Errors from the
// loop are recorded but not returned as failures of Execute itself unless the
// transcript cannot be opened; a decide error becomes a failed outcome.
func (e *Executor) Execute(ctx context.Context, task agent.Task, steering func() []agent.Message) (agent.Outcome, error) {
	logf := e.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}

	tr, err := OpenTranscript(e.DataDir, task.ID, e.CompactBytes, e.Kernel)
	if err != nil {
		return agent.Outcome{Status: agent.StatusFailed, Reason: "transcript: " + err.Error()}, err
	}

	// Prepend the assembled system prompt (persona + knowledge + current memory
	// digest) so the model carries its identity and history into the task.
	if e.Prompter != nil {
		sys := e.Prompter.Build(e.Memory.Digest())
		task.Messages = append([]agent.Message{sys}, task.Messages...)
	}

	deps := agent.Deps{
		Decider:    WrapDecider(e.Kernel, e.Retry, e.retryOK()),
		Catalog:    e.Kernel,
		Runner:     WrapRunner(e.Kernel, e.Retry, e.retryOK()),
		Transcript: tr,
		Steering:   steering,
		Log:        logf,
	}

	out, loopErr := agent.Run(ctx, task, e.Loop, deps)
	if loopErr != nil {
		logf("loop_error", map[string]any{"task_id": task.ID, "error": loopErr.Error()})
	}

	// Record the terminal outcome so recovery can tell a finished task from an
	// interrupted one.
	_ = tr.Append(ctx, agent.Message{Role: agent.RoleSystem, Kind: "outcome",
		Content: string(out.Status), Result: map[string]any{
			"status": string(out.Status), "reason": out.Reason, "spent": out.Spent, "turns": out.Turns,
		}})
	msgs := tr.Messages()
	_ = tr.Close()

	if e.RateTasks {
		RateTask(ctx, msgs, e.Kernel, e.Loop.Intrinsics, logf)
	}
	if e.Maker != nil {
		if synth := e.Maker.Observe(ctx, task, out); synth != nil && e.Enqueue != nil {
			e.Enqueue(*synth)
		}
	}
	// Fold what happened into durable memory for future tasks (best-effort).
	if e.Memory != nil {
		e.Memory.Update(ctx, time.Now().UTC().Format("2006-01-02 15:04"), task, out, msgs)
	}
	return out, nil
}

func (e *Executor) retryOK() func(error) bool {
	if e.RetryOK != nil {
		return e.RetryOK
	}
	return func(error) bool { return false }
}
