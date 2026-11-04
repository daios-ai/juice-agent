package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

const (
	testReason = "sys@k/llm/chat"
	testLookup = "sys@k/lookup"
)

// fakeKernel is a kernel for the executor: it answers the reasoning affordance
// with a script of selections (and the judge and the compactor with canned
// replies), the action lookup with every scripted action, and any other action
// with its name. Each call is charged by kind, once per key, so a retried call
// is answered with its first outcome and never charged twice. The lookup's terms
// are pinned: a run carrying another quote_hash is refused, as the kernel does.
type fakeKernel struct {
	script      []agent.Selection
	i           int
	reasonPrice int            // the reasoning affordance's price
	lookupHash  string         // the action lookup's current quote_hash; "" reads as "q1"
	charges     map[string]int // per kind: "select", "judge", "summary", "lookup", or an action's address
	drop        int            // charge the next calls, then lose their replies
	affErr      error          // Affordances fails with this

	spent   int                        // every charge made
	seen    [][]map[string]any         // the chat each selection was asked over
	runs    []fakeRun                  // every call received
	byKey   map[string]agent.RunResult // first outcome per key
	refused   int                        // runs refused for a stale quote_hash
	rated     map[string]int
	explained int // explanation calls answered
}

type fakeRun struct{ action, quoteHash, key string }

func (k *fakeKernel) hash() string {
	if k.lookupHash == "" {
		return "q1"
	}
	return k.lookupHash
}

func (k *fakeKernel) Affordances(context.Context) (agent.Candidate, agent.Candidate, error) {
	if k.affErr != nil {
		return agent.Candidate{}, agent.Candidate{}, k.affErr
	}
	return agent.Candidate{Ref: testReason, Price: k.reasonPrice, QuoteHash: "r1"},
		agent.Candidate{Ref: testLookup, Description: "find actions", QuoteHash: k.hash()}, nil
}

// Read gives an action's current terms; its price is its charge.
func (k *fakeKernel) Read(_ context.Context, ref string) (agent.Candidate, error) {
	c := agent.Candidate{Ref: ref, Price: k.charges[ref]}
	if ref == testLookup {
		c.QuoteHash = k.hash()
	}
	return c, nil
}

func (k *fakeKernel) Run(_ context.Context, action string, args map[string]any, quoteHash, key string) (agent.RunResult, error) {
	k.runs = append(k.runs, fakeRun{action, quoteHash, key})
	if action == testLookup && quoteHash != k.hash() {
		k.refused++
		return agent.RunResult{}, &juice.KernelError{Status: 409, Code: "terms_changed", Message: "terms moved"}
	}
	rr, done := k.byKey[key]
	if !done {
		rr = k.answer(action, args)
		k.spent += rr.Charge
		if k.byKey == nil {
			k.byKey = map[string]agent.RunResult{}
		}
		k.byKey[key] = rr
	}
	if k.drop > 0 {
		k.drop--
		return agent.RunResult{}, errors.New("connection reset")
	}
	return rr, nil
}

// answer is what a call settles with, the first time its key is seen.
func (k *fakeKernel) answer(action string, args map[string]any) agent.RunResult {
	n := strconv.Itoa(len(k.runs))
	switch action {
	case testReason:
		msgs, _ := args["messages"].([]map[string]any)
		last := ""
		if len(msgs) > 0 {
			last = content(msgs[len(msgs)-1])
		}
		kind, content := "summary", "summary"
		switch {
		case strings.HasPrefix(last, "Select exactly ONE"):
			kind = "select"
			k.seen = append(k.seen, msgs)
			sel := agent.Selection{Action: "reply", Args: map[string]any{"answer": "fallback"}}
			if k.i < len(k.script) {
				sel = k.script[k.i]
				k.i++
			}
			b, _ := json.Marshal(sel)
			content = string(b)
		case strings.Contains(last, "JSON Schema"):
			kind, content = "judge", `{"ratings":[{"index":0,"rating":1,"note":"good"}]}`
		case strings.HasPrefix(last, "You could not complete this task"):
			kind, content = "explain", "I tried the time action repeatedly and could not finish."
			k.explained++
		}
		return agent.RunResult{TxID: "tx-r" + n, Charge: k.charges[kind], Result: map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}}}}
	case testLookup:
		var rows []any
		for _, s := range k.script {
			if s.Action != "reply" && s.Action != testLookup { // the lookup's entry is the always-offered one
				rows = append(rows, map[string]any{"action": s.Action, "description": "scripted"})
			}
		}
		return agent.RunResult{TxID: "tx-l" + n, Charge: k.charges["lookup"], Result: map[string]any{"results": rows}}
	}
	return agent.RunResult{TxID: "tx-" + action, Charge: k.charges[action], Result: map[string]any{"ran": action}}
}

func (k *fakeKernel) Rate(_ context.Context, txID string, rating int, _ string) error {
	if k.rated == nil {
		k.rated = map[string]int{}
	}
	k.rated[txID] = rating
	return nil
}

// outcomeRecord is the persona file's last record, which must be the outcome.
func outcomeRecord(t *testing.T, dir, persona string) agent.Message {
	t.Helper()
	recs, err := readRecords(filepath.Join(dir, persona+".jsonl"))
	if err != nil || len(recs) == 0 {
		t.Fatalf("persona %s: %v", persona, err)
	}
	last := recs[len(recs)-1]
	if last.Kind != "outcome" {
		t.Fatalf("persona %s ends with %+v, want the outcome record", persona, last)
	}
	return last
}

func replyIntrinsics() map[string]agent.Intrinsic {
	return map[string]agent.Intrinsic{
		"reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil
		}},
	}
}

func testExecutor(dir string, k Kernel) *Executor {
	return &Executor{
		Kernel:      k,
		PersonasDir: dir,
		Retry:       RetryPolicy{MaxRetries: 0},
		RetryOK:     func(error) bool { return false },
		Loop: agent.Config{
			ReplyTool: "reply", ActionLookup: testLookup, MaxTurns: 10, Intrinsics: replyIntrinsics(),
		},
		RateTasks: true,
		Handle:    "agent@k",
		AgentPath: filepath.Join(dir, "agent.md"),
	}
}

func userJob(id, persona, prompt string) agent.Job {
	return agent.Job{ID: id, Persona: persona, Budget: 100, Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}}}
}

// content is a chat message's text.
func content(m map[string]any) string { s, _ := m["content"].(string); return s }

func TestExecutorRunsRatesAndRecordsOutcome(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("AGENT-MARKER be terse"), 0o600)
	k := &fakeKernel{script: []agent.Selection{
		{Action: "sys@k/time"},
		{Action: "reply", Args: map[string]any{"answer": "noon"}},
	}}
	out, err := testExecutor(dir, k).Execute(context.Background(), userJob("t1", "p1", "what time is it?"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Status != agent.StatusAnswered || out.Answer["answer"] != "noon" {
		t.Fatalf("outcome = %+v", out)
	}
	if k.rated["tx-sys@k/time"] != 1 {
		t.Fatalf("expected sys@k/time rated 1, got %v", k.rated)
	}
	// The model saw agent.md in the system prompt.
	if first := k.seen[0][0]; first["role"] != "system" || !strings.Contains(content(first), "AGENT-MARKER") {
		t.Fatalf("first message = %+v, want the system prompt carrying agent.md", first)
	}
	outcomeRecord(t, dir, "p1")
}

// loopingKernel re-selects the time action forever: with no change of arguments
// the repeat guard ends the job as repeat-loop after one dispatch.
func loopingKernel() *fakeKernel {
	var script []agent.Selection
	for i := 0; i < 20; i++ {
		script = append(script, agent.Selection{Action: "sys@k/time"})
	}
	return &fakeKernel{script: script}
}

func TestAFailedJobExplainsItself(t *testing.T) {
	// A failed job makes one more reasoning call, paid like the rating pass, and
	// the model's account is the outcome's explanation; it joins the persona file
	// before the outcome record, so the next job sees it.
	dir := t.TempDir()
	k := loopingKernel()
	k.reasonPrice, k.charges = 1, map[string]int{"select": 1, "explain": 3}
	out, err := testExecutor(dir, k).Execute(context.Background(), userJob("t1", "p1", "time?"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Status != agent.StatusFailed || out.Reason != "repeat-loop" {
		t.Fatalf("outcome = %+v, want failed/repeat-loop", out)
	}
	if k.explained != 1 || !strings.HasPrefix(out.Explanation, "I tried") {
		t.Fatalf("explained %d, explanation %q; want one call and its text", k.explained, out.Explanation)
	}
	if out.Spent != k.spent || out.Spent < 3 {
		t.Fatalf("spent %d, kernel charged %d; want the explanation's 3 counted", out.Spent, k.spent)
	}
	recs, _ := readRecords(filepath.Join(dir, "p1.jsonl"))
	if n := len(recs); n < 2 || recs[n-2].Kind != "explanation" || recs[n-2].Content != out.Explanation {
		t.Fatalf("persona file ends %+v, want the explanation before the outcome", recs[max(0, len(recs)-2):])
	}
}

func TestAnExplanationTheBudgetCannotPayIsSkipped(t *testing.T) {
	// Five selections at 1 each exhaust a budget of 5: the explanation does not
	// fit, is not made, and the job delivers its reason alone.
	dir := t.TempDir()
	k := loopingKernel()
	k.reasonPrice, k.charges = 1, map[string]int{"select": 1, "explain": 1}
	var events []string
	e := testExecutor(dir, k)
	e.Log = func(ev string, _ map[string]any) { events = append(events, ev) }
	job := userJob("t1", "p1", "time?")
	job.Budget = 5
	out, _ := e.Execute(context.Background(), job)
	if out.Status != agent.StatusFailed || out.Explanation != "" || k.explained != 0 || out.Spent != 5 ||
		!slices.Contains(events, "explain_error") {
		t.Fatalf("outcome %+v, explained %d, events %v; want failed, unexplained at 5, explain_error logged", out, k.explained, events)
	}
}

func TestAnAnsweredJobIsNotExplained(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{script: []agent.Selection{{Action: "reply", Args: map[string]any{"answer": "noon"}}}}
	out, _ := testExecutor(dir, k).Execute(context.Background(), userJob("t1", "p1", "time?"))
	if out.Status != agent.StatusAnswered || out.Explanation != "" || k.explained != 0 {
		t.Fatalf("outcome %+v, explained %d; want answered with no explanation call", out, k.explained)
	}
}

// A second job in a persona continues the first's conversation, with one system
// prompt (its own), and rates only its own calls: the first job's transaction was
// rated once already, and the kernel refuses a second rating (kernel D11).
func TestExecutorContinuesPersonaAndRatesOnlyThisJob(t *testing.T) {
	dir := t.TempDir()
	k1 := &fakeKernel{script: []agent.Selection{{Action: "sys@k/time"}, {Action: "reply", Args: map[string]any{"answer": "noon"}}}}
	if _, err := testExecutor(dir, k1).Execute(context.Background(), userJob("j1", "alice", "what time is it?")); err != nil {
		t.Fatal(err)
	}
	k2 := &fakeKernel{script: []agent.Selection{{Action: "sys@k/web"}, {Action: "reply", Args: map[string]any{"answer": "done"}}}}
	if _, err := testExecutor(dir, k2).Execute(context.Background(), userJob("j2", "alice", "and the weather?")); err != nil {
		t.Fatal(err)
	}
	// The view ends with the new request, then the selection prompt; an earlier
	// job's system prompt or outcome record would be a second system message.
	view := k2.seen[0]
	var sawFirst, sawFirstReply bool
	nsys := 0
	for _, m := range view {
		switch c := content(m); {
		case m["role"] == "system":
			nsys++
		case m["role"] == "user" && c == "what time is it?":
			sawFirst = true
		case strings.HasPrefix(c, "result of reply:") && strings.Contains(c, "noon"):
			sawFirstReply = true
		}
	}
	if !sawFirst || !sawFirstReply || nsys != 1 || view[0]["role"] != "system" || content(view[len(view)-2]) != "and the weather?" {
		t.Fatalf("second job's view does not continue the first: %+v", view)
	}
	if _, again := k2.rated["tx-sys@k/time"]; again || k2.rated["tx-sys@k/web"] != 1 {
		t.Fatalf("second job rated %v, want only tx-sys@k/web", k2.rated)
	}
	// Another persona starts with nothing but its own prompt.
	k3 := &fakeKernel{}
	testExecutor(dir, k3).Execute(context.Background(), userJob("j3", "bob", "hi"))
	if v := k3.seen[0]; len(v) != 3 || content(v[1]) != "hi" {
		t.Fatalf("a new persona's view = %+v, want only its system prompt, request and selection prompt", v)
	}
}

// jobKernelOf is a job's kernel over k with budget, at k's current terms.
func jobKernelOf(t *testing.T, k *fakeKernel, budget int, retries int) *jobKernel {
	t.Helper()
	reason, lookup, err := k.Affordances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := WrapRunner(k, RetryPolicy{MaxRetries: retries}, alwaysRetry)
	return &jobKernel{kernel: k, run: run, reason: reason, lookup: lookup, meter: &agent.Meter{Budget: budget}}
}

// A reasoning call runs only if it fits what the job has left, carries the
// terms the job read under a key of its own, and counts what the kernel charged.
func TestJobKernelChecksPinsAndCounts(t *testing.T) {
	k := &fakeKernel{reasonPrice: 3, charges: map[string]int{"summary": 2}}
	j := jobKernelOf(t, k, 5, 0)
	for range 2 {
		if _, err := j.Chat(context.Background(), []agent.Message{{Role: agent.RoleUser, Content: "sum up"}}); err != nil {
			t.Fatal(err)
		}
	}
	if j.meter.Spent != 4 || k.spent != 4 {
		t.Fatalf("spent = %d (kernel %d), want the reported 2 twice, not the price", j.meter.Spent, k.spent)
	}
	if r := k.runs; len(r) != 2 || r[0].quoteHash != "r1" || r[0].key == "" || r[0].key == r[1].key {
		t.Fatalf("runs = %+v, want two pinned to r1 under distinct keys", r)
	}
	if _, err := j.Chat(context.Background(), nil); !errors.Is(err, agent.ErrBudget) || len(k.runs) != 2 {
		t.Fatalf("err = %v after %d runs, want ErrBudget and no third run (4 + 3 > 5)", err, len(k.runs))
	}
}

// A reply lost after the kernel charged is retried under the same key, so the
// retry is answered with the first outcome and the call is paid once.
func TestALostReplyIsPaidOnce(t *testing.T) {
	k := &fakeKernel{reasonPrice: 3, charges: map[string]int{"summary": 2}, drop: 1}
	j := jobKernelOf(t, k, 100, 1)
	if _, err := j.Chat(context.Background(), []agent.Message{{Role: agent.RoleUser, Content: "sum up"}}); err != nil {
		t.Fatal(err)
	}
	if len(k.runs) != 2 || k.runs[0].key != k.runs[1].key || k.spent != 2 || j.meter.Spent != 2 {
		t.Fatalf("runs = %+v, kernel spent %d, meter %d; want one key, paid 2 once", k.runs, k.spent, j.meter.Spent)
	}
}

// A call whose every attempt went unanswered may still have been charged, so it
// counts at its price.
func TestAnUnansweredCallCountsItsPrice(t *testing.T) {
	k := &fakeKernel{reasonPrice: 3, charges: map[string]int{"summary": 2}, drop: 2}
	j := jobKernelOf(t, k, 100, 1)
	if _, err := j.Chat(context.Background(), nil); err == nil {
		t.Fatal("want the transport error")
	}
	if j.meter.Spent != 3 {
		t.Fatalf("meter = %d, want the price 3", j.meter.Spent)
	}
}

// The spend returned and recorded is everything the job paid: its turns, its
// tools and its rating, all of which come after the loop's own figure.
func TestTheOutcomeCountsEverythingTheJobPaid(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{reasonPrice: 2, charges: map[string]int{"select": 1, "judge": 2, "sys@k/time": 5}, script: []agent.Selection{
		{Action: "sys@k/time"}, {Action: "reply", Args: map[string]any{"answer": "noon"}},
	}}
	out, _ := testExecutor(dir, k).Execute(context.Background(), userJob("t1", "p1", "time?"))
	rec := outcomeRecord(t, dir, "p1")
	if out.Spent != 9 || k.spent != 9 || rec.Result["spent"] != float64(9) {
		t.Fatalf("spent = %d, recorded %v, kernel %d; want 2 turns + 5 + rating 2 = 9", out.Spent, rec.Result["spent"], k.spent)
	}
	if k.rated["tx-sys@k/time"] != 1 {
		t.Fatalf("rated = %v, want the time call rated", k.rated)
	}
}

// Compaction is paid like any call, and nothing is paid once the figure is
// taken: the compaction the outcome record triggers finds the meter closed.
func TestNothingIsPaidAfterTheFigure(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{reasonPrice: 1, charges: map[string]int{"select": 1, "summary": 1, "judge": 1}, script: []agent.Selection{
		{Action: "sys@k/time"}, {Action: "reply", Args: map[string]any{"answer": "noon"}},
	}}
	e := testExecutor(dir, k)
	e.CompactBytes = 1 // every append compacts
	out, _ := e.Execute(context.Background(), userJob("t1", "p1", "time?"))
	rec := outcomeRecord(t, dir, "p1")
	if out.Spent != k.spent || rec.Result["spent"] != float64(k.spent) || k.spent <= 3 {
		t.Fatalf("spent = %d, recorded %v, kernel %d; want them equal, compaction included", out.Spent, rec.Result["spent"], k.spent)
	}
}

// A rating the job cannot pay for is skipped, and the skip is logged.
func TestRatingIsSkippedWhenItDoesNotFit(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKernel{reasonPrice: 1, charges: map[string]int{"select": 1, "judge": 1, "sys@k/time": 5}, script: []agent.Selection{
		{Action: "sys@k/time"}, {Action: "reply", Args: map[string]any{"answer": "noon"}},
	}}
	var events []string
	e := testExecutor(dir, k)
	e.Log = func(ev string, _ map[string]any) { events = append(events, ev) }
	job := userJob("t1", "p1", "time?")
	job.Budget = 7 // two turns and the time call, nothing left to judge
	out, _ := e.Execute(context.Background(), job)
	if out.Status != agent.StatusAnswered || out.Spent != 7 || len(k.rated) != 0 || !slices.Contains(events, "judge_error") {
		t.Fatalf("outcome %+v, rated %v, events %v; want answered at 7, unrated, judge_error logged", out, k.rated, events)
	}
}

// Each job reads the affordances' terms again, so a change of the lookup's
// terms reaches the next job instead of having every lookup refused.
func TestEachJobReadsTheTermsAgain(t *testing.T) {
	dir := t.TempDir()
	script := []agent.Selection{{Action: testLookup, Args: map[string]any{"query": "x"}}, {Action: "reply", Args: map[string]any{"answer": "ok"}}}
	k := &fakeKernel{script: script}
	e := testExecutor(dir, k)
	e.Execute(context.Background(), userJob("j1", "p1", "find"))
	k.lookupHash, k.i = "q2", 0
	before := len(k.runs)
	e.Execute(context.Background(), userJob("j2", "p1", "find again"))
	pinned := 0
	for _, r := range k.runs[before:] {
		if r.action == testLookup && r.quoteHash == "q2" {
			pinned++
		}
	}
	if k.refused != 0 || pinned != 2 {
		t.Fatalf("refused %d, lookups at q2 %d; want the opening and the chosen lookup both at the new terms", k.refused, pinned)
	}
}

// An affordance that no longer meets its contract fails the job, not the daemon.
func TestAnAffordanceFailureFailsTheJob(t *testing.T) {
	k := &fakeKernel{affErr: errors.New("reason = x@k/y: output.properties.choices: missing (want array)")}
	out, err := testExecutor(t.TempDir(), k).Execute(context.Background(), userJob("j1", "p1", "hi"))
	if err != nil || out.Status != agent.StatusFailed || !strings.HasPrefix(out.Reason, "affordance: reason = x@k/y") || len(k.runs) != 0 {
		t.Fatalf("outcome = %+v, err = %v, runs %d; want a failed job naming the affordance, nothing run", out, err, len(k.runs))
	}
}

func alwaysRetry(error) bool { return true }

type flakyRunner struct {
	failN int
	calls int
	keys  []string
}

func (r *flakyRunner) Run(_ context.Context, _ string, _ map[string]any, _, key string) (agent.RunResult, error) {
	r.calls++
	r.keys = append(r.keys, key)
	if r.calls <= r.failN {
		return agent.RunResult{}, errors.New("network")
	}
	return agent.RunResult{Result: map[string]any{"ok": true}}, nil
}

func TestRetryRunnerRetriesTransportUnderOneKey(t *testing.T) {
	r := &flakyRunner{failN: 1}
	wrapped := retryRunner{inner: r, policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond},
		retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	if _, err := wrapped.Run(context.Background(), "x", nil, "", "key-1"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// Both attempts name the same run, so the kernel answers a repeat of a run
	// that already committed with its first outcome rather than running it again.
	if r.calls != 2 || r.keys[0] != "key-1" || r.keys[1] != "key-1" {
		t.Fatalf("calls = %d keys = %q, want 2 attempts under key-1", r.calls, r.keys)
	}
}

func TestRetryRunnerDoesNotRetryParkedRun(t *testing.T) {
	calls := 0
	parked := &juice.KernelError{Status: 504, Code: "timeout"}
	wrapped := retryRunner{inner: runnerResult(func() (agent.RunResult, error) { calls++; return agent.RunResult{}, parked }),
		policy: RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond}, retryOK: juice.IsRetryable, sleep: func(time.Duration) {}}
	if _, err := wrapped.Run(context.Background(), "x", nil, "", "key-1"); err != parked || calls != 1 {
		t.Fatalf("err = %v calls = %d, want the parked timeout returned after one attempt", err, calls)
	}
}

// A retryable failure that reports a settlement is a call that ran: it is not
// retried, and its reported charge, not the price, is what the job pays.
func TestRetryKeepsAReportedSettlement(t *testing.T) {
	calls := 0
	failed := &juice.KernelError{Status: 503, Code: "internal"}
	inner := runnerResult(func() (agent.RunResult, error) {
		calls++
		return agent.RunResult{TxID: "tx-1", Charge: 2}, failed
	})
	wrapped := retryRunner{inner: inner, policy: RetryPolicy{MaxRetries: 3}, retryOK: juice.IsRetryable, sleep: func(time.Duration) {}}
	rr, err := wrapped.Run(context.Background(), "x", nil, "", "key-1")
	if err != failed || calls != 1 || agent.Charged(5, rr, err) != 2 {
		t.Fatalf("calls = %d, rr = %+v, err = %v; want one attempt charged 2", calls, rr, err)
	}
}

// The last attempt's result rides with its error when every attempt failed.
func TestExhaustedRetriesReturnTheLastResult(t *testing.T) {
	calls := 0
	inner := runnerResult(func() (agent.RunResult, error) {
		calls++
		return agent.RunResult{Result: map[string]any{"attempt": calls}}, errors.New("connection reset")
	})
	wrapped := retryRunner{inner: inner, policy: RetryPolicy{MaxRetries: 2}, retryOK: alwaysRetry, sleep: func(time.Duration) {}}
	rr, err := wrapped.Run(context.Background(), "x", nil, "", "key-1")
	if err == nil || calls != 3 || rr.Result["attempt"] != 3 {
		t.Fatalf("calls = %d, rr = %+v, err = %v; want 3 attempts and the third's result", calls, rr, err)
	}
}

// runnerResult is a Runner answering every call with f's result and error.
type runnerResult func() (agent.RunResult, error)

func (f runnerResult) Run(context.Context, string, map[string]any, string, string) (agent.RunResult, error) {
	return f()
}

func TestBackoffDoubles(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: 2 * time.Second}
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := p.backoff(attempt); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// bracketExecutor opens a subtask on the root's first turn, replies inside the
// bracket, then replies at root — exercising the full frame recursion.
func bracketExecutor(dir string) *Executor {
	return &Executor{
		Kernel: &fakeKernel{script: []agent.Selection{
			{Action: "subtask", Args: map[string]any{"instruction": "sub"}},
			{Action: "reply", Args: map[string]any{"answer": "sub-done"}},
			{Action: "reply", Args: map[string]any{"answer": "root-done"}},
		}},
		PersonasDir: dir,
		RetryOK:     func(error) bool { return false },
		Loop: agent.Config{
			ReplyTool: "reply", SubtaskTool: "subtask", ActionLookup: testLookup, MaxTurns: 10,
			Tools: []agent.Tool{
				{Name: "reply", InputSchema: map[string]any{"type": "object", "required": []any{"answer"}}},
				{Name: "subtask", InputSchema: map[string]any{"type": "object", "required": []any{"instruction"}}},
			},
		},
	}
}

func TestConcurrentBracketTasksDoNotCrossTalk(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make([]agent.Outcome, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := bracketExecutor(dir) // independent kernel and meter per task
			task := agent.Job{ID: "task-" + strconv.Itoa(i), Persona: "task-" + strconv.Itoa(i), Budget: 100,
				Messages: []agent.Message{{Role: agent.RoleUser, Content: "go"}}}
			out, _ := e.Execute(context.Background(), task)
			results[i] = out
		}(i)
	}
	wg.Wait()
	// Every task answered independently with its own root reply; each transcript
	// belongs to exactly one task.
	for i, out := range results {
		if out.Status != agent.StatusAnswered || out.Answer["answer"] != "root-done" {
			t.Fatalf("task %d outcome = %+v", i, out)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "task-"+strconv.Itoa(i)+".jsonl"))
		// Exactly one *root* user message (the bracket inherits the parent's history,
		// so a bracket-tagged copy of "go" is expected and excluded here). A second
		// root "go" would mean another task's run bled into this file.
		if strings.Count(string(data), `{"role":"user","content":"go"}`) != 1 {
			t.Fatalf("task %d transcript has cross-task content:\n%s", i, data)
		}
	}
}
