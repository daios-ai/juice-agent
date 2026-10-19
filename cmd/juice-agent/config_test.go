package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setJuiceHome points $JUICE_HOME at a temp dir, so resolution is deterministic
// and isolated from the developer's real home.
func setJuiceHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("JUICE_HOME", dir)
	return dir
}

func writeAgentConfig(t *testing.T, root, login, body string) {
	t.Helper()
	home := filepath.Join(root, "agents", login)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigDefaultsWhenNoFile(t *testing.T) {
	root := setJuiceHome(t)
	cfg, err := ResolveConfig("research@work")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantHome := filepath.Join(root, "agents", "research@work")
	if cfg.Login != "research@work" || cfg.Home != wantHome || cfg.PersonasDir != filepath.Join(wantHome, "personas") {
		t.Fatalf("login=%q home=%q personas=%q", cfg.Login, cfg.Home, cfg.PersonasDir)
	}
	if cfg.MaxTurns != 32 || cfg.DefaultBudget != 100 || cfg.PollSeconds != 5 || cfg.Concurrency != 4 || !cfg.RateTasks {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	// The affordances default to the natives of the login's own kernel.
	if cfg.Reason != "sys@work/llm/chat" || cfg.ActionLookup != "sys@work/lookup" {
		t.Fatalf("reason=%q action_lookup=%q", cfg.Reason, cfg.ActionLookup)
	}
}

func TestResolveConfigRejectsMalformedLogin(t *testing.T) {
	setJuiceHome(t)
	for _, bad := range []string{"", "agent", "agent@", "a/b@work"} {
		if _, err := ResolveConfig(bad); err == nil {
			t.Errorf("ResolveConfig(%q) accepted, want an error", bad)
		}
	}
}

func TestResolveConfigOverridesDefaultsAndRepairsPoll(t *testing.T) {
	root := setJuiceHome(t)
	writeAgentConfig(t, root, "a@k", `{"max_turns": 7, "reason": "brain@k/think", "poll_seconds": 0, "concurrency": -1}`)
	cfg, err := ResolveConfig("a@k")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurns != 7 || cfg.DefaultBudget != 100 || cfg.Reason != "brain@k/think" || cfg.ActionLookup != "sys@k/lookup" {
		t.Fatalf("cfg = %+v, want max_turns 7 and reason swapped, the rest defaulted", cfg)
	}
	if cfg.PollSeconds != 5 || cfg.Concurrency != 4 {
		t.Fatalf("poll=%d concurrency=%d, want the defaults restored", cfg.PollSeconds, cfg.Concurrency)
	}
}

func TestResolveConfigRejectsUnknownKeysAndMalformedJSON(t *testing.T) {
	root := setJuiceHome(t)
	// Every key retired in 0.4.0 is reported, never silently ignored.
	for _, body := range []string{`{"server_url": "http://x"}`, `{"channels": {"task": {}}}`, `{"memory_bytes": 0}`,
		`{"maker": {}}`, `{"max_repeats": 3}`, `{"candidate_limit": 9}`, `{"lookup_limit": 9}`, `{"lookup": "sys@k/lookup"}`, `{"reason": ""}`, `{"action_lookup": ""}`, `{not json`} {
		writeAgentConfig(t, root, "a@k", body)
		if _, err := ResolveConfig("a@k"); err == nil {
			t.Errorf("config %s accepted, want an error", body)
		}
	}
}

func TestEnsureHomeCreatesAgentFileAndPersonasAndNoConfig(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(cfg.agentPath()); err != nil || len(b) != 0 {
		t.Fatalf("agent.md: %q, %v; want an empty file", b, err)
	}
	if fi, err := os.Stat(cfg.PersonasDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("personas dir: %v %v, want mode 0700", fi, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("EnsureHome must not write config.json (stat err = %v)", err)
	}
}

func TestEnsureHomeNeverTruncates(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	cfg.EnsureHome()
	os.WriteFile(cfg.agentPath(), []byte("you are terse"), 0o600)
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(cfg.agentPath()); string(got) != "you are terse" {
		t.Fatalf("agent.md truncated: %q", got)
	}
}

func TestUsageNamesTheLogin(t *testing.T) {
	if err := runServe(nil); err == nil || !strings.Contains(err.Error(), "<login>") {
		t.Fatalf("serve without a login: err = %v", err)
	}
	if err := runTask([]string{"a@k"}); err == nil || !strings.Contains(err.Error(), "<login>") {
		t.Fatalf("task without a prompt: err = %v", err)
	}
}
