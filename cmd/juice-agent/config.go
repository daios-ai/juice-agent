package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is a single agent's configuration, kernel-style: flat keys, safe
// defaults, rejected if malformed at startup. It is resolved by layering an
// optional component-wide config.json under $JUICE_HOME/agent/ and the agent's
// own config.json under $JUICE_HOME/agent/<handle>/ (§11 of requirements.md).
// Secrets never live here — the password comes from --password-stdin,
// AGENT_PASSWORD, AGENT_PASSWORD_FILE, or an interactive prompt (main.go).
type Config struct {
	ServerURL      string        `json:"server_url"`
	Memory         MemoryConfig  `json:"memory"`
	DefaultBudget  int           `json:"default_budget"`
	MaxTurns       int           `json:"max_turns"`
	MaxRepeats     int           `json:"max_repeats"`
	Concurrency    int           `json:"concurrency"`
	CandidateLimit int           `json:"candidate_limit"`
	LookupLimit    int           `json:"lookup_limit"`
	CompactBytes   int           `json:"compact_bytes"`
	RateTasks      bool          `json:"rate_tasks"`
	Maker          MakerConfig   `json:"maker"`
	Channels       ChannelConfig `json:"channels"`

	// Computed at resolution time from the handle and $JUICE_HOME; never JSON.
	Handle  string `json:"-"`
	Home    string `json:"-"`
	DataDir string `json:"-"`
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
	Chat *ChatChannelConfig `json:"chat,omitempty"`
}

type StepChannelConfig struct {
	PollSeconds int `json:"poll_seconds"`
}

type HTTPChannelConfig struct {
	Addr string `json:"addr"`
}

// ChatChannelConfig enables the chat channel (juice-ui chat rooms). Off by
// default; the agent must be a room member (the chat/msg grant is provisioned on
// demand — see chatAdapter.ensureGrant).
type ChatChannelConfig struct {
	Enabled     bool `json:"enabled"`
	PollSeconds int  `json:"poll_seconds"`
	// ContextMessages is how many recent conversation turns to seed as context for
	// each reply (0 → default 20; -1 disables, answering each message in isolation).
	ContextMessages int               `json:"context_messages"`
	Loop            LoopChannelConfig `json:"loop"`
}

// LoopChannelConfig bounds the chat channel's reply rate to any one counterpart,
// breaking runaway agent↔agent exchanges. Zero fields fall back to the driver's
// defaults (20 / 60s / 60s).
type LoopChannelConfig struct {
	Enabled         bool `json:"enabled"`
	MaxEvents       int  `json:"max_events"`
	WindowSeconds   int  `json:"window_seconds"`
	CooldownSeconds int  `json:"cooldown_seconds"`
}

// DefaultConfig is the safe baseline applied when a key is absent. Identity
// (Handle) and paths (Home, DataDir) are computed by ResolveConfig, not here.
func DefaultConfig() Config {
	return Config{
		ServerURL:      "http://localhost:4040",
		Memory:         MemoryConfig{Enabled: true, MemoryBytes: 65536},
		DefaultBudget:  100,
		MaxTurns:       32,
		MaxRepeats:     3,
		Concurrency:    4,
		CandidateLimit: 24,
		LookupLimit:    8,
		CompactBytes:   262144,
		RateTasks:      true,
		Maker:          MakerConfig{Enabled: false, MinRepeats: 3, Window: 50, Budget: 40, Threshold: 0.85},
	}
}

// juiceHome returns the family root: $JUICE_HOME, else ~/.juice. It is fixed,
// never cwd-relative, so an agent attaches to the same state wherever launched.
func juiceHome() string {
	if h := os.Getenv("JUICE_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".juice")
	}
	return ".juice"
}

// resolveHandle fixes the agent's identity: the handle argument, else
// AGENT_HANDLE, else the default agent. Handles are bare, as on the kernel: a
// sigil or a slash is rejected, never stripped.
func resolveHandle(cliHandle string) (string, error) {
	handle := cliHandle
	if handle == "" {
		handle = os.Getenv("AGENT_HANDLE")
	}
	if handle == "" {
		handle = "agent"
	}
	if strings.ContainsAny(handle, "@/") {
		return "", fmt.Errorf("handle %q must be bare, without %q or %q", handle, "@", "/")
	}
	return handle, nil
}

// defaultStepPoll is the fallback poll interval (seconds) for the step channel,
// which every agent always has (see ensureStepChannel).
const defaultStepPoll = 5

// componentDir is $JUICE_HOME/agent — the juice-agent component root, holding the
// optional shared config.json and the `agents/` subtree of per-agent homes.
func componentDir() string { return filepath.Join(juiceHome(), "agent") }

// ResolveConfig computes the agent home ($JUICE_HOME/agent/agents/<handle>/) from
// the handle, then layers the component-wide config.json and the per-agent
// config.json over the defaults (a later layer overrides only the keys it sets —
// a merge, not a wholesale replace). A missing config file is fine; a malformed
// one is an error. The step channel is always present (ensureStepChannel).
func ResolveConfig(cliHandle string) (Config, error) {
	handle, err := resolveHandle(cliHandle)
	if err != nil {
		return Config{}, err
	}
	component := componentDir()
	home := filepath.Join(component, "agents", handle)

	cfg := DefaultConfig()
	if err := overlayConfig(&cfg, filepath.Join(component, "config.json")); err != nil {
		return cfg, err
	}
	if err := overlayConfig(&cfg, filepath.Join(home, "config.json")); err != nil {
		return cfg, err
	}
	ensureStepChannel(&cfg)

	cfg.Handle = handle
	cfg.Home = home
	cfg.DataDir = filepath.Join(home, "data")
	return cfg, nil
}

// ensureStepChannel guarantees the agent always has a working step channel — the
// kernel-native intake by which other users and agents hand it work. It is the
// primary way the daemon receives tasks, so it is never left unconfigured: an
// absent channel is defaulted, and a non-positive poll interval is repaired.
func ensureStepChannel(c *Config) {
	if c.Channels.Step == nil {
		c.Channels.Step = &StepChannelConfig{PollSeconds: defaultStepPoll}
		return
	}
	if c.Channels.Step.PollSeconds <= 0 {
		c.Channels.Step.PollSeconds = defaultStepPoll
	}
}

// overlayConfig unmarshals the JSON file at path over cfg, overriding only the
// keys it contains. A nonexistent file is a no-op; a malformed one is an error.
func overlayConfig(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, cfg)
}

// EnsureHome creates the agent home and data dir (mode 0700), writes blank
// persona.md/knowledge.md/memory.md if absent, and dumps a standard config.json
// if absent, so the operator always has concrete files to edit and the agent is
// functional out of the box. It never truncates an existing file. The dumped
// config is the resolved effective config (component defaults folded in, step
// channel guaranteed by ResolveConfig), so it stays consistent across restarts.
func (c Config) EnsureHome() error {
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"persona.md", "knowledge.md", "memory.md"} {
		f, err := os.OpenFile(filepath.Join(c.Home, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		f.Close()
	}
	cfgPath := filepath.Join(c.Home, "config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, append(b, '\n'), 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}

func (c Config) personaPath() string   { return filepath.Join(c.Home, "persona.md") }
func (c Config) knowledgePath() string { return filepath.Join(c.Home, "knowledge.md") }
func (c Config) memoryPath() string    { return filepath.Join(c.Home, "memory.md") }

// legacyNotice returns a one-line warning if the pre-0.1.3 cwd-relative layout
// is detected (a local agent.json/agent-data or the retired AGENT_HOME/
// AGENT_CONFIG env vars). The daemon now uses $JUICE_HOME/agent/<handle>/ and
// ignores these — a clean cut-over — so the operator is told to move state, not
// silently attached to stale files.
func legacyNotice() string {
	var found []string
	for _, p := range []string{"agent.json", "agent-data"} {
		if _, err := os.Stat(p); err == nil {
			found = append(found, "./"+p)
		}
	}
	for _, e := range []string{"AGENT_HOME", "AGENT_CONFIG"} {
		if os.Getenv(e) != "" {
			found = append(found, "$"+e)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return "legacy layout detected (" + strings.Join(found, ", ") +
		"); the daemon now uses $JUICE_HOME/agent/<handle>/ and ignores these — move any needed state there"
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
