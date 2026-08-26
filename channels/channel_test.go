package channels

import "testing"

func TestSameHandle(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"agent", "agent", true},
		{"agent", "alice", false},
		// Handles are bare everywhere; a stray sigil is a different string, never
		// stripped (kernel contract).
		{"@agent", "agent", false},
		{"", "", true},
		{"a", "ab", false},
	}
	for _, c := range cases {
		if got := SameHandle(c.a, c.b); got != c.want {
			t.Errorf("SameHandle(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
