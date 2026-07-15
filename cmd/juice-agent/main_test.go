package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/daios-ai/juice-agent/channels"
	"github.com/daios-ai/juice-agent/juice"
)

// Compile-time guarantees that the juice client and adapters satisfy the
// interfaces the higher layers depend on. If a method drifts, this fails to
// build rather than at runtime.
var (
	_ Kernel                 = (*juice.Client)(nil)
	_ Publisher              = (*juice.Client)(nil)
	_ channels.StepSource    = stepAdapter{}
	_ channels.ServiceActions = (*chatAdapter)(nil)
)

func TestLoggerEmitsStructuredJSON(t *testing.T) {
	// Redirect stderr to capture the log line.
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	l := newLogger()
	l.event("dispatch", map[string]any{"action": "@sys/time", "price": 0})
	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["event"] != "dispatch" || rec["action"] != "@sys/time" {
		t.Fatalf("log record = %v", rec)
	}
	if _, ok := rec["time"]; !ok {
		t.Fatal("log record missing time")
	}
}

func TestResolvePasswordStdinFlag(t *testing.T) {
	// --password-stdin reads one line from stdin, stripping the trailing newline.
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	w.WriteString("s3cret\n")
	w.Close()

	pw, err := resolvePassword("@agent", true)
	if err != nil {
		t.Fatalf("stdin password: %v", err)
	}
	if pw != "s3cret" {
		t.Fatalf("password = %q, want s3cret", pw)
	}
}

func TestResolvePasswordEnvAndFile(t *testing.T) {
	t.Setenv("AGENT_PASSWORD_FILE", "")
	t.Setenv("AGENT_PASSWORD", "from-env")
	if pw, err := resolvePassword("@agent", false); err != nil || pw != "from-env" {
		t.Fatalf("env password = %q err=%v", pw, err)
	}

	// File source is consulted when AGENT_PASSWORD is unset; whitespace trimmed.
	t.Setenv("AGENT_PASSWORD", "")
	f := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(f, []byte("  from-file\n"), 0o600)
	t.Setenv("AGENT_PASSWORD_FILE", f)
	if pw, err := resolvePassword("@agent", false); err != nil || pw != "from-file" {
		t.Fatalf("file password = %q err=%v", pw, err)
	}
}

func TestResolvePasswordNoSourceNonTTYErrors(t *testing.T) {
	// With no flag, no env, and stdin not a terminal (a pipe under `go test`),
	// resolution must fail rather than block.
	t.Setenv("AGENT_PASSWORD", "")
	t.Setenv("AGENT_PASSWORD_FILE", "")
	if _, err := resolvePassword("@agent", false); err == nil {
		t.Fatal("expected an error when no password source is available")
	}
}

func TestExtractFlagIsPositionIndependent(t *testing.T) {
	set := false
	got := extractFlag([]string{"task", "--password-stdin", "hello"}, "--password-stdin", &set)
	if !set {
		t.Fatal("flag not detected")
	}
	if !reflect.DeepEqual(got, []string{"task", "hello"}) {
		t.Fatalf("args after strip = %v", got)
	}
	set = false
	got = extractFlag([]string{"serve"}, "--password-stdin", &set)
	if set || !reflect.DeepEqual(got, []string{"serve"}) {
		t.Fatalf("absent flag mishandled: set=%v got=%v", set, got)
	}
}
