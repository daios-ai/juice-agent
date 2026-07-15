package juice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

// fakeKernel is a minimal httptest kernel: it authenticates a fixed token and
// routes the handful of endpoints the client uses to canned JSON.
type fakeKernel struct {
	token       string
	requireAuth bool
	runResults  map[string]map[string]any // action -> result
	lastRunArgs map[string]any
	rated       map[string]any
	connectors  []map[string]any // returned under /v1/me connectors
	lastGrant   map[string]any   // last POST /v1/grants body
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{token: "tok-123", requireAuth: true, runResults: map[string]map[string]any{}, rated: map[string]any{}}
}

func (f *fakeKernel) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"token": f.token})
	})

	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if !f.requireAuth {
			return true
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeJSON(w, 401, map[string]string{"error": "unauthenticated", "code": "unauthenticated"})
			return false
		}
		return true
	}

	mux.HandleFunc("/v1/run", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var req struct {
			Action string         `json:"action"`
			Args   map[string]any `json:"args"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		f.lastRunArgs = req.Args
		res, ok := f.runResults[req.Action]
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "action failed", "code": "execution_failed"})
			return
		}
		writeJSON(w, 200, map[string]any{"result": res, "tx_id": "tx-" + req.Action, "trace_id": "tr", "process_id": "pr"})
	})

	mux.HandleFunc("/v1/actions", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		name := r.URL.Query().Get("name")
		writeJSON(w, 200, []map[string]any{
			{"id": "act-" + name, "action": "@sys/" + name, "price": 7, "active": true, "public": true, "name": name},
		})
	})

	mux.HandleFunc("/v1/stats/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, 200, map[string]any{
			"uses": 10, "successes": 9, "failures": 1, "rating_count": 4,
			"latency_estimate": 12.5, "rating_estimate": 0.75,
		})
	})

	mux.HandleFunc("/v1/transactions/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		f.rated[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/transactions/"), "/rate")] = req
		writeJSON(w, 200, map[string]any{"rating": req["rating"]})
	})

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, 200, map[string]any{"id": "u1", "handle": "@agent", "available": 500, "locked": 0,
			"connectors": f.connectors})
	})

	mux.HandleFunc("/v1/grants", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &f.lastGrant)
		writeJSON(w, 200, map[string]any{"status": "granted"})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func startFake(t *testing.T, f *fakeKernel) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(srv.URL, "@agent", "pw")
}

func TestLoginAndAuthHeader(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	// A subsequent authed call must succeed (token attached).
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("me after login: %v", err)
	}
}

func TestReLoginOn401(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	// No explicit Login: the first Me hits 401, the client logs in and retries.
	self, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if self.Handle != "@agent" || self.Available != 500 {
		t.Fatalf("self = %+v", self)
	}
}

func TestRunParsesResultAndTxID(t *testing.T) {
	f := newFakeKernel()
	f.runResults["@sys/time"] = map[string]any{"unix": float64(1), "iso": "2026-07-10T00:00:00Z"}
	c := startFake(t, f)
	rr, err := c.Run(context.Background(), "@sys/time", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rr.TxID != "tx-@sys/time" {
		t.Fatalf("tx_id = %q", rr.TxID)
	}
	if rr.Result["iso"] != "2026-07-10T00:00:00Z" {
		t.Fatalf("result = %v", rr.Result)
	}
}

func TestRunFailureIsKernelError(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	_, err := c.Run(context.Background(), "@does/not-exist", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if Code(err) != "execution_failed" {
		t.Fatalf("code = %q, want execution_failed", Code(err))
	}
	if IsRetryable(err) {
		t.Fatal("execution_failed must not be retryable")
	}
}

func TestDecideRendersToolsAndExtractsSelection(t *testing.T) {
	f := newFakeKernel()
	// Decide runs @sys/llm/chat; the model replies with prose wrapping a JSON
	// selection, which the client must extract.
	f.runResults["@sys/llm/chat"] = map[string]any{
		"message": map[string]any{"role": "assistant",
			"content": `Sure, I'll search. {"action": "@sys/web", "args": {"url": "x"}}`},
	}
	c := startFake(t, f)
	catalog := []agent.Candidate{{Ref: "@sys/web", Description: "fetch a page", Price: 3,
		Stats:       agent.ActionStats{Known: true, Uses: 4, Successes: 3},
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"}}}}
	tools := []agent.Tool{{Name: "reply", Description: "finish", InputSchema: map[string]any{"type": "object",
		"properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []any{"answer"}}}}

	sel, err := c.Decide(context.Background(), []agent.Message{{Role: agent.RoleUser, Content: "hi"}}, catalog, tools)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if sel.Action != "@sys/web" || sel.Args["url"] != "x" {
		t.Fatalf("selection = %+v", sel)
	}
	// The rendered prompt (last chat message) must list both the tool and the
	// catalog action with its stats.
	msgs, _ := f.lastRunArgs["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	prompt, _ := last["content"].(string)
	for _, want := range []string{"reply", "answer (required)", "@sys/web", "uses=4", "success=75%", "url (required)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rendered prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                 `{"a":1}`,
		"prose {\"a\":1} more":    `{"a":1}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		`{"s":"has } brace"}`:     `{"s":"has } brace"}`,
		`{"nest":{"b":2},"c":3}`:  `{"nest":{"b":2},"c":3}`,
		"no json here":            "",
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookupAndMeta(t *testing.T) {
	f := newFakeKernel()
	f.runResults["@sys/lookup"] = map[string]any{"results": []any{
		map[string]any{"action": "@sys/web", "description": "fetch", "input_schema": map[string]any{"type": "object"}},
	}}
	c := startFake(t, f)
	cands, err := c.Lookup(context.Background(), "fetch a page", 8)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(cands) != 1 || cands[0].Ref != "@sys/web" {
		t.Fatalf("candidates = %+v", cands)
	}
	price, stats, err := c.Meta(context.Background(), "@sys/web")
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if price != 7 {
		t.Fatalf("price = %d, want 7", price)
	}
	if !stats.Known || stats.Uses != 10 || stats.Successes != 9 || stats.RatingEstimate != 0.75 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRatePostsRatingAndNote(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	if err := c.Rate(context.Background(), "tx-1", 1, "worked well"); err != nil {
		t.Fatalf("rate: %v", err)
	}
	got := f.rated["tx-1"]
	m, _ := got.(map[string]any)
	if m["rating"] != float64(1) || m["note"] != "worked well" {
		t.Fatalf("rated = %v", got)
	}
}

func TestSplitRef(t *testing.T) {
	owner, name, err := splitRef("@sys/llm/decide")
	if err != nil {
		t.Fatalf("splitRef: %v", err)
	}
	if owner != "@sys" || name != "llm/decide" {
		t.Fatalf("got owner=%q name=%q", owner, name)
	}
	if _, _, err := splitRef("noslash"); err == nil {
		t.Fatal("expected error for malformed ref")
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&KernelError{Status: http.StatusTooManyRequests}, true},
		{&KernelError{Status: 500, Code: "internal"}, true},
		{&KernelError{Status: 503}, true},
		{&KernelError{Code: "timeout", Status: 504}, true},
		// Model-output 500s are NOT retryable — the loop feeds them back instead.
		{&KernelError{Status: 500, Code: "execution_failed"}, false},
		{&KernelError{Status: 422, Code: "schema_violation"}, false},
		{&KernelError{Status: 422, Code: "invalid_input"}, false},
		{&KernelError{Status: 402, Code: "insufficient_funds"}, false},
		{&KernelError{Status: 401, Code: "unauthenticated"}, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{errors.New("connection refused"), true}, // transport failure
	}
	for i, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("case %d (%v): IsRetryable = %v, want %v", i, c.err, got, c.want)
		}
	}
}

func TestConnectGrantPostsSelectorAndToken(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	if err := c.ConnectGrant(context.Background(), "@chat/msg", "key-abc"); err != nil {
		t.Fatalf("connect grant: %v", err)
	}
	if f.lastGrant["selector"] != "@chat/msg" || f.lastGrant["token"] != "key-abc" {
		t.Fatalf("grant body = %v", f.lastGrant)
	}
}

func TestMeDecodesConnectors(t *testing.T) {
	f := newFakeKernel()
	f.connectors = []map[string]any{{
		"directory": "@chat/msg",
		"actions":   []map[string]any{{"action": "@chat/msg/read"}, {"action": "@chat/msg/message"}},
	}}
	c := startFake(t, f)
	self, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if len(self.Connectors) != 1 || self.Connectors[0].Directory != "@chat/msg" {
		t.Fatalf("connectors = %+v", self.Connectors)
	}
	if len(self.Connectors[0].Actions) != 2 || self.Connectors[0].Actions[0].Action != "@chat/msg/read" {
		t.Fatalf("connector actions = %+v", self.Connectors[0].Actions)
	}
}

func TestIsGrantRequired(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&KernelError{Status: 403, Code: "grant_required"}, true},
		{&KernelError{Status: 403, Code: ""}, true}, // defensive status fallback
		{&KernelError{Status: 401, Code: "unauthenticated"}, false},
		{&KernelError{Status: 500, Code: "internal"}, false},
		{errors.New("plain"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := IsGrantRequired(c.err); got != c.want {
			t.Errorf("case %d (%v): IsGrantRequired = %v, want %v", i, c.err, got, c.want)
		}
	}
}

func TestCodeExtraction(t *testing.T) {
	if got := Code(&KernelError{Code: "not_found"}); got != "not_found" {
		t.Fatalf("Code = %q", got)
	}
	if got := Code(errors.New("plain")); got != "" {
		t.Fatalf("Code of non-kernel error = %q, want empty", got)
	}
}

func TestKernelErrorMessage(t *testing.T) {
	e := &KernelError{Status: 422, Code: "invalid_input", Message: "bad args"}
	if e.Error() != "invalid_input: bad args" {
		t.Fatalf("Error() = %q", e.Error())
	}
	e2 := &KernelError{Status: 500, Message: "boom"}
	if e2.Error() != "http 500: boom" {
		t.Fatalf("Error() = %q", e2.Error())
	}
}
