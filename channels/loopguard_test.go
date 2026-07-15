package channels

import (
	"testing"
	"time"
)

// clock is a controllable time source for the guard's injected now().
type clock struct{ t time.Time }

func (c *clock) now() time.Time  { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestLoopGuardTripsThenCooldownThenRecovers(t *testing.T) {
	clk := newClock()
	g := newLoopGuard(2, 60*time.Second, 60*time.Second, clk.now)

	// The first max attempts pass; the one that exceeds max trips the cooldown.
	if !g.allow("k") || !g.allow("k") {
		t.Fatal("first two attempts must be allowed")
	}
	if g.allow("k") {
		t.Fatal("the third attempt within the window must be refused (cooldown)")
	}
	// Every attempt during the cooldown is refused.
	clk.add(30 * time.Second)
	if g.allow("k") {
		t.Fatal("attempts during cooldown must be refused")
	}
	// After the cooldown expires the window resets and traffic resumes.
	clk.add(31 * time.Second)
	if !g.allow("k") {
		t.Fatal("after cooldown the key must be allowed again")
	}
}

func TestLoopGuardSlidingWindow(t *testing.T) {
	clk := newClock()
	g := newLoopGuard(2, 60*time.Second, 60*time.Second, clk.now)
	if !g.allow("k") {
		t.Fatal("attempt 1 allowed")
	}
	// Advance past the window so the first event ages out; two more must then fit.
	clk.add(61 * time.Second)
	if !g.allow("k") || !g.allow("k") {
		t.Fatal("stale events must not count toward the budget")
	}
}

func TestLoopGuardPerKeyIsolation(t *testing.T) {
	clk := newClock()
	g := newLoopGuard(1, 60*time.Second, 60*time.Second, clk.now)
	if !g.allow("a") {
		t.Fatal("first attempt on a allowed")
	}
	if g.allow("a") {
		t.Fatal("second attempt on a refused")
	}
	// A different key is unaffected by a's cooldown.
	if !g.allow("b") {
		t.Fatal("a's cooldown must not suppress key b")
	}
}
