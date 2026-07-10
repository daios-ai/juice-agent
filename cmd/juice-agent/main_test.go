package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/daios-ai/juice-agent/juice"
)

// Compile-time guarantees that the juice client and adapters satisfy the
// interfaces the higher layers depend on. If a method drifts, this fails to
// build rather than at runtime.
var (
	_ Kernel     = (*juice.Client)(nil)
	_ Publisher  = (*juice.Client)(nil)
	_ StepSource = stepAdapter{}
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
