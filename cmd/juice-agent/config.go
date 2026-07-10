package main

import (
	"encoding/json"
	"os"

	"github.com/daios-ai/juice-agent/session"
)

// Config is the daemon's configuration (agent.json), kernel-style: flat keys,
// safe local defaults, rejected if invalid at startup. Secrets never live here —
// the password comes from AGENT_PASSWORD or a mode-0600 file.
type Config struct {
	ServerURL      string        `json:"server_url"`
	Handle         string        `json:"handle"`
	DataDir        string        `json:"data_dir"`
	DefaultBudget  int           `json:"default_budget"`
	MaxTurns       int           `json:"max_turns"`
	Concurrency    int           `json:"concurrency"`
	CandidateLimit int           `json:"candidate_limit"`
	LookupLimit    int           `json:"lookup_limit"`
	CompactBytes   int           `json:"compact_bytes"`
	RateTasks      bool          `json:"rate_tasks"`
	Maker          MakerConfig   `json:"maker"`
	Channels       ChannelConfig `json:"channels"`
}

// MakerConfig mirrors session.MakerConfig for JSON.
type MakerConfig struct {
	Enabled    bool    `json:"enabled"`
	MinRepeats int     `json:"min_repeats"`
	Window     int     `json:"window"`
	Budget     int     `json:"budget"`
	Threshold  float64 `json:"threshold"`
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
func DefaultConfig() Config {
	return Config{
		ServerURL:      "http://localhost:4040",
		Handle:         "@agent",
		DataDir:        "./agent-data",
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

// makerConfig converts the JSON maker config into the session type, filling
// zero fields from the session default so a partial config stays sane.
func (c Config) makerConfig() session.MakerConfig {
	d := session.DefaultMakerConfig()
	m := session.MakerConfig{
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
