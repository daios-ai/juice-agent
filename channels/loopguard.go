package channels

import (
	"sync"
	"time"
)

// Loop-guard defaults: at most defaultLoopMax replies to the same counterpart
// within defaultLoopWindow, then defaultLoopCooldown of silence. The rate is the
// signal — no bot detection — so a runaway agent↔agent exchange self-limits while
// human conversation (never 20 replies a minute to one peer) is untouched.
const (
	defaultLoopMax      = 20
	defaultLoopWindow   = 60 * time.Second
	defaultLoopCooldown = 60 * time.Second
)

// loopGuard is a per-key sliding-window rate limiter with a cooldown. A key is a
// (session, sender) pair, so the guard bounds how often the agent replies to one
// counterpart in one conversation — a busy multi-party room is never penalized for
// traffic between others. Safe for concurrent use.
type loopGuard struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	cooldown time.Duration
	now      func() time.Time
	events   map[string][]time.Time // key -> reply times within the window
	until    map[string]time.Time   // key -> cooldown expiry (absent when not cooling down)
}

// newLoopGuard builds a guard; non-positive bounds fall back to the defaults and
// a nil clock to time.Now.
func newLoopGuard(max int, window, cooldown time.Duration, now func() time.Time) *loopGuard {
	if max <= 0 {
		max = defaultLoopMax
	}
	if window <= 0 {
		window = defaultLoopWindow
	}
	if cooldown <= 0 {
		cooldown = defaultLoopCooldown
	}
	if now == nil {
		now = time.Now
	}
	return &loopGuard{max: max, window: window, cooldown: cooldown, now: now,
		events: map[string][]time.Time{}, until: map[string]time.Time{}}
}

// allow records a reply attempt for key and reports whether it may proceed. Once
// more than max attempts fall inside the window the key enters a cooldown during
// which every attempt is refused; when the cooldown expires the window resets.
func (g *loopGuard) allow(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if exp, ok := g.until[key]; ok {
		if now.Before(exp) {
			return false
		}
		delete(g.until, key)
		delete(g.events, key)
	}
	cutoff := now.Add(-g.window)
	kept := g.events[key][:0]
	for _, t := range g.events[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	g.events[key] = kept
	if len(kept) > g.max {
		g.until[key] = now.Add(g.cooldown)
		return false
	}
	return true
}
