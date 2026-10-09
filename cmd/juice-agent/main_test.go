package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/channels"
	"github.com/daios-ai/juice-agent/juice"
)

// Compile-time guarantees that the juice client and adapters satisfy the
// interfaces the higher layers depend on. If a method drifts, this fails to
// build rather than at runtime. kernelAdapter is the single kernel bridge for
// every channel; satisfying channels.Kernel also satisfies channels.TaskSource
// (embedded), so the task channel is covered too.
var (
	_ Kernel          = (*juice.Client)(nil)
	_ Publisher       = (*juice.Client)(nil)
	_ channels.Kernel = kernelAdapter{}
)

func TestLoggerEmitsStructuredJSON(t *testing.T) {
	// Redirect stderr to capture the log line.
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	l := newLogger()
	l.event("dispatch", map[string]any{"action": "sys@k/time", "price": 0})
	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["event"] != "dispatch" || rec["action"] != "sys@k/time" {
		t.Fatalf("log record = %v", rec)
	}
	if _, ok := rec["time"]; !ok {
		t.Fatal("log record missing time")
	}
}

// kernelServing answers GET /v1/me as address/id, accepting only token tok-1.
func kernelServing(t *testing.T, address, id string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthenticated","code":"unauthenticated"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "address": address})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeLogin writes the juice CLI's records for agent@k under root.
func writeLogin(t *testing.T, root, endpoint, principal string) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, "client", "credentials"), 0o700)
	cfg, _ := json.Marshal(map[string]any{"kernels": map[string]any{"k": map[string]any{"endpoint": endpoint, "public_key": "KEY"}}})
	os.WriteFile(filepath.Join(root, "client", "config.json"), cfg, 0o600)
	cred, _ := json.Marshal(map[string]any{"token": "tok-1", "principal_id": principal})
	os.WriteFile(filepath.Join(root, "client", "credentials", "agent@k.json"), cred, 0o600)
}

func TestSetupAcceptsTheLoginsOwnAccount(t *testing.T) {
	root := setJuiceHome(t)
	writeLogin(t, root, kernelServing(t, "agent@k", "u1"), "u1")
	_, cl, cfg, err := setup(context.Background(), newLogger(), "agent@k")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if cl.Sys("lookup") != "sys@k/lookup" || cfg.Home != filepath.Join(root, "agents", "agent@k") {
		t.Fatalf("lookup=%q home=%q", cl.Sys("lookup"), cfg.Home)
	}
}

func TestSetupRefusesAnotherAccount(t *testing.T) {
	for _, tc := range []struct{ address, id string }{{"other@k", "u1"}, {"agent@k", "u2"}} {
		root := setJuiceHome(t)
		writeLogin(t, root, kernelServing(t, tc.address, tc.id), "u1")
		_, _, _, err := setup(context.Background(), newLogger(), "agent@k")
		if err == nil || !strings.Contains(err.Error(), "juice auth login agent@k") {
			t.Fatalf("%+v: err = %v, want a refusal naming juice auth login", tc, err)
		}
	}
}

func TestSetupWithoutSessionNamesTheLoginCommand(t *testing.T) {
	root := setJuiceHome(t)
	writeLogin(t, root, "http://127.0.0.1:1", "u1")
	os.Remove(filepath.Join(root, "client", "credentials", "agent@k.json"))
	_, _, _, err := setup(context.Background(), newLogger(), "agent@k")
	if err == nil || !strings.Contains(err.Error(), "juice auth login agent@k") {
		t.Fatalf("err = %v", err)
	}
}
