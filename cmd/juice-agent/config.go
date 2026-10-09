package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daios-ai/juice-agent/juice"
)

// Config is a single agent's configuration, kernel-style: flat keys, safe
// defaults. It is read from the optional config.json in the agent's home,
// $JUICE_HOME/agents/<login>/ (§11 of requirements.md); an unknown key or a
// malformed file is an error at startup. The daemon never writes it. Secrets never
// live here: the session is the login's, in $JUICE_HOME/client/credentials/.
type Config struct {
	MemoryBytes    int           `json:"memory_bytes"`
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

	// Computed at resolution time from the login and $JUICE_HOME; never JSON.
	Login   string `json:"-"`
	Home    string `json:"-"`
	DataDir string `json:"-"`
}

// ChannelConfig enables and configures intake surfaces.
type ChannelConfig struct {
	Task *TaskChannelConfig `json:"task,omitempty"`
	HTTP *HTTPChannelConfig `json:"http,omitempty"`
	Chat *ChatChannelConfig `json:"chat,omitempty"`
}

type TaskChannelConfig struct {
	PollSeconds int `json:"poll_seconds"`
}

type HTTPChannelConfig struct {
	Addr string `json:"addr"`
}

// ChatChannelConfig enables the chat channel (juice-ui chat rooms). Off by
// default; the agent must be a room member (the msg grant is provisioned on
// demand — see chatActions.ensureGrant).
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
// (Login) and paths (Home, DataDir) are computed by ResolveConfig, not here.
func DefaultConfig() Config {
	return Config{
		MemoryBytes:    65536,
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

// defaultTaskPoll is the fallback poll interval (seconds) for the task channel,
// which every agent always has (see ResolveConfig).
const defaultTaskPoll = 5

// ResolveConfig computes the agent home ($JUICE_HOME/agents/<login>/) from the
// login and decodes its config.json over the defaults (a key it omits keeps its
// default). A missing file is fine; a malformed file or an unknown key is an
// error. The task channel is always present: it is how the kernel hands the
// agent work, so an absent channel or a non-positive poll interval is defaulted.
func ResolveConfig(login string) (Config, error) {
	if _, _, err := juice.ParseLogin(login); err != nil {
		return Config{}, err
	}
	cfg := DefaultConfig()
	cfg.Login = login
	cfg.Home = filepath.Join(juiceHome(), "agents", login)
	cfg.DataDir = filepath.Join(cfg.Home, "data")
	path := filepath.Join(cfg.Home, "config.json")
	if raw, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, err
	}
	if cfg.Channels.Task == nil {
		cfg.Channels.Task = &TaskChannelConfig{}
	}
	if cfg.Channels.Task.PollSeconds <= 0 {
		cfg.Channels.Task.PollSeconds = defaultTaskPoll
	}
	return cfg, nil
}

// EnsureHome creates the agent home and data dir (mode 0700) and writes blank
// persona.md/knowledge.md/memory.md if absent, so the operator always has concrete
// files to edit. It never truncates an existing file and writes no config.
func (c Config) EnsureHome() error {
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
	return nil
}

func (c Config) personaPath() string   { return filepath.Join(c.Home, "persona.md") }
func (c Config) knowledgePath() string { return filepath.Join(c.Home, "knowledge.md") }
func (c Config) memoryPath() string    { return filepath.Join(c.Home, "memory.md") }

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
