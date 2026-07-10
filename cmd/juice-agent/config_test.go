package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultsWhenAbsent(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if cfg.Handle != "@agent" || cfg.MaxTurns != 32 || cfg.DefaultBudget != 100 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	os.WriteFile(path, []byte(`{
		"handle": "@bot",
		"max_turns": 8,
		"maker": {"enabled": true, "min_repeats": 5},
		"channels": {"http": {"addr": ":9000"}, "step": {"poll_seconds": 3}}
	}`), 0o644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Handle != "@bot" || cfg.MaxTurns != 8 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	// Unspecified keys keep defaults.
	if cfg.DefaultBudget != 100 {
		t.Fatalf("default budget lost: %d", cfg.DefaultBudget)
	}
	if cfg.Channels.HTTP == nil || cfg.Channels.HTTP.Addr != ":9000" {
		t.Fatalf("http channel config = %+v", cfg.Channels.HTTP)
	}
	if cfg.Channels.Step == nil || cfg.Channels.Step.PollSeconds != 3 {
		t.Fatalf("step channel config = %+v", cfg.Channels.Step)
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

func TestResolveLegacyLayout(t *testing.T) {
	// No CLI handle, no env: legacy layout — home ".", data ./agent-data.
	c := DefaultConfig()
	c.resolve("", "", false)
	if c.Home != "." {
		t.Fatalf("legacy home = %q, want .", c.Home)
	}
	if c.DataDir != "./agent-data" {
		t.Fatalf("legacy data_dir = %q, want ./agent-data", c.DataDir)
	}
	if c.memoryPath() != "memory.md" {
		t.Fatalf("legacy memory path = %q", c.memoryPath())
	}
}

func TestResolveNamedAgentHome(t *testing.T) {
	c := DefaultConfig() // AgentsDir ./agents
	c.resolve("@research", "", true)
	if c.Handle != "@research" {
		t.Fatalf("handle = %q", c.Handle)
	}
	if c.Home != "agents/research" {
		t.Fatalf("home = %q, want agents/research", c.Home)
	}
	if c.DataDir != "agents/research/data" {
		t.Fatalf("data_dir = %q, want agents/research/data", c.DataDir)
	}
	if c.personaPath() != "agents/research/persona.md" || c.memoryPath() != "agents/research/memory.md" {
		t.Fatalf("paths = %q / %q", c.personaPath(), c.memoryPath())
	}
}

func TestResolveHomeOverrideAndExplicitDataDir(t *testing.T) {
	c := DefaultConfig()
	c.DataDir = "/tmp/custom-data" // explicit -> respected
	c.resolve("@bob", "/opt/bob", true)
	if c.Home != "/opt/bob" {
		t.Fatalf("home override = %q", c.Home)
	}
	if c.DataDir != "/tmp/custom-data" {
		t.Fatalf("explicit data_dir clobbered: %q", c.DataDir)
	}
}
