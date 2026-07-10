package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config is the daemon's configuration (agent.json), kernel-style: flat keys,
// safe local defaults, rejected if invalid at startup. Secrets never live here —
// the password comes from AGENT_PASSWORD or a mode-0600 file.
type Config struct {
	ServerURL      string        `json:"server_url"`
	Handle         string        `json:"handle"`
	AgentsDir      string        `json:"agents_dir"`
	DataDir        string        `json:"data_dir"`
	PersonaFile    string        `json:"persona_file"`
	KnowledgeFile  string        `json:"knowledge_file"`
	Memory         MemoryConfig  `json:"memory"`
	DefaultBudget  int           `json:"default_budget"`
	MaxTurns       int           `json:"max_turns"`
	Concurrency    int           `json:"concurrency"`
	CandidateLimit int           `json:"candidate_limit"`
	LookupLimit    int           `json:"lookup_limit"`
	CompactBytes   int           `json:"compact_bytes"`
	RateTasks      bool          `json:"rate_tasks"`
	Maker          MakerConfig   `json:"maker"`
	Channels       ChannelConfig `json:"channels"`

	// Home is the resolved agent-home directory; computed, never from JSON.
	Home string `json:"-"`
}

// MemoryConfig governs the agent's learned cross-task memory.
type MemoryConfig struct {
	Enabled     bool `json:"enabled"`
	MemoryBytes int  `json:"memory_bytes"`
}

// ChannelConfig enables and configures intake surfaces.
type ChannelConfig struct {
	Step *StepChannelConfig `json:"step,omitempty"`
	HTTP *HTTPChannelConfig `json:"http,omitempty"`
}

type StepChannelConfig struct {
	PollSeconds int `json:"poll_seconds"`
}

type HTTPChannelConfig struct {
	Addr string `json:"addr"`
}

// DefaultConfig is the safe local baseline applied when a key is absent.
// DataDir is left empty so home resolution can decide it (home/data for a named
// agent, ./agent-data in legacy mode) without clobbering an explicit setting.
func DefaultConfig() Config {
	return Config{
		ServerURL:      "http://localhost:4040",
		Handle:         "@agent",
		AgentsDir:      "./agents",
		DataDir:        "",
		PersonaFile:    "persona.md",
		KnowledgeFile:  "knowledge.md",
		Memory:         MemoryConfig{Enabled: true, MemoryBytes: 65536},
		DefaultBudget:  100,
		MaxTurns:       32,
		Concurrency:    4,
		CandidateLimit: 24,
		LookupLimit:    8,
		CompactBytes:   262144,
		RateTasks:      true,
		Maker:          MakerConfig{Enabled: false, MinRepeats: 3, Window: 50, Budget: 40, Threshold: 0.85},
	}
}

// resolve fixes the agent's identity and home from the launch inputs: an
// optional CLI handle (`juice-agent @research`), AGENT_HANDLE, or the config's
// own handle; and an optional AGENT_HOME override. A *named* launch (handle from
// CLI or env) roots the agent in its own home under AgentsDir; otherwise the
// legacy layout applies (home ".", data ./agent-data). Paths default inside the
// home so a named agent is self-contained.
func (c *Config) resolve(cliHandle, homeOverride string, named bool) {
	if cliHandle != "" {
		c.Handle = cliHandle
	}
	switch {
	case homeOverride != "":
		c.Home = homeOverride
	case named:
		c.Home = filepath.Join(c.AgentsDir, strings.TrimPrefix(c.Handle, "@"))
	default:
		c.Home = "."
	}
	if c.DataDir == "" {
		if c.Home == "." {
			c.DataDir = "./agent-data"
		} else {
			c.DataDir = filepath.Join(c.Home, "data")
		}
	}
}

// underHome resolves a file relative to the agent home (absolute paths and empty
// names pass through unchanged).
func (c Config) underHome(name string) string {
	if name == "" || filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(c.Home, name)
}

func (c Config) personaPath() string   { return c.underHome(c.PersonaFile) }
func (c Config) knowledgePath() string { return c.underHome(c.KnowledgeFile) }
func (c Config) memoryPath() string    { return c.underHome("memory.md") }

// ResolveConfig loads the config and fixes the agent's identity and home. The
// handle is cliHandle (from a leading `@handle` arg), else AGENT_HANDLE, else the
// config's handle; a launch is *named* when the handle came from the CLI or env.
// A named agent's own `<home>/agent.json` takes precedence over AGENT_CONFIG, so
// per-agent config is just a file in the home.
func ResolveConfig(cliHandle string) (Config, error) {
	cfg, err := LoadConfig(configPath())
	if err != nil {
		return cfg, err
	}
	envHandle := os.Getenv("AGENT_HANDLE")
	named := cliHandle != "" || envHandle != ""
	handle := cliHandle
	if handle == "" {
		handle = envHandle
	}
	if handle == "" {
		handle = cfg.Handle
	}
	homeOverride := os.Getenv("AGENT_HOME")

	// Determine the tentative home to look for a home-local config, then reload
	// from it if present.
	tentative := &Config{Handle: cfg.Handle, AgentsDir: cfg.AgentsDir}
	tentative.resolve(handle, homeOverride, named)
	if tentative.Home != "." {
		if raw, rerr := os.ReadFile(filepath.Join(tentative.Home, "agent.json")); rerr == nil {
			home := DefaultConfig()
			if json.Unmarshal(raw, &home) == nil {
				cfg = home
			}
		}
	}
	cfg.resolve(handle, homeOverride, named)
	return cfg, nil
}

// LoadConfig reads agent.json at path over the defaults. A missing file is not
// an error (defaults apply); a malformed one is.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// makerConfig returns the maker config with zero fields filled from defaults,
// so a partial config stays sane.
func (c Config) makerConfig() MakerConfig {
	d := DefaultMakerConfig()
	m := MakerConfig{
		Enabled:    c.Maker.Enabled,
		MinRepeats: c.Maker.MinRepeats,
		Window:     c.Maker.Window,
		Budget:     c.Maker.Budget,
		Threshold:  c.Maker.Threshold,
	}
	if m.MinRepeats <= 0 {
		m.MinRepeats = d.MinRepeats
	}
	if m.Window <= 0 {
		m.Window = d.Window
	}
	if m.Budget <= 0 {
		m.Budget = d.Budget
	}
	if m.Threshold <= 0 {
		m.Threshold = d.Threshold
	}
	return m
}
