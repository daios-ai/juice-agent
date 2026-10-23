// Package juice is the daemon's sole conduit to a Juice kernel: a typed HTTP
// client over the public TCP API. The client implements the agent core's Runner
// and the authoritative read, plus the supervision calls the policy layer needs
// (ratings, tasks). It is the only package that speaks HTTP. The two actions the
// harness depends on, reasoning and action lookup, are configured addresses whose
// actions must meet a contract (contract.go); the client names no other action.
// What the harness sends them and reads back is built here too (Decide, Chat,
// ChatJSON, Lookup), over a Call the caller supplies, so whoever runs a call
// decides its terms, its key and what it costs the job.
//
// The client holds no secret of its own. It acts as one login, handle@kernel,
// read from the juice-family client records under $JUICE_HOME/client/ (ecosystem
// standard §5): the kernel's endpoint from config.json, the session from
// credentials/<login>.json, which it rotates under that file's lock.
package juice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// ErrNoSession reports that the login holds no usable session: the operator must
// run `juice auth login <login>`.
var ErrNoSession = errors.New("no session")

// Client is an authenticated kernel client for one login. It is safe for
// concurrent use; the access token is guarded by a mutex and rotated on 401.
type Client struct {
	login        string // handle@kernel
	handle       string
	kernel       string // the name the kernel calls itself, the login's kernel half
	principal    string // the account's principal id, as recorded by `juice auth login`
	reason       string // the reasoning affordance's address (reasonContract)
	actionLookup string // the action-lookup affordance's address (actionLookupContract)
	base         string
	credPath     string
	hc           *http.Client

	mu    sync.Mutex
	token string
}

// ParseLogin splits handle@kernel. Both halves are local names: letters, digits,
// dot, dash and underscore, at most 64 characters, not beginning with a dot —
// the rule the juice CLI applies, which keeps a login one file name.
func ParseLogin(s string) (handle, kernel string, err error) {
	handle, kernel, found := strings.Cut(s, "@")
	if !found || !localName(handle) || !localName(kernel) {
		return "", "", fmt.Errorf("login %q: name the account and its kernel as handle@kernel, as in agent@work", s)
	}
	return handle, kernel, nil
}

func localName(s string) bool {
	if s == "" || len(s) > 64 || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// New builds a client for login from the client records under home
// ($JUICE_HOME), reasoning through reason and finding actions through
// actionLookup. It
// fails when the kernel is not recorded or the login holds no session; it sends
// nothing.
func New(login, home, reason, actionLookup string) (*Client, error) {
	handle, kern, err := ParseLogin(login)
	if err != nil {
		return nil, err
	}
	var records struct {
		Kernels map[string]struct {
			Endpoint string `json:"endpoint"`
		} `json:"kernels"`
	}
	cfgPath := filepath.Join(home, "client", "config.json")
	data, _ := os.ReadFile(cfgPath)
	_ = json.Unmarshal(data, &records)
	rec, ok := records.Kernels[kern]
	if !ok || rec.Endpoint == "" {
		return nil, fmt.Errorf("kernel %q is not recorded in %s: run juice kernel add <url>", kern, cfgPath)
	}
	c := &Client{
		login: login, handle: handle, kernel: kern, reason: reason, actionLookup: actionLookup,
		base:     strings.TrimRight(rec.Endpoint, "/"),
		credPath: filepath.Join(home, "client", "credentials", login+".json"),
		hc:       &http.Client{Timeout: 3 * time.Minute},
	}
	cred := readCredentials(c.credPath)
	c.principal, _ = cred["principal_id"].(string)
	c.token, _ = cred["token"].(string)
	if refresh, _ := cred["refresh_token"].(string); c.token == "" && refresh == "" {
		return nil, c.noSession()
	}
	return c, nil
}

func (c *Client) noSession() error {
	return fmt.Errorf("%w for %s: run juice auth login %s", ErrNoSession, c.login, c.login)
}

// Login is the client's login, handle@kernel.
func (c *Client) Login() string { return c.login }

// Principal is the account's recorded principal id (ecosystem standard §5).
func (c *Client) Principal() string { return c.principal }

// readCredentials reads a credential file as a map, so a rewrite keeps every
// field the juice CLI stored, not only the ones this client uses.
func readCredentials(path string) map[string]any {
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// refresh rotates the session after the kernel refused token used. The whole
// read-rotate-write runs under an exclusive lock on the credential file, as the
// juice CLI does, so a process sharing the login that rotated first is followed
// rather than raced: its stored token is adopted and no refresh token is spent
// twice.
func (c *Client) refresh(ctx context.Context, used string) error {
	f, err := os.OpenFile(c.credPath, os.O_RDWR, 0o600)
	if err != nil {
		return c.noSession()
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	cred := map[string]any{}
	if data, err := io.ReadAll(f); err == nil {
		_ = json.Unmarshal(data, &cred)
	}
	// The file name is a label: a later login under it may be another account,
	// whose session this daemon must not adopt (its home and memory are this one's).
	if p, _ := cred["principal_id"].(string); p != c.principal {
		return fmt.Errorf("%w for %s: the stored session now belongs to principal %s, not %s: restart juice-agent to serve it",
			ErrNoSession, c.login, p, c.principal)
	}
	if tok, _ := cred["token"].(string); tok != "" && tok != used {
		c.setToken(tok)
		return nil
	}
	refresh, _ := cred["refresh_token"].(string)
	if refresh == "" {
		return c.noSession()
	}
	var pair struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": refresh}, &pair, ""); err != nil {
		if ke, ok := err.(*KernelError); ok && ke.Status < 500 {
			return c.noSession() // the kernel refused the refresh token
		}
		return err // transport or server failure: retryable, the session may still be good
	}
	if pair.AccessToken == "" {
		return c.noSession()
	}
	cred["token"] = pair.AccessToken
	if pair.RefreshToken != "" {
		cred["refresh_token"] = pair.RefreshToken
	}
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(append(data, '\n'), 0); err != nil {
		return err
	}
	c.setToken(pair.AccessToken)
	return nil
}

func (c *Client) setToken(tok string) {
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
}

// do issues an authenticated request and, on a 401, rotates the session once and
// retries.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tok := c.currentToken()
	err := c.call(ctx, method, path, body, out, tok)
	var ke *KernelError
	if err == nil || !errors.As(err, &ke) || ke.Status != http.StatusUnauthorized {
		return err
	}
	if err := c.refresh(ctx, tok); err != nil {
		return err
	}
	return c.call(ctx, method, path, body, out, c.currentToken())
}

// call performs one HTTP round-trip, attaching tok as the bearer token when
// non-empty. Non-2xx responses decode into a KernelError.
func (c *Client) call(ctx context.Context, method, path string, body, out any, tok string) error {
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
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
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
		Error   string         `json:"error"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Meta    map[string]any `json:"meta"`
	}
	_ = json.Unmarshal(raw, &e)
	msg := e.Error
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return &KernelError{Status: status, Code: e.Code, Message: msg, Meta: e.Meta}
}

// --- Runner ----------------------------------------------------------------

type runReply struct {
	Result map[string]any `json:"result"`
	TxID   string         `json:"tx_id"`
	Charge int            `json:"charge"`
}

// Run dispatches a tool call: POST /v1/run. quoteHash, when non-empty, pins the
// terms the caller read; the kernel refuses the call with terms_changed before
// locking any funds if they have moved. key, when non-empty, is the run's
// external_key: a repeat answers with the first run's outcome and moves nothing.
// A kernel-level call failure surfaces as a KernelError (the loop records it as an
// error tool result), returned with the settlement its meta reports.
func (c *Client) Run(ctx context.Context, action string, args map[string]any, quoteHash, key string) (agent.RunResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	body := map[string]any{"action": action, "args": args}
	if quoteHash != "" {
		body["quote_hash"] = quoteHash
	}
	if key != "" {
		body["external_key"] = key
	}
	var rep runReply
	if err := c.do(ctx, http.MethodPost, "/v1/run", body, &rep); err != nil {
		var ke *KernelError
		if errors.As(err, &ke) {
			tx, _ := ke.Meta["tx_id"].(string)
			charge, _ := ke.Meta["charge"].(float64)
			return agent.RunResult{TxID: tx, Charge: int(charge)}, err
		}
		return agent.RunResult{}, err
	}
	return agent.RunResult{Result: rep.Result, TxID: rep.TxID, Charge: rep.Charge}, nil
}

// --- Affordance calls ------------------------------------------------------

// Call runs one call of an affordance with args and returns its result. The
// executor supplies it: it checks the call against the job's budget, pins it to
// the terms the job read, and counts what it cost.
type Call func(ctx context.Context, args map[string]any) (map[string]any, error)

// Decide performs one selection round-trip. It renders the harness tools and
// catalog candidates into a chat prompt, runs llm/chat, and extracts a JSON
// selection from the reply. We use plain chat, not llm/decide: decide only
// accepts action references (so plain harness-tool names can't be offered) and,
// on Ollama, its grammar-constrained JSON corrupts copied strings (measured:
// timestamps garbled). Unconstrained chat + balanced-brace extraction is both
// more capable (offers tools) and higher-fidelity.
func Decide(ctx context.Context, call Call, messages []agent.Message, catalog []agent.Candidate, tools []agent.Tool) (*agent.Selection, error) {
	aug := append(append([]agent.Message{}, messages...),
		agent.Message{Role: agent.RoleUser, Content: selectionPrompt(catalog, tools)})
	content, err := Chat(ctx, call, aug)
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
// from: harness tools (plain names) first, then catalog actions (addresses) with
// price, the kernel's own record of them, and a compact schema.
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
		b.WriteString("Actions (owner@kernel/name), with this kernel's record of each:\n")
		for _, c := range catalog {
			fmt.Fprintf(&b, "- %s — %s; price=%d; record: %s; args: %s\n",
				c.Ref, c.Description, c.Price, recordSummary(c.Evidence), argSummary(c.InputSchema))
		}
		b.WriteByte('\n')
	}
	b.WriteString(`Respond with ONLY a JSON object: {"action": "<name or owner@kernel/name>", "args": {...}}. No prose, no code fences.`)
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

// recordSummary renders what this kernel itself recorded about an action (the
// evidence block's local_experience) as compact JSON. The block's provenances
// are never summed or scored (kernel U39), so only this kernel's own row is shown.
func recordSummary(evidence map[string]any) string {
	if le, ok := evidence["local_experience"].(map[string]any); ok && len(le) > 0 {
		if b, err := json.Marshal(le); err == nil {
			return string(b)
		}
	}
	return "none"
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

// --- LLM natives (kernel D17) ---------------------------------------------

// Chat runs the reasoning affordance over role/content messages and returns the
// reply text.
func Chat(ctx context.Context, call Call, messages []agent.Message) (string, error) {
	res, err := call(ctx, map[string]any{"messages": toChatMessages(messages)})
	if err != nil {
		return "", err
	}
	// The contract cannot make these fields present (the kernel's own schema has
	// them optional), so a reply without them is reported here, not read as empty.
	var first map[string]any
	if choices, _ := res["choices"].([]any); len(choices) > 0 {
		first, _ = choices[0].(map[string]any)
	}
	msg, _ := first["message"].(map[string]any)
	content, ok := msg["content"].(string)
	if !ok {
		return "", &KernelError{Status: 200, Code: "execution_failed",
			Message: "the reasoning reply has no choices[0].message.content"}
	}
	return content, nil
}

// ChatJSON asks the reasoning affordance for a JSON value shaped by schema and
// returns the first JSON object in its reply. The schema is stated in the prompt,
// so structured output needs no affordance beyond reasoning.
func ChatJSON(ctx context.Context, call Call, messages []agent.Message, schema map[string]any) (any, error) {
	sch, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	aug := append(append([]agent.Message{}, messages...), agent.Message{Role: agent.RoleUser,
		Content: "Respond with ONLY a JSON object matching this JSON Schema, no prose, no code fences:\n" + string(sch)})
	content, err := Chat(ctx, call, aug)
	if err != nil {
		return nil, err
	}
	var v any
	if js := extractJSON(content); js == "" || json.Unmarshal([]byte(js), &v) != nil {
		return nil, &KernelError{Status: 200, Code: "execution_failed", Message: "no JSON object in model reply: " + trunc(content, 120)}
	}
	return v, nil
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

// Lookup ranks callable actions for a query through the action-lookup
// affordance. The rows are indicative; Read gives the authoritative terms.
func Lookup(ctx context.Context, call Call, query string, limit int) ([]agent.Candidate, error) {
	if limit <= 0 {
		limit = 10
	}
	res, err := call(ctx, map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	if _, ok := res["results"].([]any); !ok {
		return nil, &KernelError{Status: 200, Code: "execution_failed",
			Message: "the action lookup reply has no results list"}
	}
	return agent.ParseLookup(res), nil
}

// --- Catalog ---------------------------------------------------------------

// Read resolves an action reference to its authoritative terms: price, the
// quote_hash that pins them, any value effect, and the kernel's evidence about
// it (GET /v1/actions?ref=).
func (c *Client) Read(ctx context.Context, ref string) (agent.Candidate, error) {
	a, err := c.readAction(ctx, ref)
	return a.candidate(), err
}

// actionRow is an action read (GET /v1/actions?ref=), as far as the client uses it.
type actionRow struct {
	ActionRef    string         `json:"action"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Price        int            `json:"price"`
	QuoteHash    string         `json:"quote_hash"`
	Effect       string         `json:"effect"`
	Evidence     map[string]any `json:"evidence"`
}

func (a actionRow) candidate() agent.Candidate {
	return agent.Candidate{Ref: a.ActionRef, Description: a.Description, InputSchema: a.InputSchema,
		Price: a.Price, QuoteHash: a.QuoteHash, Effect: a.Effect, Evidence: a.Evidence}
}

func (c *Client) readAction(ctx context.Context, ref string) (actionRow, error) {
	var rows []actionRow
	if err := c.do(ctx, http.MethodGet, "/v1/actions?ref="+url.QueryEscape(ref), nil, &rows); err != nil {
		return actionRow{}, err
	}
	if len(rows) == 0 {
		return actionRow{}, &KernelError{Status: 404, Code: "not_found", Message: "action not found: " + ref}
	}
	return rows[0], nil
}

// Affordances reads the two configured actions and checks each one's declared
// schemas against its contract. It returns both with the terms the kernel holds
// for them now: their price and the quote_hash that pins it. Startup calls it to
// refuse a wrong configuration, and every job calls it again, so a job's calls
// run at current terms. An error names the configuration key and the address.
func (c *Client) Affordances(ctx context.Context) (reason, actionLookup agent.Candidate, err error) {
	r, err := c.affordance(ctx, "reason", c.reason, reasonContract)
	if err != nil {
		return agent.Candidate{}, agent.Candidate{}, err
	}
	l, err := c.affordance(ctx, "action_lookup", c.actionLookup, actionLookupContract)
	if err != nil {
		return agent.Candidate{}, agent.Candidate{}, err
	}
	return r.candidate(), l.candidate(), nil
}

// affordance reads the action configured under key and checks it against k.
func (c *Client) affordance(ctx context.Context, key, ref string, k contract) (actionRow, error) {
	row, err := c.readAction(ctx, ref)
	if err == nil {
		err = k.check(row.InputSchema, row.OutputSchema)
	}
	if err != nil {
		return actionRow{}, fmt.Errorf("%s = %s: %w", key, ref, err)
	}
	return row, nil
}

// --- Supervision: ratings, tasks, self ---------------------------------------

// Rate posts a 0/1 rating with an optional note for a transaction the client
// paid for (POST /v1/transactions/{id}/rate).
func (c *Client) Rate(ctx context.Context, txID string, rating int, note string) error {
	return c.do(ctx, http.MethodPost, "/v1/transactions/"+url.PathEscape(txID)+"/rate",
		map[string]any{"rating": rating, "note": note}, nil)
}

// Self is the authenticated identity (GET /v1/me).
type Self struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	Available int    `json:"available"`
	Locked    int    `json:"locked"`
}

// Me returns the client's own identity and wallet.
func (c *Client) Me(ctx context.Context) (Self, error) {
	var s Self
	err := c.do(ctx, http.MethodGet, "/v1/me", nil, &s)
	return s, err
}

// Task is a kernel task (D6) as GET /v1/tasks renders it: every party an address.
type Task struct {
	ID             string         `json:"id"`
	ActionRef      string         `json:"action"`
	CreatedBy      string         `json:"created_by"`
	Requester      string         `json:"requester"`
	Owner          string         `json:"owner"`
	RequiredCaller string         `json:"required_caller"`
	PartialArgs    map[string]any `json:"partial_args"`
	AllowedInput   map[string]any `json:"allowed_input"`
	Status         string         `json:"status"`
}

// taskPage is the most tasks one list request returns (the API's ceiling).
const taskPage = 200

// WaitingTasks lists the waiting tasks visible to the client: those it funded
// and those addressed to it by a requester it approved (kernel U52). It reads
// every page: the listing cannot be filtered by required caller, so a first
// page full of tasks the client funded would otherwise hide every later one.
// A task missed while the list changes between pages appears at the next call.
func (c *Client) WaitingTasks(ctx context.Context) ([]Task, error) {
	var tasks []Task
	for offset := 0; ; offset += taskPage {
		var page []Task
		path := fmt.Sprintf("/v1/tasks?status=waiting&limit=%d&offset=%d", taskPage, offset)
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		tasks = append(tasks, page...)
		if len(page) < taskPage {
			return tasks, nil
		}
	}
}

// CompleteTask completes a waiting task with the given input.
func (c *Client) CompleteTask(ctx context.Context, id string, args map[string]any) error {
	if args == nil {
		args = map[string]any{}
	}
	return c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(id)+"/complete",
		map[string]any{"args": args}, nil)
}

// KernelError is a typed kernel failure decoded from an error HTTP response
// ({"error", "code", "meta"}). It preserves the kernel's stable error code so
// callers can branch on it without parsing prose.
type KernelError struct {
	Status  int
	Code    string
	Message string
	Meta    map[string]any
}

func (e *KernelError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("http %d: %s", e.Status, e.Message)
}

// Is lets the pure loop recognise a refused quote pin without importing this
// package: errors.Is(err, agent.ErrTermsChanged).
func (e *KernelError) Is(target error) bool {
	return target == agent.ErrTermsChanged && e.Code == "terms_changed"
}

// Code returns the kernel error code of err, or "" if err is not a KernelError.
func Code(err error) string {
	var ke *KernelError
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
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
		// A timeout means the call was dispatched and parked: the kernel settles it,
		// and a new run would be a second call.
		if ke.Code == "timeout" {
			return false
		}
		return ke.Status == http.StatusTooManyRequests || ke.Status >= 500
	}
	// A non-kernel error here is a transport failure (connection refused, reset,
	// DNS) — retryable.
	return true
}
