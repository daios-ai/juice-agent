package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

type flakyDecider struct {
	failN int // fail this many times before succeeding
	calls int
}

func (d *flakyDecider) Decide(_ context.Context, _ []agent.Message, _ []string) (*agent.Selection, error) {
	d.calls++
	if d.calls <= d.failN {
		return nil, errors.New("transient")
	}
	return &agent.Selection{Action: "@agent/reply"}, nil
}

func alwaysRetry(error) bool { return true }
func neverRetry(error) bool  { return false }

func TestRetryDeciderSucceedsAfterTransientFailures(t *testing.T) {
	d := &flakyDecider{failN: 2}
	wrapped := retryDecider{inner: d, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Decide(context.Background(), nil, nil); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3 (2 fail + 1 success)", d.calls)
	}
}

func TestRetryDeciderStopsOnNonRetryable(t *testing.T) {
	d := &flakyDecider{failN: 5}
	wrapped := retryDecider{inner: d, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: neverRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Decide(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on non-retryable)", d.calls)
	}
}

type flakyRunner struct {
	failN int
	calls int
}

func (r *flakyRunner) Run(_ context.Context, _ string, _ map[string]any) (agent.RunResult, error) {
	r.calls++
	if r.calls <= r.failN {
		return agent.RunResult{}, errors.New("network")
	}
	return agent.RunResult{Result: map[string]any{"ok": true}}, nil
}

func TestRetryRunnerRetriesTransport(t *testing.T) {
	r := &flakyRunner{failN: 1}
	wrapped := retryRunner{inner: r, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Run(context.Background(), "@x", nil); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if r.calls != 2 {
		t.Fatalf("calls = %d, want 2", r.calls)
	}
}

func TestBackoffDoubles(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: 2 * time.Second}
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := p.backoff(attempt); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}
