package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/daios-ai/juice-agent/juice"
)

// Config is a single agent's configuration, as the ecosystem standard configures
// every component (ecosystem §3): one config.json in the agent's home,
// $JUICE_HOME/agents/<login>/, laid over the defaults, and described by a JSON
// Schema generated from them (§11 of requirements.md). Its keys are flat, so
// decoding the file onto the defaults is the standard's merge. Secrets never live
// here: the session is the login's, in $JUICE_HOME/client/credentials/.
//
// Reason and ActionLookup are the two affordances the harness depends on, named
// by address so either can be swapped for any action meeting its contract, which
// the daemon checks at startup (juice/contract.go). No other action is named in
// configuration or code.
type Config struct {
	Schema        string `json:"$schema" doc:"The JSON Schema describing this file, for editors."`
	Reason        string `json:"reason" default:"-" doc:"Reasoning action: any action meeting the reason contract. First start writes the login's kernel's llm/chat."`
	ActionLookup  string `json:"action_lookup" default:"-" doc:"Action the agent uses to find tools: any action meeting the lookup contract. First start writes the login's kernel's lookup."`
	DefaultBudget int    `json:"default_budget" doc:"Spending budget per job, in base units, model calls included."`
	MaxTurns      int    `json:"max_turns" doc:"Maximum reasoning turns per job."`
	Concurrency   int    `json:"concurrency" doc:"Number of conversations the daemon serves at once; a non-positive value takes the default."`
	PollSeconds   int    `json:"poll_seconds" doc:"Seconds between checks for waiting tasks; a non-positive value takes the default."`
	CompactBytes  int    `json:"compact_bytes" doc:"Conversation size in bytes that triggers summarisation of older turns."`
	RateTasks     bool   `json:"rate_tasks" doc:"Whether to rate the tools a job called after it ends."`

	// Computed at resolution time from the login and $JUICE_HOME; never JSON.
	Login       string `json:"-"`
	Home        string `json:"-"`
	PersonasDir string `json:"-"`
}

// DefaultConfig is every setting's default, and the one place each is stated: the
// schema publishes it and a key absent from the file keeps it. The two affordances
// have none: their natural value names the login's kernel, which one schema for
// every agent cannot state, so first start writes them (ResolveConfig).
func DefaultConfig() Config {
	return Config{
		Schema:        "../../schemas/juice-agent.schema.json", // agents/<login>/config.json → schemas/
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
// login, creates it, writes the schema, and reads config.json over the defaults.
// First start writes the file whole. A file that lacks a key first start creates
// ($schema and the two affordances: a file older than the rule, or one written by
// hand) gets exactly those written in, every other key kept and no default added.
// A key the schema does not name (matched exactly: the decoder alone would accept
// any case), a null, a value of the wrong type or malformed JSON stops the start,
// naming the key. A non-positive poll interval or worker count falls back to its
// default: intake always runs.
func ResolveConfig(login string) (Config, error) {
	_, kernel, err := juice.ParseLogin(login)
	if err != nil {
		return Config{}, err
	}
	def := DefaultConfig()
	cfg := def
	cfg.Login = login
	cfg.Home = filepath.Join(juiceHome(), "agents", login)
	cfg.PersonasDir = filepath.Join(cfg.Home, "personas")
	if err := cfg.EnsureHome(); err != nil {
		return cfg, fmt.Errorf("ensure home %s: %w", cfg.Home, err)
	}
	schema, err := writeSchema()
	if err != nil {
		return cfg, err
	}

	path := filepath.Join(cfg.Home, "config.json")
	created := map[string]string{"$schema": def.Schema, "reason": "sys@" + kernel + "/llm/chat", "action_lookup": "sys@" + kernel + "/lookup"}
	first := def
	first.Reason, first.ActionLookup = created["reason"], created["action_lookup"]
	b, _ := json.MarshalIndent(first, "", "  ")
	if err := writeFile(path, append(b, '\n'), false); err != nil {
		return cfg, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	file := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &file); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if file == nil { // the decoder takes a bare null as an empty map
		return cfg, fmt.Errorf("%s: not a JSON object", path)
	}
	props := schema["properties"].(map[string]any)
	for key, v := range file {
		if props[key] == nil {
			return cfg, fmt.Errorf("%s: unknown key %q", path, key)
		}
		if string(v) == "null" {
			return cfg, fmt.Errorf("%s: %s is null", path, key)
		}
	}
	missing := false
	for key, v := range created {
		if _, ok := file[key]; !ok {
			file[key], _ = json.Marshal(v)
			missing = true
		}
	}
	b, _ = json.Marshal(file)
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Reason == "" || cfg.ActionLookup == "" {
		return cfg, fmt.Errorf("%s: reason and action_lookup must name actions", path)
	}
	if missing { // only a file that passed; sorted keys put $schema first
		b, _ = json.MarshalIndent(file, "", "  ")
		if err := writeFile(path, append(b, '\n'), true); err != nil {
			return cfg, err
		}
	}
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = def.PollSeconds
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = def.Concurrency
	}
	return cfg, nil
}

// writeSchema writes config.json's JSON Schema to $JUICE_HOME/schemas/, every time,
// since it describes this binary (ecosystem §3). It is all setup writes: juice-agent
// ships no defaults, and an agent's home is first start's.
func writeSchema() (map[string]any, error) {
	schema, err := configSchema()
	if err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(schema, "", "  ")
	if err := writeFile(filepath.Join(juiceHome(), "schemas", "juice-agent.schema.json"), append(b, '\n'), true); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return schema, nil
}

// configSchema describes config.json as a JSON Schema in the kernel's subset (kernel
// D4), generated from DefaultConfig so schema, file and program cannot disagree: a
// property's type is its field's kind, its description the doc tag, and its default
// the value DefaultConfig holds, unless tagged default:"-". A field without a doc
// tag, or of a kind other than string, integer and boolean, is an error, so a
// setting cannot ship undescribed.
func configSchema() (map[string]any, error) {
	def := reflect.ValueOf(DefaultConfig())
	types := map[reflect.Kind]string{reflect.String: "string", reflect.Int: "integer", reflect.Bool: "boolean"}
	props := map[string]any{}
	for i := 0; i < def.NumField(); i++ {
		f := def.Type().Field(i)
		key := f.Tag.Get("json")
		if key == "-" {
			continue
		}
		typ, doc := types[f.Type.Kind()], f.Tag.Get("doc")
		if key == "" || typ == "" || doc == "" {
			return nil, fmt.Errorf("config field %s needs a json key, a doc tag and a flat type", f.Name)
		}
		prop := map[string]any{"type": typ, "description": doc}
		if f.Tag.Get("default") != "-" {
			prop["default"] = def.Field(i).Interface()
		}
		props[key] = prop
	}
	return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "title": "A juice-agent's config.json", "type": "object", "properties": props, "additionalProperties": false}, nil
}

// writeFile writes b to path through a temporary file beside it, so a reader never
// sees half of it. With replace it renames over path; without, it links, which
// creates path only if absent, so a file someone wrote first is kept.
func writeFile(path string, b []byte, replace bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if replace {
		return os.Rename(tmp.Name(), path)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// EnsureHome creates the agent home and its personas directory (mode 0700) and
// writes a blank agent.md if absent, so the operator always has a concrete file
// to edit. It never truncates an existing file.
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
