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
	if cfg.Login != "research@work" || cfg.Home != wantHome || cfg.DataDir != filepath.Join(wantHome, "data") {
		t.Fatalf("login=%q home=%q data=%q", cfg.Login, cfg.Home, cfg.DataDir)
	}
	if cfg.MaxTurns != 32 || cfg.DefaultBudget != 100 || cfg.MemoryBytes != 65536 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Channels.Task == nil || cfg.Channels.Task.PollSeconds != defaultTaskPoll {
		t.Fatalf("task channel = %+v, want present with the default poll", cfg.Channels.Task)
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
	writeAgentConfig(t, root, "a@k", `{"max_turns": 7, "memory_bytes": 0, "channels": {"task": {"poll_seconds": 0}, "http": {"addr": "127.0.0.1:0"}}}`)
	cfg, err := ResolveConfig("a@k")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurns != 7 || cfg.DefaultBudget != 100 || cfg.MemoryBytes != 0 {
		t.Fatalf("cfg = %+v, want max_turns 7, default budget kept, memory off", cfg)
	}
	if cfg.Channels.Task.PollSeconds != defaultTaskPoll || cfg.Channels.HTTP.Addr != "127.0.0.1:0" {
		t.Fatalf("channels = %+v", cfg.Channels)
	}
}

func TestResolveConfigRejectsUnknownKeysAndMalformedJSON(t *testing.T) {
	root := setJuiceHome(t)
	for _, body := range []string{`{"server_url": "http://x"}`, `{"channels": {"step": {}}}`, `{not json`} {
		writeAgentConfig(t, root, "a@k", body)
		if _, err := ResolveConfig("a@k"); err == nil {
			t.Errorf("config %s accepted, want an error", body)
		}
	}
}

func TestEnsureHomeCreatesBlankFilesAndNoConfig(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg.personaPath(), cfg.knowledgePath(), cfg.memoryPath()} {
		if b, err := os.ReadFile(p); err != nil || len(b) != 0 {
			t.Fatalf("%s: %q, %v; want an empty file", p, b, err)
		}
	}
	if fi, err := os.Stat(cfg.DataDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir: %v %v, want mode 0700", fi, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("EnsureHome must not write config.json (stat err = %v)", err)
	}
}

func TestEnsureHomeNeverTruncates(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	cfg.EnsureHome()
	os.WriteFile(cfg.personaPath(), []byte("you are terse"), 0o600)
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(cfg.personaPath()); string(got) != "you are terse" {
		t.Fatalf("persona truncated: %q", got)
	}
}

func TestMakerConfigFillsZeroFields(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Maker = MakerConfig{Enabled: true} // all thresholds zero
	m := cfg.makerConfig()
	if !m.Enabled {
		t.Fatal("enabled lost")
	}
	if m.MinRepeats <= 0 || m.Window <= 0 || m.Budget <= 0 || m.Threshold <= 0 {
		t.Fatalf("zero fields not filled from defaults: %+v", m)
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
