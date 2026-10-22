package main

import (
	"context"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// Kernel is everything job execution needs from the kernel client: dispatch,
// authoritative reads, the two affordances' current terms, and rating. The juice
// client satisfies it; tests use a fake.
type Kernel interface {
	agent.Runner
	Read(ctx context.Context, ref string) (agent.Candidate, error)
	Affordances(ctx context.Context) (reason, actionLookup agent.Candidate, err error)
	Rate(ctx context.Context, txID string, rating int, note string) error
}

// Executor runs jobs end to end: the affordances' current terms, the persona's
// transcript, the system prompt, the loop, the post-job rating pass, then the
// outcome record. It is the seam cmd/juice-agent drives; the loop itself stays
// in agent/.
type Executor struct {
	Kernel       Kernel
	PersonasDir  string
	CompactBytes int
	Retry        RetryPolicy
	RetryOK      func(error) bool
	Loop         agent.Config
	RateTasks    bool
	Handle       string // the agent's login, named in the system prompt
	AgentPath    string // agent.md, re-read at every job start so edits apply to the next job
	Log          func(event string, fields map[string]any)
}

// Execute runs one job to completion in its persona and returns its outcome.
// Errors from the loop are recorded but not returned as failures of Execute
// itself unless the transcript cannot be opened; a decide error becomes a failed
// outcome.
//
// Every priced call the job makes is paid from one meter: the actions the model
// chooses, and the reasoning, lookup, compaction and rating the harness makes on
// its own. The rating pass runs before the meter closes and the outcome is
// recorded, so the spend reported and recorded is all the job cost.
func (e *Executor) Execute(ctx context.Context, job agent.Job) (agent.Outcome, error) {
	logf := e.Log
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	// The terms are read for each job: a call pinned to them is refused, before
	// any funds are locked, if they move, and the next job reads the new ones.
	reason, lookup, err := e.Kernel.Affordances(ctx)
	if err != nil {
		logf("affordance_error", map[string]any{"job": job.ID, "error": err.Error()})
		return agent.Outcome{Status: agent.StatusFailed, Reason: "affordance: " + err.Error()}, nil
	}
	meter := &agent.Meter{Budget: job.Budget}
	jk := &jobKernel{kernel: e.Kernel, run: WrapRunner(e.Kernel, e.Retry, e.retryOK()),
		reason: reason, lookup: lookup, meter: meter}

	tr, err := OpenTranscript(e.PersonasDir, job.Persona, e.CompactBytes, jk)
	if err != nil {
		return agent.Outcome{Status: agent.StatusFailed, Reason: "transcript: " + err.Error()}, err
	}
	logf("persona_opened", map[string]any{"job": job.ID, "persona": job.Persona, "loaded": tr.Loaded()})

	sys := systemPrompt(e.Handle, e.Loop.ReplyTool, readFileOrEmpty(e.AgentPath))
	job.Messages = append([]agent.Message{sys}, job.Messages...)
	loop := e.Loop
	loop.ActionLookup, loop.Always = lookup.Ref, []agent.Candidate{lookup}
	deps := agent.Deps{Decider: jk, Catalog: jk, Runner: jk.run, Transcript: tr, Meter: meter, Log: logf}
	out, loopErr := agent.Run(ctx, job, loop, deps)
	if loopErr != nil {
		logf("loop_error", map[string]any{"job": job.ID, "error": loopErr.Error()})
	}

	// All() is this job's records across frames: bracketed calls are rated, and
	// an earlier job's transactions, already rated once (kernel D11), never are.
	if e.RateTasks {
		RateTask(ctx, tr.All(), jk, e.Loop.Intrinsics, logf)
	}
	// Closing the meter fixes the spend: a compaction the outcome record triggers
	// finds no budget, so nothing is paid after the figure is taken.
	meter.Close()
	out.Spent = meter.Spent
	logf("task_end", map[string]any{"job": job.ID, "status": out.Status, "reason": out.Reason, "turns": out.Turns, "spent": out.Spent})
	// Record the terminal outcome so recovery can tell a finished job from an
	// interrupted one.
	_ = tr.Append(ctx, agent.Message{Role: agent.RoleSystem, Kind: "outcome",
		Content: string(out.Status), Result: map[string]any{
			"status": string(out.Status), "reason": out.Reason, "spent": out.Spent, "turns": out.Turns,
		}})
	_ = tr.Close()
	return out, nil
}

func (e *Executor) retryOK() func(error) bool {
	if e.RetryOK != nil {
		return e.RetryOK
	}
	return func(error) bool { return false }
}

// Chatter summarises a conversation (used by compaction). The job's jobKernel
// implements it over the reasoning affordance.
type Chatter interface {
	Chat(ctx context.Context, messages []agent.Message) (string, error)
}

// jobKernel is the kernel as one job sees it: the loop's Decider and Catalog,
// the transcript's Chatter and the rating pass's Rater. Each reasoning or lookup
// call is checked against the job's meter, pinned to the terms the job read, run
// under one key across its retries (so a retry after a lost reply is answered
// with the first run's outcome and is not paid twice), and counted at what the
// kernel charged. Reads and ratings pass through: they cost nothing. It holds the
// kernel by name, not embedded, so no unmetered call is reachable through it.
type jobKernel struct {
	kernel         Kernel
	run            agent.Runner // kernel with the retry policy
	reason, lookup agent.Candidate
	meter          *agent.Meter
}

// call runs the affordance at its job's terms.
func (j *jobKernel) call(at agent.Candidate) juice.Call {
	return func(ctx context.Context, args map[string]any) (map[string]any, error) {
		if !j.meter.Fits(at.Price) {
			return nil, agent.ErrBudget
		}
		rr, err := j.run.Run(ctx, at.Ref, args, at.QuoteHash, agent.RunKey())
		j.meter.Add(agent.Charged(at.Price, rr, err))
		return rr.Result, err
	}
}

func (j *jobKernel) Read(ctx context.Context, ref string) (agent.Candidate, error) {
	return j.kernel.Read(ctx, ref)
}

func (j *jobKernel) Rate(ctx context.Context, txID string, rating int, note string) error {
	return j.kernel.Rate(ctx, txID, rating, note)
}

func (j *jobKernel) Decide(ctx context.Context, msgs []agent.Message, catalog []agent.Candidate, tools []agent.Tool) (*agent.Selection, error) {
	return juice.Decide(ctx, j.call(j.reason), msgs, catalog, tools)
}

func (j *jobKernel) Lookup(ctx context.Context, query string, limit int) ([]agent.Candidate, error) {
	return juice.Lookup(ctx, j.call(j.lookup), query, limit)
}

func (j *jobKernel) Chat(ctx context.Context, msgs []agent.Message) (string, error) {
	return juice.Chat(ctx, j.call(j.reason), msgs)
}

func (j *jobKernel) ChatJSON(ctx context.Context, msgs []agent.Message, schema map[string]any) (any, error) {
	return juice.ChatJSON(ctx, j.call(j.reason), msgs, schema)
}

// retryRunner wraps an agent.Runner: it retries only transport-level failures,
// never a kernel call that ran and failed (that is a result fed to the model).
// Every attempt carries the dispatch's key, so a retry after a lost reply is
// answered with the first run's outcome instead of running the action again.
type retryRunner struct {
	inner   agent.Runner
	policy  RetryPolicy
	retryOK func(error) bool
	sleep   func(time.Duration)
}

func (r retryRunner) Run(ctx context.Context, action string, args map[string]any, quoteHash, key string) (agent.RunResult, error) {
	for attempt := 0; ; attempt++ {
		res, err := r.inner.Run(ctx, action, args, quoteHash, key)
		// A reported transaction is a settled call: a retry under its key would
		// only repeat its outcome. The last attempt's settlement is returned with
		// its error, so the charge it reports is the one counted.
		if err == nil || !r.retryOK(err) || res.TxID != "" || attempt == r.policy.MaxRetries {
			return res, err
		}
		r.sleep(r.policy.backoff(attempt))
	}
}

// RetryPolicy is the backoff schedule: MaxRetries additional attempts after the
// first, delays doubling from BaseDelay (pi's 2s/4s/8s by default).
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
}

// DefaultRetryPolicy matches pi: 3 retries, 2s base (2s, 4s, 8s).
func DefaultRetryPolicy() RetryPolicy { return RetryPolicy{MaxRetries: 3, BaseDelay: 2 * time.Second} }

func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

// WrapRunner returns run fortified with the retry policy (transport failures only).
func WrapRunner(run agent.Runner, p RetryPolicy, retryOK func(error) bool) agent.Runner {
	return retryRunner{inner: run, policy: p, retryOK: retryOK, sleep: time.Sleep}
}
