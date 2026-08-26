// Package juice is the daemon's sole conduit to a Juice kernel: a typed HTTP
// client over the public TCP API. It implements the agent core's Decider,
// Catalog, and Runner interfaces (every capability reached through Call() / the
// REST surface), plus the supervision calls the policy layer needs (stats,
// ratings, action lifecycle, steps). It is the only package that speaks HTTP.
package juice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// Client is an authenticated kernel client for one handle. It is safe for
// concurrent use; the token pair is guarded by a mutex and renewed on 401.
type Client struct {
	base     string
	handle   string
	password string
	hc       *http.Client

	mu      sync.Mutex
	token   string // access token (bearer)
	refresh string // refresh token, consumed by renew on 401
}

// New builds a client for base URL and credentials. It does not log in; call
// Login (or any method, which logs in lazily on first 401) first.
func New(base, handle, password string) *Client {
	return &Client{
		base:     strings.TrimRight(base, "/"),
		handle:   handle,
		password: password,
		hc:       &http.Client{Timeout: 3 * time.Minute},
	}
}

// tokenPair is the body of a successful token or refresh exchange.
type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Login authenticates with the handle+password over the kernel's PKCE flow:
// authorize with a code challenge, then exchange the returned code plus the
// verifier for an access/refresh token pair.
func (c *Client) Login(ctx context.Context) error {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	var auth struct {
		Redirect string `json:"redirect"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/auth/authorize",
		map[string]any{"handle": c.handle, "password": c.password, "code_challenge": challenge},
		&auth, false); err != nil {
		return err
	}
	code, err := codeFromRedirect(auth.Redirect)
	if err != nil {
		return err
	}
	var pair tokenPair
	if err := c.call(ctx, http.MethodPost, "/v1/auth/token",
		map[string]any{"code": code, "code_verifier": verifier}, &pair, false); err != nil {
		return err
	}
	return c.setTokens(pair)
}

// pkcePair returns a fresh S256 verifier/challenge pair.
func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// codeFromRedirect extracts the authorization code from the authorize reply's
// redirect value (a query string like "?code=CODE").
func codeFromRedirect(redirect string) (string, error) {
	q, err := url.ParseQuery(strings.TrimPrefix(redirect, "?"))
	if err != nil || q.Get("code") == "" {
		return "", &KernelError{Status: 200, Code: "invalid_state",
			Message: "authorize returned no code: " + redirect}
	}
	return q.Get("code"), nil
}

// renew rotates the token pair via the refresh endpoint. It fails when no
// refresh token is held or the kernel rejects it (revoked, expired).
func (c *Client) renew(ctx context.Context) error {
	c.mu.Lock()
	refresh := c.refresh
	c.mu.Unlock()
	if refresh == "" {
		return &KernelError{Status: 401, Code: "unauthenticated", Message: "no refresh token"}
	}
	var pair tokenPair
	if err := c.call(ctx, http.MethodPost, "/v1/auth/refresh",
		map[string]any{"refresh_token": refresh}, &pair, false); err != nil {
		return err
	}
	return c.setTokens(pair)
}

func (c *Client) setTokens(pair tokenPair) error {
	if pair.AccessToken == "" {
		return &KernelError{Status: 200, Code: "invalid_state", Message: "auth returned empty access token"}
	}
	c.mu.Lock()
	c.token, c.refresh = pair.AccessToken, pair.RefreshToken
	c.mu.Unlock()
	return nil
}

// do issues an authenticated request, transparently renewing the token pair —
// or re-logging-in from the in-memory password when renewal fails — and
// retrying once on a 401 (an expired or missing token).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	err := c.call(ctx, method, path, body, out, true)
	var ke *KernelError
	if err != nil && asKernel(err, &ke) && ke.Status == http.StatusUnauthorized {
		if rerr := c.renew(ctx); rerr != nil {
			if lerr := c.Login(ctx); lerr != nil {
				return lerr
			}
		}
		return c.call(ctx, method, path, body, out, true)
	}
	return err
}

// call performs one HTTP round-trip. When auth is true it attaches the current
// bearer token. Non-2xx responses decode into a KernelError.
func (c *Client) call(ctx context.Context, method, path string, body, out any, auth bool) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		c.mu.Lock()
		tok := c.token
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeKernelError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

func decodeKernelError(status int, raw []byte) *KernelError {
	var e struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	msg := e.Error
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return &KernelError{Status: status, Code: e.Code, Message: msg}
}

func asKernel(err error, dst **KernelError) bool {
	ke, ok := err.(*KernelError)
	if ok {
		*dst = ke
	}
	return ok
}

// --- Runner ----------------------------------------------------------------

type runReply struct {
	Result    map[string]any `json:"result"`
	TxID      string         `json:"tx_id"`
	TraceID   string         `json:"trace_id"`
	ProcessID string         `json:"process_id"`
}

// Run dispatches a tool call: POST /v1/run. A kernel-level call failure surfaces
// as a KernelError (the loop records it as an error tool result).
func (c *Client) Run(ctx context.Context, action string, args map[string]any) (agent.RunResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var rep runReply
	if err := c.do(ctx, http.MethodPost, "/v1/run",
		map[string]any{"action": action, "args": args}, &rep); err != nil {
		return agent.RunResult{}, err
	}
	return agent.RunResult{Result: rep.Result, TxID: rep.TxID}, nil
}

// --- Decider ---------------------------------------------------------------

// Decide performs one selection round-trip. It renders the harness tools and
// catalog candidates into a chat prompt, runs sys/llm/chat, and extracts a JSON
// selection from the reply. We use plain chat, not sys/llm/decide: decide only
// accepts owner/name refs (so plain harness-tool names can't be offered) and,
// on Ollama, its grammar-constrained JSON corrupts copied strings (measured:
// timestamps garbled). Unconstrained chat + balanced-brace extraction is both
// more capable (offers tools) and higher-fidelity.
func (c *Client) Decide(ctx context.Context, messages []agent.Message, catalog []agent.Candidate, tools []agent.Tool) (*agent.Selection, error) {
	aug := append(append([]agent.Message{}, messages...),
		agent.Message{Role: agent.RoleUser, Content: selectionPrompt(catalog, tools)})
	content, err := c.Chat(ctx, aug)
	if err != nil {
		return nil, err
	}
	js := extractJSON(content)
	if js == "" {
		return nil, &KernelError{Status: 200, Code: "execution_failed",
			Message: "no JSON selection in model reply: " + trunc(content, 120)}
	}
	var sel struct {
		Action  string         `json:"action"`
		Args    map[string]any `json:"args"`
		Message string         `json:"message"`
	}
	if err := json.Unmarshal([]byte(js), &sel); err != nil || sel.Action == "" {
		return nil, &KernelError{Status: 200, Code: "execution_failed",
			Message: "unparseable selection: " + trunc(js, 120)}
	}
	if sel.Args == nil {
		sel.Args = map[string]any{}
	}
	return &agent.Selection{Action: sel.Action, Args: sel.Args, Message: sel.Message}, nil
}

// selectionPrompt renders the one-shot instruction + tool list the model chooses
// from: harness tools (plain names) first, then catalog actions (owner/name)
// with price, stats, and a compact schema.
func selectionPrompt(catalog []agent.Candidate, tools []agent.Tool) string {
	var b strings.Builder
	b.WriteString("Select exactly ONE tool or action to take next.\n\n")
	if len(tools) > 0 {
		b.WriteString("Tools (plain names):\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "- %s — %s; args: %s\n", t.Name, t.Description, argSummary(t.InputSchema))
		}
		b.WriteByte('\n')
	}
	if len(catalog) > 0 {
		b.WriteString("Actions (owner/name), preferring proven ones:\n")
		for _, c := range catalog {
			fmt.Fprintf(&b, "- %s — %s; price=%d%s; args: %s\n",
				c.Ref, c.Description, c.Price, statsSummary(c.Stats), argSummary(c.InputSchema))
		}
		b.WriteByte('\n')
	}
	b.WriteString(`Respond with ONLY a JSON object: {"action": "<name or owner/name>", "args": {...}}. No prose, no code fences.`)
	return b.String()
}

// argSummary renders a schema's properties as "name (required), other" — a
// compact hint, not the full schema.
func argSummary(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return "{}"
	}
	req := map[string]bool{}
	if rs, ok := schema["required"].([]any); ok {
		for _, r := range rs {
			if s, ok := r.(string); ok {
				req[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		if req[k] {
			parts = append(parts, k+" (required)")
		} else {
			parts = append(parts, k)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// statsSummary renders an action's track record inline, or "" when untested.
func statsSummary(s agent.ActionStats) string {
	if !s.Known || s.Uses == 0 {
		return " (untested)"
	}
	out := fmt.Sprintf(" uses=%d success=%d%%", s.Uses, pct(s.Successes, s.Uses))
	if s.RatingCount > 0 {
		out += fmt.Sprintf(" rating=%.2f", s.RatingEstimate)
	}
	return out
}

func pct(n, d int) int {
	if d == 0 {
		return 0
	}
	return n * 100 / d
}

// extractJSON returns the first balanced {...} object in s (skipping prose and
// code fences), ported from build.py's extract_json.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- LLM helpers (compaction, rating, embedding) ---------------------------

// Chat runs sys/llm/chat over role/content messages and returns the reply text.
func (c *Client) Chat(ctx context.Context, messages []agent.Message) (string, error) {
	res, err := c.Run(ctx, "sys/llm/chat", map[string]any{"messages": toChatMessages(messages)})
	if err != nil {
		return "", err
	}
	if mo, ok := res.Result["message"].(map[string]any); ok {
		s, _ := mo["content"].(string)
		return s, nil
	}
	return "", nil
}

// ChatJSON runs sys/llm/json with an output schema and returns the validated value.
func (c *Client) ChatJSON(ctx context.Context, messages []agent.Message, schema map[string]any) (any, error) {
	res, err := c.Run(ctx, "sys/llm/json", map[string]any{
		"messages": toChatMessages(messages), "output_schema": schema,
	})
	if err != nil {
		return nil, err
	}
	return res.Result["value"], nil
}

// Embed runs sys/llm/embed and returns the embedding vector.
func (c *Client) Embed(ctx context.Context, text string) ([]float64, error) {
	res, err := c.Run(ctx, "sys/llm/embed", map[string]any{"text": text})
	if err != nil {
		return nil, err
	}
	raw, _ := res.Result["embedding"].([]any)
	out := make([]float64, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(float64); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// toChatMessages flattens transcript messages into {role, content} for the chat
// natives, rendering tool selections and results as readable content lines.
func toChatMessages(msgs []agent.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		switch {
		case m.Role == agent.RoleAssistant && m.Action != "":
			content = "called " + m.Action
			if m.Content != "" {
				content += ": " + m.Content
			}
		case m.Role == agent.RoleTool && m.Action != "":
			if b, err := json.Marshal(m.Result); err == nil && m.Result != nil {
				content = "result of " + m.Action + ": " + string(b)
			} else if m.Content != "" {
				content = "error from " + m.Action + ": " + m.Content
			}
		}
		role := string(m.Role)
		if role == string(agent.RoleTool) {
			role = "user" // chat natives take system/user/assistant
		}
		out = append(out, map[string]any{"role": role, "content": content})
	}
	return out
}

// --- Catalog ---------------------------------------------------------------

// Lookup ranks active actions for a query via sys/lookup.
func (c *Client) Lookup(ctx context.Context, query string, limit int) ([]agent.Candidate, error) {
	if limit <= 0 {
		limit = 10
	}
	res, err := c.Run(ctx, "sys/lookup", map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	raw, _ := res.Result["results"].([]any)
	out := make([]agent.Candidate, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := m["action"].(string)
		if ref == "" {
			continue
		}
		desc, _ := m["description"].(string)
		schema, _ := m["input_schema"].(map[string]any)
		out = append(out, agent.Candidate{Ref: ref, Description: desc, InputSchema: schema})
	}
	return out, nil
}

type actionJSON struct {
	ID        string `json:"id"`
	ActionRef string `json:"action"`
	Price     int    `json:"price"`
	Active    bool   `json:"active"`
	Name      string `json:"name"`
}

type statsJSON struct {
	Uses            int     `json:"uses"`
	Successes       int     `json:"successes"`
	Failures        int     `json:"failures"`
	RatingCount     int     `json:"rating_count"`
	LatencyEstimate float64 `json:"latency_estimate"`
	RatingEstimate  float64 `json:"rating_estimate"`
}

// Meta fetches an action's price and stats by owner/name. Price comes from the
// filtered action list; stats from GET /v1/stats/{id}. A missing stats row
// yields zeroed, not-Known stats (an untested action).
func (c *Client) Meta(ctx context.Context, ref string) (int, agent.ActionStats, error) {
	a, err := c.resolve(ctx, ref)
	if err != nil {
		return 0, agent.ActionStats{}, err
	}
	var s statsJSON
	if err := c.do(ctx, http.MethodGet, "/v1/stats/"+url.PathEscape(a.ID), nil, &s); err != nil {
		// An action with no stats row is untested, not an error.
		if Code(err) == "not_found" {
			return a.Price, agent.ActionStats{}, nil
		}
		return a.Price, agent.ActionStats{}, err
	}
	stats := agent.ActionStats{
		Uses: s.Uses, Successes: s.Successes, Failures: s.Failures,
		RatingCount: s.RatingCount, LatencyEstimate: s.LatencyEstimate,
		RatingEstimate: s.RatingEstimate, Known: true,
	}
	return a.Price, stats, nil
}

// resolve finds the action row for a owner/name ref via the filtered list
// (which the {id} path does not accept — it resolves UUIDs only).
func (c *Client) resolve(ctx context.Context, ref string) (actionJSON, error) {
	owner, name, err := splitRef(ref)
	if err != nil {
		return actionJSON{}, err
	}
	q := url.Values{"owner": {owner}, "name": {name}, "all": {"1"}}
	var acts []actionJSON
	if err := c.do(ctx, http.MethodGet, "/v1/actions?"+q.Encode(), nil, &acts); err != nil {
		return actionJSON{}, err
	}
	for _, a := range acts {
		if a.ActionRef == ref || a.Name == name {
			return a, nil
		}
	}
	return actionJSON{}, &KernelError{Status: 404, Code: "not_found", Message: "action not found: " + ref}
}

// splitRef splits a bare owner/name ref at the first slash (names may contain
// further slashes). A sigil-prefixed ref is malformed: the kernel rejects
// sigils, never strips them.
func splitRef(ref string) (owner, name string, err error) {
	i := strings.Index(ref, "/")
	if strings.HasPrefix(ref, "@") || i <= 0 || i == len(ref)-1 {
		return "", "", &KernelError{Status: 400, Code: "invalid_input", Message: "malformed action ref: " + ref}
	}
	return ref[:i], ref[i+1:], nil
}

// --- Supervision: ratings, action lifecycle, steps, self -------------------

// Rate posts a 0/1 rating with an optional note for a transaction the client
// owns (POST /v1/transactions/{id}/rate).
func (c *Client) Rate(ctx context.Context, txID string, rating int, note string) error {
	return c.do(ctx, http.MethodPost, "/v1/transactions/"+url.PathEscape(txID)+"/rate",
		map[string]any{"rating": rating, "note": note}, nil)
}

// CreateActionReq describes a WASM action registration from a prebuilt artifact.
type CreateActionReq struct {
	Name         string
	Description  string
	Price        int
	InputSchema  map[string]any
	OutputSchema map[string]any
	ArtifactB64  string
}

// CreateAction registers a kind=wasm action from a base64 artifact and returns
// its id and owner/name ref.
func (c *Client) CreateAction(ctx context.Context, req CreateActionReq) (id, ref string, err error) {
	body := map[string]any{
		"name": req.Name, "kind": "wasm", "price": req.Price,
		"description": req.Description, "wasm_artifact": req.ArtifactB64,
	}
	if req.InputSchema != nil {
		body["input_schema"] = req.InputSchema
	}
	if req.OutputSchema != nil {
		body["output_schema"] = req.OutputSchema
	}
	var a actionJSON
	if err := c.do(ctx, http.MethodPost, "/v1/actions", body, &a); err != nil {
		return "", "", err
	}
	return a.ID, a.ActionRef, nil
}

// CreateWasmAction is the Publisher-shaped wrapper over CreateAction, used by
// the maker's agent-side publish intrinsic.
func (c *Client) CreateWasmAction(ctx context.Context, name, description string, price int, in, out map[string]any, artifactB64 string) (id, ref string, err error) {
	return c.CreateAction(ctx, CreateActionReq{
		Name: name, Description: description, Price: price,
		InputSchema: in, OutputSchema: out, ArtifactB64: artifactB64,
	})
}

// EnableAction activates an action by id.
func (c *Client) EnableAction(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/actions/"+url.PathEscape(id)+"/enable", nil, nil)
}

// SetPublic makes an action public by id.
func (c *Client) SetPublic(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPut, "/v1/actions/"+url.PathEscape(id),
		map[string]any{"visibility": "public"}, nil)
}

// DeleteAction removes an action by id (history preserved kernel-side).
func (c *Client) DeleteAction(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/actions/"+url.PathEscape(id), nil, nil)
}

// ResolveID returns the action id for a owner/name ref, or "" if absent.
func (c *Client) ResolveID(ctx context.Context, ref string) (string, error) {
	a, err := c.resolve(ctx, ref)
	if err != nil {
		if Code(err) == "not_found" {
			return "", nil
		}
		return "", err
	}
	return a.ID, nil
}

// Self is the authenticated identity (GET /v1/me).
type Self struct {
	ID         string      `json:"id"`
	Handle     string      `json:"handle"`
	Available  int         `json:"available"`
	Locked     int         `json:"locked"`
	Connectors []Connector `json:"connectors"`
}

// Connector is a directory-grouped grant node from GET /v1/me: the folder the
// granted actions share (e.g. "chat/msg") and the actions granted beneath it. It
// lets a caller check whether a grant selector is already connected without
// exposing any token.
type Connector struct {
	Directory string `json:"directory"`
	Actions   []struct {
		Action string `json:"action"`
	} `json:"actions"`
}

// Me returns the client's own identity, wallet, and grant connectors.
func (c *Client) Me(ctx context.Context) (Self, error) {
	var s Self
	err := c.do(ctx, http.MethodGet, "/v1/me", nil, &s)
	return s, err
}

// ConnectGrant attaches a static token as the grant for selector (a
// delegated_bearer group): POST /v1/grants {selector, token} (kernel API §8). It
// is idempotent — reconnecting overwrites the stored token.
func (c *Client) ConnectGrant(ctx context.Context, selector, token string) error {
	return c.do(ctx, http.MethodPost, "/v1/grants",
		map[string]any{"selector": selector, "token": token}, nil)
}

// Step is a waiting step addressed to the client (a task-intake record).
type Step struct {
	ID                   string         `json:"id"`
	ActionRef            string         `json:"action"`
	CreatedBy            string         `json:"created_by"`
	OwnerHandle          string         `json:"owner_handle"`
	RequiredCallerHandle string         `json:"required_caller_handle"`
	PartialArgs          map[string]any `json:"partial_args"`
	AllowedInput         map[string]any `json:"allowed_input"`
	Status               string         `json:"status"`
}

// WaitingSteps lists waiting steps visible to the client (those it owns or is
// the required caller of).
func (c *Client) WaitingSteps(ctx context.Context) ([]Step, error) {
	var steps []Step
	err := c.do(ctx, http.MethodGet, "/v1/steps?status=waiting", nil, &steps)
	return steps, err
}

// CompleteStep completes a waiting step with the given input.
func (c *Client) CompleteStep(ctx context.Context, id string, args map[string]any) (agent.RunResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var rep runReply
	err := c.do(ctx, http.MethodPost, "/v1/steps/"+url.PathEscape(id)+"/complete",
		map[string]any{"args": args}, &rep)
	return agent.RunResult{Result: rep.Result, TxID: rep.TxID}, err
}

// KernelError is a typed kernel failure decoded from an error HTTP response
// ({"error": msg, "code": "ErrXxx"}). It preserves the kernel's stable error
// code so callers can branch on it without parsing prose.
type KernelError struct {
	Status  int
	Code    string
	Message string
}

func (e *KernelError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("http %d: %s", e.Status, e.Message)
}

// Code returns the kernel error code of err, or "" if err is not a KernelError.
func Code(err error) string {
	var ke *KernelError
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}

// IsGrantRequired reports whether err is the kernel's lazy-consent rejection
// (§8): running an action the caller has not yet granted (code "grant_required",
// HTTP 403). Callers provision the grant and retry. The status fallback is
// defensive: a grant_required always carries the code, but any 403 is treated as a
// missing grant so provisioning is attempted rather than the call failing outright.
func IsGrantRequired(err error) bool {
	if Code(err) == "grant_required" {
		return true
	}
	var ke *KernelError
	return errors.As(err, &ke) && ke.Status == http.StatusForbidden
}

// IsRetryable reports whether err is worth retrying with backoff: transport
// failures and transient kernel conditions (429, genuine 5xx, timeout), but
// never a cancellation, a schema/authz/funds rejection, or a model-output error.
//
// The kernel returns HTTP 500 for execution_failed / schema_violation — a model
// that produced an unusable selection or output. Those are not transient: a
// backoff-retry re-rolls the same prompt for the same likely failure. They are
// excluded here and handled by the loop's feed-back-and-continue instead, which
// gives the model its mistake as context on the next turn.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ke *KernelError
	if errors.As(err, &ke) {
		if ke.Code == "execution_failed" || ke.Code == "schema_violation" {
			return false
		}
		return ke.Status == http.StatusTooManyRequests || ke.Status >= 500 || ke.Code == "timeout"
	}
	// A non-kernel error here is a transport failure (connection refused, reset,
	// DNS) — retryable.
	return true
}
