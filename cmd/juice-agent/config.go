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
//
// Reason and ActionLookup are the two affordances the harness depends on, named
// by address so either can be swapped for any action meeting its contract, which
// the daemon checks at startup (juice/contract.go). No other action is named in
// configuration or code.
type Config struct {
	Reason        string `json:"reason"`
	ActionLookup  string `json:"action_lookup"`
	DefaultBudget int    `json:"default_budget"`
	MaxTurns      int    `json:"max_turns"`
	Concurrency   int    `json:"concurrency"`
	PollSeconds   int    `json:"poll_seconds"`
	CompactBytes  int    `json:"compact_bytes"`
	RateTasks     bool   `json:"rate_tasks"`

	// Computed at resolution time from the login and $JUICE_HOME; never JSON.
	Login       string `json:"-"`
	Home        string `json:"-"`
	PersonasDir string `json:"-"`
}

// DefaultConfig is the safe baseline applied when a key is absent. The two
// affordances default to the kernel's own natives on the login's kernel, and
// identity and paths are computed by ResolveConfig, not here.
func DefaultConfig(kernel string) Config {
	return Config{
		Reason:        "sys@" + kernel + "/llm/chat",
		ActionLookup:  "sys@" + kernel + "/lookup",
		DefaultBudget: 100,
		MaxTurns:      32,
		Concurrency:   4,
		PollSeconds:   5,
		CompactBytes:  262144,
		RateTasks:     true,
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

// ResolveConfig computes the agent home ($JUICE_HOME/agents/<login>/) from the
// login and decodes its config.json over the defaults (a key it omits keeps its
// default). A missing file is fine; a malformed file or an unknown key is an
// error, so a retired key is reported rather than ignored. A non-positive poll
// interval or worker count falls back to its default: intake always runs.
func ResolveConfig(login string) (Config, error) {
	_, kernel, err := juice.ParseLogin(login)
	if err != nil {
		return Config{}, err
	}
	def := DefaultConfig(kernel)
	cfg := def
	cfg.Login = login
	cfg.Home = filepath.Join(juiceHome(), "agents", login)
	cfg.PersonasDir = filepath.Join(cfg.Home, "personas")
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
	if cfg.Reason == "" || cfg.ActionLookup == "" {
		return cfg, fmt.Errorf("%s: reason and action_lookup must name actions", path)
	}
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = def.PollSeconds
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = def.Concurrency
	}
	return cfg, nil
}

// EnsureHome creates the agent home and its personas directory (mode 0700) and
// writes a blank agent.md if absent, so the operator always has a concrete file
// to edit. It never truncates an existing file and writes no config.
func (c Config) EnsureHome() error {
	if err := os.MkdirAll(c.PersonasDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.agentPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return f.Close()
}

// agentPath is agent.md, the one human-authored file: persona and knowledge,
// read at every job start and never written by the daemon.
func (c Config) agentPath() string { return filepath.Join(c.Home, "agent.md") }
