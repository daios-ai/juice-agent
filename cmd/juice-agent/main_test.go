package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/daios-ai/juice-agent/juice"
)

// Compile-time guarantees that the juice client satisfies the interfaces the
// higher layers depend on. If a method drifts, this fails to build rather than at
// runtime.
var (
	_ Kernel     = (*juice.Client)(nil)
	_ TaskSource = (*juice.Client)(nil)
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

// nativeReads are the kernel's action reads for the natives setup checks, with
// their canonical schemas (descriptions shortened).
var nativeReads = map[string]string{
	"sys@k/llm/chat": `{"action":"sys@k/llm/chat","description":"chat","price":0,
		"input_schema":{"type":"object","properties":{"messages":{"type":"array","description":"history","items":{"type":"object","properties":{"role":{"type":"string","description":"role"},"content":{"type":"string","description":"text"}},"required":["role","content"],"additionalProperties":false}}},"required":["messages"],"additionalProperties":false},
		"output_schema":{"type":"object","properties":{"choices":{"type":"array","description":"reply","items":{"type":"object","properties":{"message":{"type":"object","description":"the reply","properties":{"content":{"type":"string","description":"text"}},"additionalProperties":false}},"additionalProperties":false}}},"additionalProperties":false}}`,
	"sys@k/lookup": `{"action":"sys@k/lookup","description":"Semantic search over active actions","price":0,
		"input_schema":{"type":"object","properties":{"query":{"type":"string","description":"query"},"limit":{"type":"integer","description":"max"}},"required":["query"],"additionalProperties":false},
		"output_schema":{"type":"object","properties":{"results":{"type":"array","description":"ranked","items":{"type":"object","properties":{"action":{"type":"string","description":"address"},"description":{"type":"string","description":"what"},"input_schema":{"type":"object","description":"input"}},"additionalProperties":false}}},"additionalProperties":false}}`,
	"sys@k/time": `{"action":"sys@k/time","description":"Current time","price":0,
		"input_schema":{"type":"object","properties":{},"additionalProperties":false},
		"output_schema":{"type":"object","properties":{"iso":{"type":"string","description":"RFC 3339"}},"additionalProperties":false}}`,
}

// kernelServing answers GET /v1/me as address/id and the natives' action reads,
// accepting only token tok-1.
func kernelServing(t *testing.T, address, id string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthenticated","code":"unauthenticated"}`))
			return
		}
		if r.URL.Path == "/v1/actions" {
			row, ok := nativeReads[r.URL.Query().Get("ref")]
			if !ok {
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte("[" + row + "]"))
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
	exec, cl, cfg, err := setup(context.Background(), newLogger(), "agent@k")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if cl.Login() != "agent@k" || cfg.Home != filepath.Join(root, "agents", "agent@k") {
		t.Fatalf("login=%q home=%q", cl.Login(), cfg.Home)
	}
	// The loop discovers through the configured action lookup. Its terms are not
	// fixed at startup: each job reads them (Executor.Execute).
	if exec.Loop.ActionLookup != "sys@k/lookup" || len(exec.Loop.Always) != 0 {
		t.Fatalf("action lookup = %q / %+v", exec.Loop.ActionLookup, exec.Loop.Always)
	}
}

// An affordance whose action falls short of its contract stops startup, and the
// message names the file, the key, the address and the field.
func TestSetupRefusesAnAffordanceOfTheWrongShape(t *testing.T) {
	root := setJuiceHome(t)
	writeLogin(t, root, kernelServing(t, "agent@k", "u1"), "u1")
	home := filepath.Join(root, "agents", "agent@k")
	os.MkdirAll(home, 0o700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"action_lookup":"sys@k/time"}`), 0o600)
	_, _, _, err := setup(context.Background(), newLogger(), "agent@k")
	want := "config.json: action_lookup = sys@k/time: input.properties.limit: missing (want integer)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
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

func TestWrapText(t *testing.T) {
	for _, tc := range []struct {
		name, in      string
		width, indent int
		want          string
	}{
		{"fills to the width", "one two three four", 9, 0, "one two\nthree\nfour"},
		{"counts runes, not bytes", "a — b — c", 5, 0, "a — b\n— c"},
		{"indents what follows the first line", "one two three", 9, 4, "one\n    two\n    three"},
		{"keeps blank and indented lines", "one two\n\n  x y z w\nthree", 7, 0, "one two\n\n  x y z w\nthree"},
		{"keeps a long word whole", "a verylongword b", 5, 0, "a\nverylongword\nb"},
	} {
		if got := wrapText(tc.in, tc.width, tc.indent); got != tc.want {
			t.Errorf("%s: wrapText(%q, %d, %d) = %q, want %q", tc.name, tc.in, tc.width, tc.indent, got, tc.want)
		}
	}
}

// TestHelpWidth: stderr's width capped at 80, and 80 when stderr is not a terminal.
func TestHelpWidth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		err   error
		want  int
	}{
		{"narrow terminal", 60, nil, 60},
		{"wide terminal is capped", 200, nil, maxHelpWidth},
		{"not a terminal", 0, errors.New("not a terminal"), maxHelpWidth},
		{"a terminal reporting no width", 0, nil, maxHelpWidth},
	} {
		got := helpWidth(func(fd int) (int, int, error) {
			if fd != int(os.Stderr.Fd()) {
				t.Fatalf("%s: queried fd %d, want stderr", tc.name, fd)
			}
			return tc.width, 24, tc.err
		})
		if got != tc.want {
			t.Errorf("%s: width %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestUsageFitsWidth holds the usage to its width; every description line sits under
// its command at the fixed indent.
func TestUsageFitsWidth(t *testing.T) {
	for _, width := range []int{maxHelpWidth, 50} {
		text := usageText(width)
		for _, line := range strings.Split(text, "\n") {
			if n := utf8.RuneCountInString(line); n > width {
				t.Errorf("width %d: line of %d columns: %q", width, n, line)
			}
		}
		for _, c := range commands {
			if !strings.Contains(text, "  juice-agent "+c[0]+"\n      ") {
				t.Errorf("width %d: %q not followed by its indented description", width, c[0])
			}
		}
	}
}

// A serving daemon writes its process id into serve.lock and holds the lock for
// as long as it serves: a second daemon for the login is refused while the first
// serves, a reader cannot take the lock, and the lock is free once it stops.
func TestHoldHomeWritesThePID(t *testing.T) {
	home := t.TempDir()
	release, err := holdHome(home, "agent@k")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release) // a failed assertion below must not leave the home held
	path := filepath.Join(home, "serve.lock")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("serve.lock holds %q, %v; want this process's id", got, err)
	}
	if _, err := holdHome(home, "agent@k"); err == nil || !strings.Contains(err.Error(), "already serving agent@k") {
		t.Fatalf("a second daemon took the same home: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		t.Fatal("a reader took the lock while the daemon holds it")
	}
	release()
	again, err := holdHome(home, "agent@k")
	if err != nil {
		t.Fatalf("the lock is still held after release: %v", err)
	}
	again()
}
