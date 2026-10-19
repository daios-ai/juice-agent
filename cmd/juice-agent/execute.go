package main

import (
	"context"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// Kernel is everything job execution needs from the kernel client: the three
// agent-core interfaces plus the reasoning and rating helpers. The juice client
// satisfies it; tests use a fake.
type Kernel interface {
	agent.Decider
	agent.Catalog
	agent.Runner
	Chatter
	Rater
}

// Executor runs jobs end to end: the persona's transcript, the system prompt,
// the retry-wrapped loop, the outcome record, then the post-job rating pass. It
// is the seam cmd/juice-agent drives; the loop itself stays in agent/.
type Executor struct {
	Kernel       Kernel
	PersonasDir  string
	CompactBytes int
	Retry        RetryPolicy
	RetryOK      func(error) bool
	Loop         agent.Config
	RateTasks    bool
	Handle       string // the agent's login, named in the system prompt
	AgentPath    string // agent.md, re-read at every job start so edits apply to the next job
	Log          func(event string, fields map[string]any)
}

// Execute runs one job to completion in its persona and returns its outcome.
// Errors from the loop are recorded but not returned as failures of Execute
// itself unless the transcript cannot be opened; a decide error becomes a failed
// outcome.
func (e *Executor) Execute(ctx context.Context, job agent.Job) (agent.Outcome, error) {
	logf := e.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	tr, err := OpenTranscript(e.PersonasDir, job.Persona, e.CompactBytes, e.Kernel)
	if err != nil {
		return agent.Outcome{Status: agent.StatusFailed, Reason: "transcript: " + err.Error()}, err
	}
	logf("persona_opened", map[string]any{"job": job.ID, "persona": job.Persona, "loaded": tr.Loaded()})

	sys := systemPrompt(e.Handle, e.Loop.ReplyTool, readFileOrEmpty(e.AgentPath))
	job.Messages = append([]agent.Message{sys}, job.Messages...)
	deps := agent.Deps{
		Decider:    WrapDecider(e.Kernel, e.Retry, e.retryOK()),
		Catalog:    e.Kernel,
		Runner:     WrapRunner(e.Kernel, e.Retry, e.retryOK()),
		Transcript: tr,
		Log:        logf,
	}
	out, loopErr := agent.Run(ctx, job, e.Loop, deps)
	if loopErr != nil {
		logf("loop_error", map[string]any{"job": job.ID, "error": loopErr.Error()})
	}

	// Record the terminal outcome so recovery can tell a finished job from an
	// interrupted one.
	_ = tr.Append(ctx, agent.Message{Role: agent.RoleSystem, Kind: "outcome",
		Content: string(out.Status), Result: map[string]any{
			"status": string(out.Status), "reason": out.Reason, "spent": out.Spent, "turns": out.Turns,
		}})
	// All() is this job's records across frames: bracketed calls are rated, and
	// an earlier job's transactions, already rated once (kernel D11), never are.
	msgs := tr.All()
	_ = tr.Close()
	if e.RateTasks {
		RateTask(ctx, msgs, e.Kernel, e.Loop.Intrinsics, logf)
	}
	return out, nil
}

func (e *Executor) retryOK() func(error) bool {
	if e.RetryOK != nil {
		return e.RetryOK
	}
	return func(error) bool { return false }
}

// Chatter summarises a conversation (used by compaction). Implemented by the
// juice client over the reasoning affordance.
type Chatter interface {
	Chat(ctx context.Context, messages []agent.Message) (string, error)
}

// retryDecider wraps an agent.Decider with bounded exponential backoff on
// retryable errors (transport failures, 429, 5xx, timeout). It is the model
// round-trip's resilience layer, mirroring pi's AgentSession retry.
type retryDecider struct {
	inner   agent.Decider
	policy  RetryPolicy
	retryOK func(error) bool
	sleep   func(time.Duration)
}

func (d retryDecider) Decide(ctx context.Context, msgs []agent.Message, catalog []agent.Candidate, tools []agent.Tool) (*agent.Selection, error) {
	var last error
	for attempt := 0; attempt <= d.policy.MaxRetries; attempt++ {
		sel, err := d.inner.Decide(ctx, msgs, catalog, tools)
		if err == nil {
			return sel, nil
		}
		last = err
		if !d.retryOK(err) || attempt == d.policy.MaxRetries {
			break
		}
		d.sleep(d.policy.backoff(attempt))
	}
	return nil, last
}

// retryRunner wraps an agent.Runner: it retries only transport-level failures,
// never a kernel call that ran and failed (that is a result fed to the model).
// Every attempt carries the dispatch's key, so a retry after a lost reply is
// answered with the first run's outcome instead of running the action again.
type retryRunner struct {
	inner   agent.Runner
	policy  RetryPolicy
	retryOK func(error) bool
	sleep   func(time.Duration)
}

func (r retryRunner) Run(ctx context.Context, action string, args map[string]any, quoteHash, key string) (agent.RunResult, error) {
	var last error
	for attempt := 0; attempt <= r.policy.MaxRetries; attempt++ {
		res, err := r.inner.Run(ctx, action, args, quoteHash, key)
		if err == nil || !r.retryOK(err) {
			return res, err
		}
		last = err
		if attempt == r.policy.MaxRetries {
			break
		}
		r.sleep(r.policy.backoff(attempt))
	}
	return agent.RunResult{}, last
}

// RetryPolicy is the backoff schedule: MaxRetries additional attempts after the
// first, delays doubling from BaseDelay (pi's 2s/4s/8s by default).
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
}

// DefaultRetryPolicy matches pi: 3 retries, 2s base (2s, 4s, 8s).
func DefaultRetryPolicy() RetryPolicy { return RetryPolicy{MaxRetries: 3, BaseDelay: 2 * time.Second} }

func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

// WrapDecider returns dec fortified with the retry policy. retryOK classifies
// which errors are worth retrying (juice.IsRetryable in production).
func WrapDecider(dec agent.Decider, p RetryPolicy, retryOK func(error) bool) agent.Decider {
	return retryDecider{inner: dec, policy: p, retryOK: retryOK, sleep: time.Sleep}
}

// WrapRunner returns run fortified with the retry policy (transport failures only).
func WrapRunner(run agent.Runner, p RetryPolicy, retryOK func(error) bool) agent.Runner {
	return retryRunner{inner: run, policy: p, retryOK: retryOK, sleep: time.Sleep}
}
