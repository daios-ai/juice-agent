// Package session is the agent's policy shell (pi's coding-agent layer): durable
// JSONL transcripts, crash recovery, retry-with-backoff around model and tool
// calls, and context compaction. It wraps the pure agent core with the messy
// realities of persistence and unreliable I/O, keeping the core clean.
package session

import (
	"context"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// Chatter summarises a conversation (used by compaction). Implemented by the
// juice client over @sys/llm/chat.
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

func (d retryDecider) Decide(ctx context.Context, msgs []agent.Message, actions []string) (*agent.Selection, error) {
	var last error
	for attempt := 0; attempt <= d.policy.MaxRetries; attempt++ {
		sel, err := d.inner.Decide(ctx, msgs, actions)
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
type retryRunner struct {
	inner   agent.Runner
	policy  RetryPolicy
	retryOK func(error) bool
	sleep   func(time.Duration)
}

func (r retryRunner) Run(ctx context.Context, action string, args map[string]any) (agent.RunResult, error) {
	var last error
	for attempt := 0; attempt <= r.policy.MaxRetries; attempt++ {
		res, err := r.inner.Run(ctx, action, args)
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
