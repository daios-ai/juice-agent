package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setJuiceHome points $JUICE_HOME at a temp dir and clears AGENT_HANDLE, so
// resolution is deterministic and isolated from the developer's real home.
func setJuiceHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("JUICE_HOME", dir)
	t.Setenv("AGENT_HANDLE", "")
	return dir
}

func TestResolveConfigDefaultsWhenNoFiles(t *testing.T) {
	root := setJuiceHome(t)
	cfg, err := ResolveConfig("@research")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Handle != "@research" {
		t.Fatalf("handle = %q", cfg.Handle)
	}
	wantHome := filepath.Join(root, "agent", "research")
	if cfg.Home != wantHome {
		t.Fatalf("home = %q, want %q", cfg.Home, wantHome)
	}
	if cfg.DataDir != filepath.Join(wantHome, "data") {
		t.Fatalf("data dir = %q", cfg.DataDir)
	}
	if cfg.memoryPath() != filepath.Join(wantHome, "memory.md") {
		t.Fatalf("memory path = %q", cfg.memoryPath())
	}
	// Defaults applied when no config file exists.
	if cfg.MaxTurns != 32 || cfg.DefaultBudget != 100 || cfg.ServerURL != "http://localhost:4040" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestResolveHandlePrecedence(t *testing.T) {
	setJuiceHome(t)
	// CLI arg wins.
	if got := resolveHandle("@cli"); got != "@cli" {
		t.Fatalf("cli precedence = %q", got)
	}
	// Else AGENT_HANDLE.
	t.Setenv("AGENT_HANDLE", "@env")
	if got := resolveHandle(""); got != "@env" {
		t.Fatalf("env precedence = %q", got)
	}
	// Else default.
	t.Setenv("AGENT_HANDLE", "")
	if got := resolveHandle(""); got != "@agent" {
		t.Fatalf("default = %q", got)
	}
}

func TestResolveConfigLayeredMerge(t *testing.T) {
	root := setJuiceHome(t)
	component := filepath.Join(root, "agent")
	home := filepath.Join(component, "triage")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Component-wide defaults: server_url + max_turns.
	os.WriteFile(filepath.Join(component, "config.json"),
		[]byte(`{"server_url":"http://kernel:5000","max_turns":10}`), 0o644)
	// Per-agent overrides: max_turns only (server_url inherited from component).
	os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"max_turns":7,"maker":{"enabled":true}}`), 0o644)

	cfg, err := ResolveConfig("@triage")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.ServerURL != "http://kernel:5000" {
		t.Fatalf("component key not inherited: %q", cfg.ServerURL)
	}
	if cfg.MaxTurns != 7 {
		t.Fatalf("per-agent override lost: %d", cfg.MaxTurns)
	}
	if !cfg.Maker.Enabled {
		t.Fatalf("maker override lost")
	}
	// A key set in neither file keeps the built-in default.
	if cfg.DefaultBudget != 100 {
		t.Fatalf("default budget lost: %d", cfg.DefaultBudget)
	}
}

func TestResolveConfigMalformedIsError(t *testing.T) {
	root := setJuiceHome(t)
	home := filepath.Join(root, "agent", "agent")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{ not json`), 0o644)
	if _, err := ResolveConfig("@agent"); err == nil {
		t.Fatal("malformed config.json should be a startup error")
	}
}

func TestEnsureHomeCreatesDirAndBlankFiles(t *testing.T) {
	setJuiceHome(t)
	cfg, err := ResolveConfig("@research")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatalf("ensure home: %v", err)
	}
	info, err := os.Stat(cfg.Home)
	if err != nil {
		t.Fatalf("home not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("home mode = %o, want 0700", info.Mode().Perm())
	}
	if di, err := os.Stat(cfg.DataDir); err != nil || !di.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}
	for _, name := range []string{"persona.md", "knowledge.md", "memory.md"} {
		fi, err := os.Stat(filepath.Join(cfg.Home, name))
		if err != nil {
			t.Fatalf("%s not created: %v", name, err)
		}
		if fi.Size() != 0 {
			t.Fatalf("%s not blank: %d bytes", name, fi.Size())
		}
	}
}

func TestEnsureHomeIsIdempotentAndNonTruncating(t *testing.T) {
	setJuiceHome(t)
	cfg, err := ResolveConfig("@research")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	// Operator hand-edits persona.md.
	persona := cfg.personaPath()
	if err := os.WriteFile(persona, []byte("you are terse"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second init (e.g. daemon restart) must not truncate it.
	if err := cfg.EnsureHome(); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	got, err := os.ReadFile(persona)
	if err != nil || string(got) != "you are terse" {
		t.Fatalf("persona truncated or lost: %q err=%v", got, err)
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

func TestLegacyNoticeDetectsRetiredEnv(t *testing.T) {
	t.Setenv("AGENT_HOME", "")
	t.Setenv("AGENT_CONFIG", "")
	if n := legacyNotice(); n != "" {
		t.Fatalf("no legacy inputs should yield no notice, got %q", n)
	}
	t.Setenv("AGENT_HOME", "/somewhere")
	if n := legacyNotice(); n == "" {
		t.Fatal("AGENT_HOME should trigger a legacy notice")
	}
}
