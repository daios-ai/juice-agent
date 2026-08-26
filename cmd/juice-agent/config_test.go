package main

import (
	"encoding/json"
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
	cfg, err := ResolveConfig("research")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Handle != "research" {
		t.Fatalf("handle = %q", cfg.Handle)
	}
	wantHome := filepath.Join(root, "agent", "agents", "research")
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

func TestResolveConfigAlwaysHasStepChannel(t *testing.T) {
	setJuiceHome(t)
	// No config files at all — the step channel must still be present.
	cfg, err := ResolveConfig("research")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels.Step == nil {
		t.Fatal("step channel absent with no config; must be guaranteed")
	}
	if cfg.Channels.Step.PollSeconds != defaultStepPoll {
		t.Fatalf("step poll = %d, want %d", cfg.Channels.Step.PollSeconds, defaultStepPoll)
	}
}

func TestResolveHandlePrecedence(t *testing.T) {
	setJuiceHome(t)
	// The handle argument wins.
	if got, err := resolveHandle("cli"); err != nil || got != "cli" {
		t.Fatalf("arg precedence = %q, %v", got, err)
	}
	// Else AGENT_HANDLE.
	t.Setenv("AGENT_HANDLE", "env")
	if got, err := resolveHandle(""); err != nil || got != "env" {
		t.Fatalf("env precedence = %q, %v", got, err)
	}
	// Else default.
	t.Setenv("AGENT_HANDLE", "")
	if got, err := resolveHandle(""); err != nil || got != "agent" {
		t.Fatalf("default = %q, %v", got, err)
	}
}

func TestResolveHandleRejectsSigilAndSlash(t *testing.T) {
	setJuiceHome(t)
	// Handles are bare: a sigil or a slash is an error, never stripped.
	for _, bad := range []string{"@research", "sys/lookup", "a@b"} {
		if _, err := resolveHandle(bad); err == nil {
			t.Fatalf("resolveHandle(%q) must be rejected", bad)
		}
	}
	// The same rule applies to AGENT_HANDLE.
	t.Setenv("AGENT_HANDLE", "@env")
	if _, err := resolveHandle(""); err == nil {
		t.Fatal("AGENT_HANDLE with a sigil must be rejected")
	}
	// And it surfaces through ResolveConfig.
	t.Setenv("AGENT_HANDLE", "")
	if _, err := ResolveConfig("@research"); err == nil {
		t.Fatal("ResolveConfig must reject a sigil-prefixed handle")
	}
}

func TestResolveConfigLayeredMerge(t *testing.T) {
	root := setJuiceHome(t)
	component := filepath.Join(root, "agent")
	home := filepath.Join(component, "agents", "triage")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Component-wide defaults: server_url + max_turns.
	os.WriteFile(filepath.Join(component, "config.json"),
		[]byte(`{"server_url":"http://kernel:5000","max_turns":10}`), 0o644)
	// Per-agent overrides: max_turns only (server_url inherited from component).
	os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"max_turns":7,"maker":{"enabled":true}}`), 0o644)

	cfg, err := ResolveConfig("triage")
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
	home := filepath.Join(root, "agent", "agents", "agent")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{ not json`), 0o644)
	if _, err := ResolveConfig("agent"); err == nil {
		t.Fatal("malformed config.json should be a startup error")
	}
}

func TestEnsureHomeCreatesDirAndBlankFiles(t *testing.T) {
	setJuiceHome(t)
	cfg, err := ResolveConfig("research")
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

func TestEnsureHomeDumpsStandardConfigWithStep(t *testing.T) {
	setJuiceHome(t)
	cfg, err := ResolveConfig("research")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	// A standard config.json must be dumped, and it must carry a step channel.
	raw, err := os.ReadFile(filepath.Join(cfg.Home, "config.json"))
	if err != nil {
		t.Fatalf("config.json not dumped: %v", err)
	}
	var dumped Config
	if err := json.Unmarshal(raw, &dumped); err != nil {
		t.Fatalf("dumped config is not valid JSON: %v", err)
	}
	if dumped.Channels.Step == nil {
		t.Fatal("dumped config.json has no step channel")
	}
	// It must not overwrite an operator-edited config on a later init.
	if err := os.WriteFile(filepath.Join(cfg.Home, "config.json"), []byte(`{"max_turns":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(cfg.Home, "config.json"))
	if string(raw) != `{"max_turns":99}` {
		t.Fatalf("existing config.json was overwritten: %s", raw)
	}
}

func TestEnsureHomeIsIdempotentAndNonTruncating(t *testing.T) {
	setJuiceHome(t)
	cfg, err := ResolveConfig("research")
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
