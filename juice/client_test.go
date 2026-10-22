package juice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/daios-ai/juice-agent/agent"
)

// fakeKernel is a minimal httptest kernel routing the endpoints the client uses
// to canned JSON. It accepts one access token and one refresh token at a time.
type fakeKernel struct {
	token      string // the access token the authed check accepts
	refreshTok string // the refresh token the refresh endpoint accepts
	refreshes  int
	authDown   bool // the refresh endpoint answers 503

	runResults map[string]map[string]any // action -> result
	runErrs    map[string]map[string]any // action -> error body
	lastRun    map[string]any            // last POST /v1/run body
	actions    map[string]map[string]any // ref -> GET /v1/actions?ref= row
	rated      map[string]any
	tasks      []map[string]any
	completed  map[string]any // task id -> completion body
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{token: "tok-1", refreshTok: "ref-1",
		runResults: map[string]map[string]any{}, runErrs: map[string]map[string]any{},
		actions: map[string]map[string]any{}, rated: map[string]any{}, completed: map[string]any{}}
}

func readBody(r *http.Request) map[string]any {
	var m map[string]any
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &m)
	return m
}

func (f *fakeKernel) handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				writeJSON(w, 401, map[string]string{"error": "unauthenticated", "code": "unauthenticated"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		if f.authDown {
			writeJSON(w, 503, map[string]string{"error": "unavailable", "code": "internal"})
			return
		}
		if readBody(r)["refresh_token"] != f.refreshTok {
			writeJSON(w, 401, map[string]string{"error": "revoked", "code": "unauthenticated"})
			return
		}
		f.refreshes++
		f.refreshTok = "ref-rotated"
		writeJSON(w, 200, map[string]string{"access_token": f.token, "refresh_token": f.refreshTok})
	})
	mux.HandleFunc("/v1/run", authed(func(w http.ResponseWriter, r *http.Request) {
		f.lastRun = readBody(r)
		action, _ := f.lastRun["action"].(string)
		if e, ok := f.runErrs[action]; ok {
			writeJSON(w, 409, e)
			return
		}
		res, ok := f.runResults[action]
		if !ok {
			writeJSON(w, 500, map[string]string{"error": "action failed", "code": "execution_failed"})
			return
		}
		writeJSON(w, 200, map[string]any{"result": res, "tx_id": "tx-" + action, "charge": 3})
	}))
	mux.HandleFunc("/v1/actions", authed(func(w http.ResponseWriter, r *http.Request) {
		if ref := r.URL.Query().Get("ref"); ref != "" && r.Method == http.MethodGet {
			if row, ok := f.actions[ref]; ok {
				writeJSON(w, 200, []any{row})
			} else {
				writeJSON(w, 200, []any{})
			}
			return
		}
		writeJSON(w, 400, map[string]string{"error": "want ?ref=", "code": "invalid_input"})
	}))
	mux.HandleFunc("/v1/transactions/", authed(func(w http.ResponseWriter, r *http.Request) {
		f.rated[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/transactions/"), "/rate")] = readBody(r)
		writeJSON(w, 200, map[string]any{})
	}))
	mux.HandleFunc("/v1/me", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": "u1", "address": "agent@k", "available": 500})
	}))
	mux.HandleFunc("/v1/tasks", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "waiting" {
			writeJSON(w, 400, map[string]string{"error": "want status=waiting", "code": "invalid_input"})
			return
		}
		// Paged as the API pages every list: limit defaults to 50, ceiling 200.
		limit, offset := 50, 0
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			limit = min(v, 200)
		}
		if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil {
			offset = max(v, 0)
		}
		page := f.tasks[min(offset, len(f.tasks)):min(offset+limit, len(f.tasks))]
		writeJSON(w, 200, page)
	}))
	mux.HandleFunc("/v1/tasks/", authed(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/complete")
		f.completed[id] = readBody(r)
		writeJSON(w, 200, map[string]any{"result": map[string]any{}, "tx_id": "tx-" + id})
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeRecords writes the juice CLI's client records for login agent@k under a
// temp $JUICE_HOME: the kernel's endpoint and key, and the login's credentials.
func writeRecords(t *testing.T, endpoint string, cred map[string]any) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "client", "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"current": "", "kernels": map[string]any{
		"k": map[string]any{"endpoint": endpoint, "public_key": "KEY"}}}
	b, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(home, "client", "config.json"), b, 0o600)
	if cred != nil {
		b, _ = json.Marshal(cred)
		os.WriteFile(filepath.Join(home, "client", "credentials", "agent@k.json"), b, 0o600)
	}
	return home
}

// The configured affordances in tests. They are deliberately not the kernel's
// default natives, so a test passing proves the client names no other action.
const (
	testReason = "brain@k/think"
	testLookup = "index@k/find"
)

// startFake starts f and returns a client for agent@k holding tok-1/ref-1.
func startFake(t *testing.T, f *fakeKernel) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	home := writeRecords(t, srv.URL, map[string]any{"token": "tok-1", "refresh_token": "ref-1", "principal_id": "u1"})
	c, err := New("agent@k", home, testReason, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseLogin(t *testing.T) {
	h, k, err := ParseLogin("agent@work")
	if err != nil || h != "agent" || k != "work" {
		t.Fatalf("ParseLogin = %q %q %v", h, k, err)
	}
	for _, bad := range []string{"agent", "agent@", "@work", "a@b@c", "a/b@work", ".agent@work", "agent@wo rk", ""} {
		if _, _, err := ParseLogin(bad); err == nil {
			t.Errorf("ParseLogin(%q) accepted, want an error", bad)
		}
	}
}

func TestNewReadsRecords(t *testing.T) {
	c := startFake(t, newFakeKernel())
	if c.Login() != "agent@k" || c.Principal() != "u1" {
		t.Fatalf("login=%q principal=%q", c.Login(), c.Principal())
	}
}

func TestNewRefusesUnrecordedKernelAndMissingSession(t *testing.T) {
	home := writeRecords(t, "http://x", nil)
	if _, err := New("agent@other", home, testReason, testLookup); err == nil || !strings.Contains(err.Error(), "juice kernel add") {
		t.Fatalf("unrecorded kernel: err = %v", err)
	}
	if _, err := New("agent@k", home, testReason, testLookup); !errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), "juice auth login agent@k") {
		t.Fatalf("missing session: err = %v", err)
	}
}

func TestRefreshOn401RotatesAndPersists(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	f.token = "tok-2" // the held token expired server-side
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("me after expiry: %v", err)
	}
	if f.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshes)
	}
	cred := readCredentials(c.credPath)
	if cred["token"] != "tok-2" || cred["refresh_token"] != "ref-rotated" || cred["principal_id"] != "u1" {
		t.Fatalf("credential file after rotation = %v", cred)
	}
}

func TestRefreshAdoptsTokenAnotherProcessRotated(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	f.token = "tok-3"
	// Another process sharing the login already rotated and stored tok-3.
	b, _ := json.Marshal(map[string]any{"token": "tok-3", "refresh_token": "ref-other", "principal_id": "u1"})
	os.WriteFile(c.credPath, b, 0o600)
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("me: %v", err)
	}
	if f.refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0 (the stored token is adopted)", f.refreshes)
	}
}

func TestRefreshRefusesAnotherPrincipalsSession(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	f.token = "tok-3"
	// A later login under the same label stored another account's session.
	b, _ := json.Marshal(map[string]any{"token": "tok-3", "refresh_token": "ref-other", "principal_id": "u2"})
	os.WriteFile(c.credPath, b, 0o600)
	_, err := c.Me(context.Background())
	if !errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), "restart juice-agent") {
		t.Fatalf("err = %v, want a refusal naming the restart", err)
	}
	if c.currentToken() != "tok-1" || f.refreshes != 0 {
		t.Fatalf("token = %q refreshes = %d, want the old session kept and nothing rotated", c.currentToken(), f.refreshes)
	}
	if cred := readCredentials(c.credPath); cred["refresh_token"] != "ref-other" {
		t.Fatalf("credential file rewritten: %v", cred)
	}
}

func TestRevokedRefreshIsNoSession(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	f.token, f.refreshTok = "tok-2", "ref-revoked"
	_, err := c.Me(context.Background())
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

func TestRefreshServerFailureIsRetryableNotNoSession(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	f.token, f.authDown = "tok-2", true
	_, err := c.Me(context.Background())
	if errors.Is(err, ErrNoSession) || !IsRetryable(err) {
		t.Fatalf("err = %v, want a retryable failure that keeps the session", err)
	}
}

func TestRunPinsQuoteAndRecognisesMovedTerms(t *testing.T) {
	f := newFakeKernel()
	f.runResults["sys@k/time"] = map[string]any{"iso": "2026-07-10T00:00:00Z"}
	f.runErrs["x@k/y"] = map[string]any{"error": "terms moved", "code": "terms_changed", "meta": map[string]any{"quote_hash": "q2"}}
	c := startFake(t, f)

	rr, err := c.Run(context.Background(), "sys@k/time", nil, "q1", "key-1")
	if err != nil || rr.TxID != "tx-sys@k/time" || rr.Charge != 3 || rr.Result["iso"] != "2026-07-10T00:00:00Z" {
		t.Fatalf("run = %+v, %v", rr, err)
	}
	if f.lastRun["quote_hash"] != "q1" || f.lastRun["external_key"] != "key-1" {
		t.Fatalf("run body = %v, want quote_hash q1 and external_key key-1", f.lastRun)
	}
	if args, ok := f.lastRun["args"].(map[string]any); !ok || len(args) != 0 {
		t.Fatalf("args = %v, want {} for nil args", f.lastRun["args"])
	}
	c.Run(context.Background(), "sys@k/time", nil, "", "")
	if _, has := f.lastRun["quote_hash"]; has {
		t.Fatal("an empty quote must not be sent")
	}
	if _, has := f.lastRun["external_key"]; has {
		t.Fatal("an empty key must not be sent")
	}

	_, err = c.Run(context.Background(), "x@k/y", nil, "q1", "")
	if !errors.Is(err, agent.ErrTermsChanged) {
		t.Fatalf("err = %v, want agent.ErrTermsChanged", err)
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Meta["quote_hash"] != "q2" {
		t.Fatalf("meta not decoded: %+v", ke)
	}
}

func TestRunFailureReturnsItsSettlement(t *testing.T) {
	f := newFakeKernel()
	f.runErrs["x@k/fail"] = map[string]any{"error": "boom", "code": "execution_failed",
		"meta": map[string]any{"tx_id": "tx-f", "charge": 2}}
	c := startFake(t, f)
	rr, err := c.Run(context.Background(), "x@k/fail", nil, "", "")
	if err == nil || rr.TxID != "tx-f" || rr.Charge != 2 {
		t.Fatalf("run = %+v, %v, want the failure with tx-f charged 2", rr, err)
	}
}

func TestRunFailureIsKernelError(t *testing.T) {
	c := startFake(t, newFakeKernel())
	_, err := c.Run(context.Background(), "does@k/not-exist", nil, "", "")
	if Code(err) != "execution_failed" || IsRetryable(err) || errors.Is(err, agent.ErrTermsChanged) {
		t.Fatalf("err = %v", err)
	}
}

// at is a Call running ref through the client, unpinned and unkeyed: what the
// executor's metered call does underneath.
func at(c *Client, ref string) Call {
	return func(ctx context.Context, args map[string]any) (map[string]any, error) {
		rr, err := c.Run(ctx, ref, args, "", "")
		return rr.Result, err
	}
}

// chatReply is the llm/chat result shape carrying content as the reply.
func chatReply(content string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": content}}}}
}

func TestChatAndChatJSONRunTheReasonAffordance(t *testing.T) {
	f := newFakeKernel()
	f.runResults[testReason] = chatReply("hello")
	c := startFake(t, f)
	ctx := context.Background()
	msgs := []agent.Message{{Role: agent.RoleUser, Content: "hi"}}

	if got, err := Chat(ctx, at(c, testReason), msgs); err != nil || got != "hello" || f.lastRun["action"] != testReason {
		t.Fatalf("chat = %q, %v, ran %v", got, err, f.lastRun["action"])
	}
	// ChatJSON states the schema in the prompt and extracts the first JSON object.
	f.runResults[testReason] = chatReply("Here: {\"ratings\": [{\"index\": 0}]} done")
	schema := map[string]any{"type": "object", "required": []any{"ratings"}}
	got, err := ChatJSON(ctx, at(c, testReason), msgs, schema)
	if err != nil || got.(map[string]any)["ratings"] == nil || f.lastRun["action"] != testReason {
		t.Fatalf("json = %v, %v, ran %v", got, err, f.lastRun["action"])
	}
	sent, _ := f.lastRun["args"].(map[string]any)["messages"].([]any)
	last, _ := sent[len(sent)-1].(map[string]any)["content"].(string)
	if !strings.Contains(last, `"required":["ratings"]`) {
		t.Fatalf("schema not stated in the prompt: %q", last)
	}
	f.runResults[testReason] = chatReply("no object at all")
	if _, err := ChatJSON(ctx, at(c, testReason), msgs, schema); Code(err) != "execution_failed" {
		t.Fatalf("reply without JSON: err = %v, want execution_failed", err)
	}
}

// A reply lacking the field the client reads is an error, never an empty answer
// or an empty search; an empty result list is a search that matched nothing.
func TestRepliesWithoutTheReadFieldAreErrors(t *testing.T) {
	f := newFakeKernel()
	c := startFake(t, f)
	ctx := context.Background()
	for _, res := range []map[string]any{{}, {"choices": []any{}}, {"choices": []any{map[string]any{"index": 0}}}} {
		f.runResults[testReason] = res
		if _, err := Chat(ctx, at(c, testReason), nil); Code(err) != "execution_failed" {
			t.Errorf("chat reply %v: err = %v, want execution_failed", res, err)
		}
	}
	f.runResults[testLookup] = map[string]any{}
	if _, err := Lookup(ctx, at(c, testLookup), "q", 8); Code(err) != "execution_failed" {
		t.Errorf("lookup reply without results: err = %v, want execution_failed", err)
	}
	f.runResults[testLookup] = map[string]any{"results": []any{}}
	if cands, err := Lookup(ctx, at(c, testLookup), "q", 8); err != nil || len(cands) != 0 {
		t.Errorf("empty results = %v, %v; want no candidates and no error", cands, err)
	}
}

func TestDecideRendersToolsAndExtractsSelection(t *testing.T) {
	f := newFakeKernel()
	// The model replies with prose wrapping a JSON selection, which the client
	// must extract.
	f.runResults[testReason] = chatReply(`Sure. {"action": "sys@k/web", "args": {"url": "x"}}`)
	c := startFake(t, f)
	catalog := []agent.Candidate{{Ref: "sys@k/web", Description: "fetch a page", Price: 3,
		Evidence:    map[string]any{"local_experience": map[string]any{"uses": 4, "successes": 3}},
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"}}}}
	tools := []agent.Tool{{Name: "reply", Description: "finish", InputSchema: map[string]any{"type": "object",
		"properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []any{"answer"}}}}

	sel, err := Decide(context.Background(), at(c, testReason), []agent.Message{{Role: agent.RoleUser, Content: "hi"}}, catalog, tools)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if sel.Action != "sys@k/web" || sel.Args["url"] != "x" {
		t.Fatalf("selection = %+v", sel)
	}
	msgs, _ := f.lastRun["args"].(map[string]any)["messages"].([]any)
	prompt, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	for _, want := range []string{"reply", "answer (required)", "sys@k/web", "price=3", `"uses":4`, "url (required)"} {
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

// nativeRow is an action read whose schemas are given as JSON.
func nativeRow(t *testing.T, ref, input, output string) map[string]any {
	t.Helper()
	return map[string]any{"action": ref, "description": "native " + ref, "price": 0, "quote_hash": "q-" + ref,
		"input_schema": schema(t, input), "output_schema": schema(t, output)}
}

func TestAffordancesAreCheckedAndReadWithTheirTerms(t *testing.T) {
	f := newFakeKernel()
	f.actions[testReason] = nativeRow(t, testReason, kernelChatInput, kernelChatOutput)
	f.actions[testLookup] = nativeRow(t, testLookup, kernelLookupInput, kernelLookupOutput)
	f.actions[testReason]["price"] = 4
	c := startFake(t, f)
	ctx := context.Background()

	// Both come back as the kernel holds them now: the terms a job pins its calls
	// to, and the action lookup's entry as the model is offered it.
	reason, lookup, err := c.Affordances(ctx)
	if err != nil || reason.Ref != testReason || reason.Price != 4 || reason.QuoteHash != "q-"+testReason ||
		lookup.Ref != testLookup || lookup.Description != "native "+testLookup ||
		lookup.QuoteHash != "q-"+testLookup || lookup.InputSchema["required"] == nil {
		t.Fatalf("affordances = %+v, %+v, %v", reason, lookup, err)
	}

	// A wrong-shaped action lookup is refused, naming the key, the address and the field.
	f.actions[testLookup] = nativeRow(t, testLookup, kernelLookupInput, kernelTimeOutput)
	if _, _, err := c.Affordances(ctx); err == nil ||
		err.Error() != "action_lookup = "+testLookup+": output.properties.results: missing (want array)" {
		t.Fatalf("err = %v", err)
	}
	// A reasoning action the kernel does not hold is refused, naming reason.
	delete(f.actions, testReason)
	if _, _, err := c.Affordances(ctx); err == nil || !strings.HasPrefix(err.Error(), "reason = "+testReason+": ") {
		t.Fatalf("err = %v", err)
	}
}

func TestLookupAndRead(t *testing.T) {
	f := newFakeKernel()
	f.runResults[testLookup] = map[string]any{"results": []any{
		map[string]any{"action": "sys@k/web", "description": "fetch", "price": 9},
	}}
	f.actions["sys@k/web"] = map[string]any{"action": "sys@k/web", "description": "fetch", "price": 7,
		"quote_hash": "q-web", "effect": "", "evidence": map[string]any{"local_experience": map[string]any{"uses": 1}}}
	f.actions["sys@k/transfer"] = map[string]any{"action": "sys@k/transfer", "effect": "transfer"}
	c := startFake(t, f)
	ctx := context.Background()

	cands, err := Lookup(ctx, at(c, testLookup), "fetch a page", 8)
	if err != nil || len(cands) != 1 || cands[0].Ref != "sys@k/web" || f.lastRun["action"] != testLookup {
		t.Fatalf("lookup = %+v, %v, ran %v", cands, err, f.lastRun["action"])
	}
	got, err := c.Read(ctx, "sys@k/web")
	if err != nil || got.Price != 7 || got.QuoteHash != "q-web" || got.Evidence["local_experience"] == nil {
		t.Fatalf("read = %+v, %v", got, err)
	}
	if tr, _ := c.Read(ctx, "sys@k/transfer"); tr.Effect != "transfer" {
		t.Fatalf("effect not read: %+v", tr)
	}
	if _, err := c.Read(ctx, "nobody@k/none"); Code(err) != "not_found" {
		t.Fatalf("unresolved ref: err = %v, want not_found", err)
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

func TestTasksListAndComplete(t *testing.T) {
	f := newFakeKernel()
	f.tasks = []map[string]any{{"id": "t1", "action": "sys@k/sink", "owner": "alice@k", "requester": "rooms@k",
		"created_by": "rooms@k/post", "required_caller": "agent@k",
		"partial_args": map[string]any{"message": "hi"}, "status": "waiting"}}
	c := startFake(t, f)
	ctx := context.Background()
	tasks, err := c.WaitingTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].Owner != "alice@k" || tasks[0].Requester != "rooms@k" ||
		tasks[0].CreatedBy != "rooms@k/post" || tasks[0].RequiredCaller != "agent@k" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	if err := c.CompleteTask(ctx, "t1", nil); err != nil {
		t.Fatal(err)
	}
	if body, _ := f.completed["t1"].(map[string]any); body == nil || body["args"] == nil {
		t.Fatalf("completion body = %v, want args {}", f.completed["t1"])
	}
}

// Tasks the agent funded for other callers fill the first pages; the one
// addressed to the agent comes last and must still be listed.
func TestWaitingTasksReadsEveryPage(t *testing.T) {
	f := newFakeKernel()
	for i := 0; i < 449; i++ {
		f.tasks = append(f.tasks, map[string]any{"id": "funded-" + strconv.Itoa(i), "required_caller": "bob@k", "status": "waiting"})
	}
	f.tasks = append(f.tasks, map[string]any{"id": "mine", "required_caller": "agent@k", "status": "waiting"})
	tasks, err := startFake(t, f).WaitingTasks(context.Background())
	if err != nil || len(tasks) != 450 || tasks[449].ID != "mine" {
		t.Fatalf("listed %d tasks, %v; want all 450 ending with mine", len(tasks), err)
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
		// A timeout parks the dispatched call; a new run would be a second call.
		{&KernelError{Code: "timeout", Status: 504}, false},
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

func TestMeReadsIdentity(t *testing.T) {
	self, err := startFake(t, newFakeKernel()).Me(context.Background())
	if err != nil || self.Address != "agent@k" || self.ID != "u1" || self.Available != 500 {
		t.Fatalf("me = %+v, %v", self, err)
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
