package channels

import (
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

func TestConvStoreSeedAndAppend(t *testing.T) {
	c := newConvStore(filepath.Join(t.TempDir(), "conv.json"), 20)
	if got := c.seed("r1"); got != nil {
		t.Fatalf("empty seed = %v, want nil", got)
	}
	c.append("r1", "alice: capital of France?", "Paris.")
	c.append("r1", "alice: and its population?", "About 2.1 million.")

	got := c.seed("r1")
	want := []agent.Message{
		{Role: agent.RoleUser, Content: "alice: capital of France?"},
		{Role: agent.RoleAssistant, Content: "Paris."},
		{Role: agent.RoleUser, Content: "alice: and its population?"},
		{Role: agent.RoleAssistant, Content: "About 2.1 million."},
	}
	if len(got) != len(want) {
		t.Fatalf("seed len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Fatalf("turn %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestConvStorePerSessionIsolation(t *testing.T) {
	c := newConvStore(filepath.Join(t.TempDir(), "conv.json"), 20)
	c.append("r1", "hi from r1", "hello r1")
	c.append("r2", "hi from r2", "hello r2")
	if len(c.seed("r1")) != 2 || c.seed("r1")[0].Content != "hi from r1" {
		t.Fatalf("r1 = %+v", c.seed("r1"))
	}
	if len(c.seed("r2")) != 2 || c.seed("r2")[1].Content != "hello r2" {
		t.Fatalf("r2 = %+v", c.seed("r2"))
	}
}

func TestConvStoreTrimsToWindow(t *testing.T) {
	c := newConvStore(filepath.Join(t.TempDir(), "conv.json"), 2) // keep 2 turns = the last single exchange
	c.append("r1", "q1", "a1")
	c.append("r1", "q2", "a2")
	got := c.seed("r1")
	if len(got) != 2 || got[0].Content != "q2" || got[1].Content != "a2" {
		t.Fatalf("windowed seed = %+v, want [q2 a2]", got)
	}
}

func TestConvStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.json")
	c := newConvStore(path, 20)
	c.append("r1", "remember this", "ok")
	reopened := newConvStore(path, 20)
	got := reopened.seed("r1")
	if len(got) != 2 || got[0].Content != "remember this" || got[1].Content != "ok" {
		t.Fatalf("persisted seed = %+v", got)
	}
}

func TestConvStoreDisabledIsNilSafe(t *testing.T) {
	c := newConvStore(filepath.Join(t.TempDir(), "conv.json"), 0) // window 0 -> disabled
	if c != nil {
		t.Fatal("window 0 must disable the store (nil)")
	}
	// Nil store: seed and append must be safe no-ops.
	if got := c.seed("r1"); got != nil {
		t.Fatalf("nil seed = %v, want nil", got)
	}
	c.append("r1", "q", "a") // must not panic
}
