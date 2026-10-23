package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

// First start writes config.json whole: $schema first, every key at its default,
// and the two affordances as the natives of the login's own kernel.
func TestFirstStartWritesEveryKey(t *testing.T) {
	root := setJuiceHome(t)
	cfg, err := ResolveConfig("research@work")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantHome := filepath.Join(root, "agents", "research@work")
	if cfg.Login != "research@work" || cfg.Home != wantHome || cfg.PersonasDir != filepath.Join(wantHome, "personas") {
		t.Fatalf("login=%q home=%q personas=%q", cfg.Login, cfg.Home, cfg.PersonasDir)
	}
	if cfg.MaxTurns != 32 || cfg.DefaultBudget != 100 || cfg.PollSeconds != 5 || cfg.Concurrency != 4 || !cfg.RateTasks {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	// The affordances default to the natives of the login's own kernel.
	if cfg.Reason != "sys@work/llm/chat" || cfg.ActionLookup != "sys@work/lookup" {
		t.Fatalf("reason=%q action_lookup=%q", cfg.Reason, cfg.ActionLookup)
	}
	raw, err := os.ReadFile(filepath.Join(wantHome, "config.json"))
	if err != nil || !strings.HasPrefix(string(raw), "{\n  \"$schema\": \"../../schemas/juice-agent.schema.json\",") {
		t.Fatalf("config.json = %q, %v; want $schema first", raw, err)
	}
	var file map[string]any
	json.Unmarshal(raw, &file)
	if len(file) != 9 || file["reason"] != "sys@work/llm/chat" || file["max_turns"] != 32.0 || file["rate_tasks"] != true {
		t.Fatalf("config.json = %v; want every key", file)
	}
}

// The schema is generated from the defaults: every setting described and, but for
// the two affordances first start creates, stating its default.
func TestTheSchemaStatesEveryDefault(t *testing.T) {
	root := setJuiceHome(t)
	if _, err := ResolveConfig("a@k"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "schemas", "juice-agent.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           map[string]map[string]any
	}
	json.Unmarshal(raw, &schema)
	var defaults map[string]any
	b, _ := json.Marshal(DefaultConfig())
	json.Unmarshal(b, &defaults)
	if schema.AdditionalProperties || len(schema.Properties) != len(defaults) {
		t.Fatalf("schema = %s", raw)
	}
	for key, prop := range schema.Properties {
		_, has := prop["default"]
		if prop["description"] == "" || has == (key == "reason" || key == "action_lookup") ||
			(has && !reflect.DeepEqual(prop["default"], defaults[key])) {
			t.Errorf("%s: %v; default %v", key, prop, defaults[key])
		}
	}
}

// A file written before the rule, or by hand, gets exactly the keys first start
// creates written in; the operator's keys are kept and no default is added. The
// file is then left alone.
func TestAPartialFileGetsOnlyTheCreatedKeys(t *testing.T) {
	root := setJuiceHome(t)
	writeAgentConfig(t, root, "a@k", `{"max_turns": 7}`)
	cfg, err := ResolveConfig("a@k")
	if err != nil || cfg.MaxTurns != 7 || cfg.Reason != "sys@k/llm/chat" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	path := filepath.Join(root, "agents", "a@k", "config.json")
	got, _ := os.ReadFile(path)
	want := "{\n  \"$schema\": \"../../schemas/juice-agent.schema.json\",\n  \"action_lookup\": \"sys@k/lookup\",\n  \"max_turns\": 7,\n  \"reason\": \"sys@k/llm/chat\"\n}\n"
	if string(got) != want {
		t.Fatalf("config.json = %s, want %s", got, want)
	}
	os.WriteFile(path, []byte(`{"$schema": "s", "reason": "b@k/r", "action_lookup": "b@k/l"}`), 0o600)
	if _, err := ResolveConfig("a@k"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"$schema": "s", "reason": "b@k/r", "action_lookup": "b@k/l"}` {
		t.Fatalf("a complete file was rewritten: %s", got)
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
	writeAgentConfig(t, root, "a@k", `{"max_turns": 7, "reason": "brain@k/think", "poll_seconds": 0, "concurrency": -1}`)
	cfg, err := ResolveConfig("a@k")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurns != 7 || cfg.DefaultBudget != 100 || cfg.Reason != "brain@k/think" || cfg.ActionLookup != "sys@k/lookup" {
		t.Fatalf("cfg = %+v, want max_turns 7 and reason swapped, the rest defaulted", cfg)
	}
	if cfg.PollSeconds != 5 || cfg.Concurrency != 4 {
		t.Fatalf("poll=%d concurrency=%d, want the defaults restored", cfg.PollSeconds, cfg.Concurrency)
	}
}

func TestResolveConfigRejectsUnknownKeysAndMalformedJSON(t *testing.T) {
	root := setJuiceHome(t)
	// Every key retired in 0.4.0 is reported, never silently ignored.
	for _, body := range []string{`{"server_url": "http://x"}`, `{"channels": {"task": {}}}`, `{"memory_bytes": 0}`,
		`{"maker": {}}`, `{"max_repeats": 3}`, `{"candidate_limit": 9}`, `{"lookup_limit": 9}`, `{"lookup": "sys@k/lookup"}`, `{"reason": ""}`, `{"action_lookup": ""}`, `{not json`} {
		writeAgentConfig(t, root, "a@k", body)
		if _, err := ResolveConfig("a@k"); err == nil {
			t.Errorf("config %s accepted, want an error", body)
		}
	}
}

// A null, a key of another case and a value of the wrong type are each refused
// naming the key.
func TestRefusalsNameTheKey(t *testing.T) {
	root := setJuiceHome(t)
	for body, key := range map[string]string{`{"max_turns": null}`: "max_turns", `{"MAX_TURNS": 7}`: "MAX_TURNS", `{"max_turns": "7"}`: "max_turns"} {
		writeAgentConfig(t, root, "a@k", body)
		if _, err := ResolveConfig("a@k"); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("config %s: err = %v, want one naming %s", body, err, key)
		}
	}
}

// A file that is not an object is refused, and a refused file is left as it was:
// the created keys are written in only once the whole file has passed.
func TestARefusedFileIsNotRewritten(t *testing.T) {
	root := setJuiceHome(t)
	for _, body := range []string{`null`, `{"max_turns": "7"}`, `{"reason": ""}`} {
		writeAgentConfig(t, root, "a@k", body)
		if _, err := ResolveConfig("a@k"); err == nil {
			t.Errorf("config %s accepted", body)
		}
		if got, _ := os.ReadFile(filepath.Join(root, "agents", "a@k", "config.json")); string(got) != body {
			t.Errorf("refused config %s was rewritten to %s", body, got)
		}
	}
}

func TestEnsureHomeCreatesAgentFileAndPersonas(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(cfg.agentPath()); err != nil || len(b) != 0 {
		t.Fatalf("agent.md: %q, %v; want an empty file", b, err)
	}
	if fi, err := os.Stat(cfg.PersonasDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("personas dir: %v %v, want mode 0700", fi, err)
	}
}

func TestEnsureHomeNeverTruncates(t *testing.T) {
	setJuiceHome(t)
	cfg, _ := ResolveConfig("a@k")
	cfg.EnsureHome()
	os.WriteFile(cfg.agentPath(), []byte("you are terse"), 0o600)
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(cfg.agentPath()); string(got) != "you are terse" {
		t.Fatalf("agent.md truncated: %q", got)
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
