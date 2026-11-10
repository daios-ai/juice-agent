# Juice Agent Requirements

Version: 0.9.0
Status: implementation requirements
Codename: `juice-agent`
Copyright (C) 2026 Pedro A. Ortega <pedro.ortega@gmail.com>

This document states what the agent daemon **must** be; `agent-design.md` states why. Where this
document and the design differ, this document governs the implementation. It depends on, and never
restates, two documents mirrored from the kernel repository by `copy-juicedocs.sh`: the kernel
contract `juicedocs/requirements.md` and the ecosystem standard `juicedocs/ecosystem-standard.md`.
A requirement here must violate neither. Citations use the kernel's own labels: *kernel §N* for a
section, *kernel D/P/G/U N* for a design decision, protocol clause, guarantee, or user story, and
*ecosystem §N* for the standard.

Revision 0.9.0 narrows the repeat guard and makes a failed job explain itself. A repeat is the
last dispatched call re-selected with the same arguments after it did not fail; the same action
over other arguments, and a retry after a failure, dispatch; a `timeout` is a failure when it
reports a `tx_id` and a parked, still-running call when it does not (§6). A failed job makes one
more reasoning call, paid from its budget, and delivers the model's account of the failure with its
status and reason (§3, §10).

Revision 0.8.0 adds the standard's `setup` command (ecosystem §3): `juice-agent setup`, hidden
from usage, writes the schema; `make install` runs it after placing the binary, and every start runs
it (§11).

Revision 0.7.0 follows the ecosystem standard's configuration rule (ecosystem §3) and its new
section numbers. Every start writes `config.json`'s JSON Schema, generated from the one default value
in the program; first start writes the file whole, `$schema` first; `reason` and `action_lookup`
have no default, since theirs names the login's kernel, and are written into a file that lacks them.
A `null`, a key not named exactly, or a value of the wrong type stops the start, naming the key, and
a refused file is never rewritten (§11).

Revision 0.6.0 allows one daemon per agent. `serve` holds `serve.lock` in the agent's home under an
exclusive `flock` for as long as it serves, with its process id in the file, as a kernel holds its
own (ecosystem §4). A second `serve` of the login is refused before recovery, so two daemons can no
longer take the same tasks, append to the same persona files, or mark each other's jobs
interrupted (§11).

Revision 0.5.0 makes the budget cover every call made for a job. The harness's own calls of the two
affordances (each turn's reasoning, the opening lookup, compaction, rating) are priced like any
other: `llm/chat` costs the model's price, which the kernel's operator sets. Each is checked against
what the job has left, pinned to the terms the job read, and counted at what the kernel charged
(§3, §11). The affordances' terms are read again for each job, so a change of price reaches the next
job without a restart.

Revision 0.4.0 resynchronised with the kernel contract 0.16 and later. A task now names its
`requester`, whom it acts for: completion runs as the requester, the completer lending neither
identity nor funds (kernel D6), and the kernel shows the agent a task only from a requester its
operator approved (kernel U52). All work therefore arrives as tasks, and the agent specialises to no
application: the channel layer, the HTTP intake and the service-specific chat channel are removed.
Each task joins a **persona**, a conversation history keyed from the task's certified fields and one
application-supplied identifier (§10). The harness depends on exactly two actions, reasoning and
action lookup, named by address in configuration and checked against their contracts at startup
(§11). Self-extension (the maker) and the learned memory
file are removed; a persona's history is the agent's only learned state, and one human-authored
file, `agent.md`, is shared by all personas (§9).

Revision 0.3.0 resynchronised with the kernel contract 0.15 and adopted the ecosystem standard.
Every name on the wire is an address (`handle@kernel`, `owner@kernel/name`; kernel D15). The kernel's
suspended call is a *task* (kernel D6); the agent's own unit of work is therefore a *job*. An
action's terms and its `evidence` (kernel D20, never summed or scored, U39) come from an
authoritative read, and the `quote_hash` from that read pins the terms on dispatch. The agent is
named by its login and holds no secret: it reads the kernel's address and its session from the juice
CLI's client records (ecosystem §5) and lives at `$JUICE_HOME/agents/<login>/` (ecosystem §2, §7).

## 1. Purpose and position

**Goal.** A Juice kernel wraps HTTP endpoints into standardized actions, each with a natural-language
description, JSON input and output schemas, and a fixed price. Any account can sign up and use those
actions; the kernel does not distinguish a human account from an agent account. The kernel also
provides, as ordinary actions, the LLM calls (`sys@<kernel>/llm/*`) with which a reasoning step is performed.
`juice-agent` is the agent in that picture: a program that operates a user account, thinks by calling
the kernel's LLM actions, and acts by calling the kernel's other actions. Everything it can do is what
any user can do — search the catalog, run actions, complete tasks addressed to it, and pay from its
own balance.

**Signal.** The agent is reactive: it starts a job only when the kernel signals it, and the
kernel's one signal to an account is a task addressed to it (kernel D6) by a requester the account's
operator approved (kernel U52). A task is a single primitive
read two ways — as a suspended computation (price parked, process open until the required caller's
input arrives) and as a request (the partial arguments are the message, the required caller is the
addressee, the completing input is the reply, and completion delivers it). Its shape is minimal rather
than convenient because one primitive serves both readings.

**Message.** To the agent a waiting task is an addressed partial application: an action, the
arguments already bound (`partial_args`), and the schema of those left open (`allowed_input`,
the action's input schema minus the bound keys). Trace, process, and price are the funder's concern
and play no part in the agent's decision. This is the agent's basic message and the only form in
which work reaches it; every job begins from one. The agent's whole decision is what to supply for
the open arguments — often nothing, as with the `message` native, whose `sink` task is fully bound
and completion merely acknowledges it: the reply then rides in the completion's arguments, which the
sender can read.

`juice-agent` is therefore a Go daemon implementing a general task-solving agent whose entire
capability surface is a Juice kernel's action catalog. It is an ordinary kernel client: it
acts as one login, `handle@kernel` (ecosystem §5), and derives every agent affordance from that one
fact — reasoning and discovery from two configured actions (by default the `llm/chat` and `lookup`
natives), tools from the actions callable under `CanCall` (kernel D5), dispatch from `POST /v1/run`,
budget from its wallet, address from its login, and accountability from the transaction role law
(kernel D4, D6).

The daemon must add no agent-specific API to the kernel and require none. It must hold no model
credentials, no API keys, and no provider adapters; the kernel is its sole external interface. Many
agents are one binary: `juice-agent serve <login>` launches the same binary as a distinct kernel
account rooted in its own home (§9), and agents compose only through the kernel, never through a
private protocol. Nothing in the agent names, or specialises to, any application.

## 2. Packages and implementation constraints

Use Go. `go build ./...` and `go test ./...` must pass **offline** — no kernel, no LLM, no network.
The Go packages:

```text
agent/           the loop: pure, interface-driven
juice/           kernel client: the only package that speaks HTTP to the kernel
cmd/juice-agent/ package main: everything else (lifecycle, config, home resolution, intake and
                 personas, the policy shell, the bench runner)
bench/           experiment-suite data (tasks.json, results/) — no Go
flows/           end-to-end shell flows against the compiled binary — no Go
```

Dependency law, mirroring the kernel's own (kernel D1):

- `agent` must import neither `net/http` nor `juice`. It depends only on the small interfaces it
  defines (`Decider`, `Catalog`, `Runner`, `Transcript`); tests use fakes. This keeps the loop pure.
- `juice` is the only package that speaks HTTP to the kernel. It names no action except the two
  configured affordances it is given.
- `cmd/juice-agent` depends on both and is the sole `package main`.

Keep file and package counts small; name files for their function; do not split a file for size
alone. **Every production Go file must have a corresponding `_test.go`** with independent tests for
its logic.

## 3. Data model

The agent adds no durable object to the kernel; its own state is the Job, the Outcome, and the
persona files (§7, §10). IDs are stable opaque strings; budgets and prices are non-negative integers
in the kernel's base units.

| Object    | Fields                                                                        | Rules                                                                                                                                                                                                             |
| --------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Job`     | `id`, `persona`, `messages`, `budget`                                         | One request for the agent: an id, the persona whose history it continues (§10), its new messages, and a spend ceiling covering every priced call made for it: the actions the model chooses and the harness's own calls of the two affordances (§11). One job = one loop run, appended to its persona's file.                                     |
| `Outcome` | `status`, `answer`, `reason`, `explanation`, `spent`, `turns`                | `status ∈ {answered, failed, interrupted}`. `answer` is the reply object (on `answered`). `reason` gives the failure class (`turn-cap`, `repeat-loop`, `budget`, `terms-changed`, `affordance: …`, `decide-error: …`) when not answered. `explanation` is the model's own account of a failed job — what it attempted, what went wrong, what would be needed — written by the policy layer after the loop (§10, "Completion") and empty when the job could not pay for it. `spent` is what the kernel charged for the job — every call's, the reasoning, lookup, compaction and rating calls included: the sum of each call's reported `charge` (on success, and in a failure's `meta`), `0` for a call refused with `terms_changed`, and the price for a call whose settlement was not reported (a parked call may still draw it); reconstructible from the kernel ledger (the agent is the payer of every one, kernel D4). `turns` is the count of model round-trips: a turn refused for the budget or for moved terms never reaches the model and is not counted. |

A job's outcome must be delivered by completing its originating task. The daemon serves jobs from a
bounded worker pool; each job's loop is serial, jobs in different personas are concurrent, and a
persona runs one job at a time.

## 4. The loop (`agent/`)

Each turn substitutes the kernel at both ends. In pseudocode — the whole of the pure loop:

```text
run(job):                                                   # meter: the job's spend, shared with every call made for it
  cands = merge(Catalog.Lookup(job.prompt, k))              # seed the working set; skipped if it does not fit
  loop:
    sel = Decider.Decide(messages, offered, tools)          # ONE llm/chat round-trip, JSON extracted, metered
    if sel is budget:          fail(budget)                 # the turn does not fit: the model cannot be told
    if sel is terms_changed:   fail(terms-changed)          # the reasoning terms moved since the job read them
    if sel is error:  fail after MaxDecideErrors consecutive # policy layer retries transport errors only
    if sel.action == ReplyTool:   return answered(sel.args)  # THE STOP RULE — nothing dispatched
    if repeat-guard trips (§6):   drop, nudge, re-query
    if sel.action not offered:    feed back as invalid       # never dispatched at an unknown price
    if spent + c.price > job.budget:  notify; continue      # the model is told, not killed
    append sel; res = Runner.Run(sel.action, sel.args, c.quote_hash, key)  # key: fresh per dispatch
    spent += res.charge if settled, 0 if terms_changed, else c.price
    if res is terms_changed: evict c                        # re-offered only after a fresh read
    append res
    if sel.action == LookupAction: merge res into cands (bounded, recency-evicted)
    if turns == MaxTurns: fail(turn-cap)

merge(rows): for each row, c = Catalog.Read(row.action); drop c if the read fails or c.effect ≠ ""
```

**Turn primitive.** A turn is exactly one call of the reasoning affordance (`llm/chat` shape) over the transcript plus a tool list the
client builds itself: the offered candidates (each rendered with description, price, this kernel's
record of it, and a compact schema, §8) followed by the plain-named harness tools. The model must
reply with a single JSON object `{action, args}`, extracted client-side by a balanced-brace parser.
The loop must **not** use `llm/decide`: it selects among action references only (so plain-named
harness tools could not be candidates), and its grammar-constrained output corrupts copied strings on
Ollama (measured).
Validation splits by tool kind: harness-tool args get a required-keys check in-harness; catalog args
are validated kernel-side at dispatch (`POST /v1/run`, which always carries `args`, `{}` when empty),
and a schema violation is fed back to the model.

**Stop rule.** The run ends exactly when the model selects the reply tool (`Config.ReplyTool`,
`"reply"`). Termination is decided by content — a selection like any other — never by a provider stop
reason and never by a heuristic over free text. At the root this answers the task; inside a bracket
it pops the frame (§5). The reply tool is never dispatched and nothing is registered for it.

**Harness tools are not catalog rows.** `reply` and `subtask` are plain-named
affordances the daemon offers and intercepts; they must **not** be registered on the platform, so
the catalog stays a truthful registry of real capabilities. Harness-tool names contain no `/` and
no `@`, so they are unambiguous against action addresses. Each carries a schema only to
standardise the args the model supplies. The loop keeps an intrinsics table keyed by tool name;
adding an agent-native capability is a table entry plus a `Tool` definition, with no platform
registration and no artifact.

**Discovery is in-band.** The action-lookup affordance (`action_lookup` in config, by default
`sys@<kernel>/lookup`; `Config.ActionLookup`) is a permanent candidate, offered with the terms the
kernel holds for it, read at the start of each job (§11). The model expands its own tool set by selecting it; the harness runs it as an
ordinary call and merges the results into the working set, bounded by `candidate_limit` and evicted
least-recently-merged first. The working set is per-frame and starts fresh each run.

**Offered terms are the dispatched terms.** A lookup row is indicative (kernel D17), so every merged
row is replaced by the action's authoritative read (`GET /v1/actions?ref=`): price, `quote_hash`,
`effect`, and `evidence`. Only an offered candidate may be dispatched — a harness tool, a working-set
member, or an always-offered candidate — and it is dispatched pinned to the `quote_hash` it was read
with, so the kernel refuses it with `terms_changed` before locking funds if the terms moved (kernel
P2). A refused candidate leaves the working set until read again. An action carrying an `effect`
moves the caller's own balance beyond its price (kernel D18), so it is never offered.

**Follow-ups and delegation.** A follow-up is a new task, hence a new job in the same persona, which
continues that persona's history (§10). The loop accepts an optional steering hook polled before each
model call; the daemon supplies none. Delegation to another agent stays out of the loop: use its
published actions or park a task — the kernel is the orchestrator. In-loop decomposition is the
subtask bracket (§5).

## 5. Brackets (subtasks)

The subtask tool (`Config.SubtaskTool`, `"subtask"`) opens a **bracket** — a nested loop frame that
runs in its own scope and rolls up to a single result in the parent chat.

- **Down.** Selecting `subtask {instruction, context?}` opens a child frame seeded with the parent's
  **full turn-by-turn history**, plus the optional context and the instruction. Inheritance flows
  only downward.
- **Inside.** The child runs the ordinary loop with its own working set but is **not offered the
  subtask tool** (`childCfg.SubtaskTool = ""`; the tool is removed from `childCfg.Tools`), so nesting
  is capped at one sublevel: root → subtasks, never deeper. This is a structural cap, not a prompt
  nudge — a child that could re-delegate an undoable clone of its own task spirals to the turn cap
  (observed live).
- **Up.** Selecting reply inside the bracket pops it: the parent chat receives exactly one
  tool-result `{action: subtask, result: <reply args>}`; the child's internal turns never enter the
  parent's model view.

**Shared bound.** Turns are one `counters{turns}` per job and spend one `Meter`, shared with every call made for the job, threaded by pointer
through the recursion, so a subtask tree totals ≤ `max_turns` and ≤ `budget` — never per-frame.
Exhaustion inside a bracket rolls up an error result the parent can act on; at the root it fails the
job.

**Durable but clean.** Every record is teed to the persona's file, tagged with its bracket id;
bracket ids continue across a persona's jobs. `Transcript.All()` (rating) sees this job's bracketed
records and no earlier job's; the per-frame `Messages()` view (the model's context) excludes sibling
and child brackets, exposing at each node only the root→node path.

## 6. Repeat guard

Budget and the turn cap both fail against a *free, succeeding* action the model re-selects forever
(observed: the same message posted 31× with no `reply`). The guard catches the one case it can
prove: a call whose outcome is already known.

- A selection is a repeat iff it names the last *dispatched* action **with the same arguments**
  (compared as canonical JSON) and that call **did not fail**. The reply tool is exempt. A
  different argument is new work and dispatches; a retry after a failure is the model's call and
  dispatches. A kernel `timeout` is a settled failure when it reports a `tx_id` (kernel §8: "a WASM
  timeout completion is reported as settled, not as a parked dispatch"); one reporting none is a
  cross-kernel call the kernel parked and will settle (API, Run), which did not fail and is still
  executing, so re-running it would run it twice.
- A repeat is **neither persisted nor dispatched**: the loop drops it, ensures a single reworded
  challenge nudge sits at the tail (*"you already ran `<action>` with these same arguments; its
  result is above or still pending; select `reply` if the task is done, otherwise change the
  arguments or choose another action"*), and re-queries. Leaving the repeat in context would
  self-prime an autoregressive model; because the repeat never dispatches, the side effect is
  bounded to the one legitimate call.
- Each re-query costs a turn, so `max_turns` bounds the rephrase loop. After `max_repeats`
  consecutive drops (`Config.MaxRepeats`, default 3) the frame hard-stops with `reason = repeat-loop`
  (a bracket rolls up `{error: "repeat-loop"}`). State is per-frame. `max_repeats < 0` disables the
  guard.

An earlier rule matched on the action name alone, because a sampled model paraphrases its own
prior output (measured: 19 posts, 16 distinct strings, 0 byte-identical). That rule failed most
multi-step tasks, which call one action over several inputs. A paraphrased re-post is now left to
the turn cap and the budget (§11), which bound it as they bound any zero-price loop.

## 7. Persistence and compaction

**Persona file.** Append-only JSONL, one file per persona under `personas/` (§10, §11), written as
each record is produced; every job of the persona appends to it. Aborts and interruptions must be
persisted as records, never merely thrown: a persona file found at startup whose last record is not
terminal (`outcome` or `interrupted`) is marked `interrupted` — the agent-side mirror of the kernel's
`reason = interrupted` recovery (kernel D3, G4). Recovery must be idempotent.

**Continuing a persona.** Opening a persona rebuilds the root view its file describes, without a
model call, so the next job sees exactly what the last one held. The rebuild replays the root records
(bracket records never enter a root view) as the live views held them: a job's system prompt begins a
job and heads its view, and the job-scoped records before it — the previous system prompt, outcome
and interruption markers — are dropped; a compaction summary replaces all but the tail it records it
kept. The view carried in is the earlier exchange and its summaries; the new job's system prompt is
re-read from `agent.md`, so an edit applies to the next job.

**Compaction.** When a view nears `compact_bytes`, older turns are replaced by a summary written by
the reasoning affordance (fixed template: Goal / Constraints / Progress / Key Decisions / Next Steps
/ Critical Context), keeping a recent tail verbatim and cutting only at turn boundaries. The summary
is written after the tail it keeps and records how many records it kept, so the rebuild is exact. The
kernel reports no token usage, so the trigger is a configurable byte budget.

## 8. Evidence and ratings

**Evidence rides with the choice.** Each offered candidate carries the `evidence` block of its
authoritative read (kernel D20). The tool list renders this kernel's own record of the action
(`local_experience`) beside its price, so the model weighs what a person would see. The three
provenances are never summed or scored (kernel U39), so the agent ranks and evicts by nothing but
recency.

**Rating pass.** After a job ends — in the policy layer, never in the loop, mirroring the kernel law
that execution must not rate (kernel G8) — one structured call of the reasoning affordance judges
each call the job dispatched, and the agent posts `rating ∈ {0,1}` plus a one-line note per
transaction (`POST /v1/transactions/{id}/rate`). A kernel-failed call is a mechanical `0`. A judgment
that is not an integral index, a rating of exactly 0 or 1, and a string note is discarded, leaving
its call unrated. The note is public (anyone who can read the action lists its ratings), so a failed
call's note is the fixed `call failed`, never its error text, and the judge is instructed to describe
only the tool's performance, never the request, its content or any principal; the instruction
reduces the risk of disclosure and cannot guarantee it, since the model's compliance is unverified.
Only the job's own records are rated: a persona's earlier jobs were rated once already, and the kernel refuses
a second rating (kernel D11). The agent is the caller of every call it dispatches, so it is the
rightful rater (kernel D11, U14). A task's completion runs as its requester, not as the agent, so the
agent never rates it. Gated by `rate_tasks`.

## 9. Identity and the agent file

An agent has one human-authored file, `agent.md`: its persona, standing instructions and domain
knowledge. It is legible (readable, greppable, diffable), is read at every job start, and is never
written by the agent, so it may be edited at any time and takes effect on the next job. It is the only
state shared by all personas. The agent writes no cross-persona memory: what it learned in one
conversation never reaches another (§10).

`agent.md` is a fixed filename in the **agent home**, `$JUICE_HOME/agents/<login>/` (ecosystem §2,
§7), which also holds `config.json` and `personas/`. `juice-agent serve research@work`
resolves this home, so a differently-behaved agent is a login and a directory, not new code.

**Initialization.** The first time an agent is launched, the daemon creates its home (§11) and writes
`agent.md` as an empty file if absent, so the operator always has a concrete file to edit. Creation is
idempotent — an existing file is never overwritten or truncated. An empty `agent.md` contributes
nothing to the system prompt.

**System prompt.** Before each job the policy layer (not the pure loop) assembles one `system`
message — role line + `agent.md` + the operating contract — and prepends it to the job's messages.
The transcript places it at the head of the persona's view. The loop is unchanged; it receives
messages and never knows a prompt exists.

## 10. Intake and personas

**Tasks only.** Intake polls `GET /v1/tasks?status=waiting` every `poll_seconds` and keeps the tasks
whose `required_caller` is the agent's address (the listing also returns tasks the agent funded). It
reads every page (`limit` 200, the API's ceiling, until a short page): the listing has no
required-caller filter, so one page of tasks the agent funded would otherwise hide every task
addressed to it. The
kernel lists only tasks from requesters the agent's operator approved (kernel U52); approval is the
operator's act (`juice user approve <address>`), never the daemon's, and the daemon applies no other
access rule. There is no other intake and no application-specific code.

**The persona key.** The kernel certifies a task's `requester`, `owner` and `created_by` (API, Tasks)
but not which conversation it belongs to: two tasks from one person in two rooms of one application
have identical certified fields, so no function of those fields alone can separate them. The
conversation therefore comes from the application, in the argument `partial_args.thread_id`, and the
key is computed from the task alone, with no knowledge of the application:

| Case | Condition | Key |
| --- | --- | --- |
| Threaded | `thread_id` is a non-empty string | (requester, created_by, `t:` + thread_id) |
| Direct | no `thread_id`, requester = owner | (requester, created_by, ∅) |
| Relayed | no `thread_id`, requester ≠ owner | (requester, created_by, `task:` + task id) |

The agent is implicit: one home per login. Wrongly merging two conversations discloses one audience's
turns to another, while wrongly separating them only loses continuity; the rule therefore fragments
whenever continuity is not established. A direct sender (requester = owner, as with the `message`
native) has no intermediary, so its tasks are one conversation; a relayed task without `thread_id` is
its own persona. An application that wants continuity supplies `thread_id`; the identifier is
namespaced by requester and creating action, so no application reaches another's persona. An
application can misstate its threads; the agent certifies attribution, not an application's
assertions, and a misbehaving requester is dealt with by revoking its approval (kernel G10).

The persona's file name is each component `url.QueryEscape`d and the three joined by `|`, so the
separator never occurs inside a component and distinct keys never share a file. A name longer than
200 bytes is replaced by its SHA-256 hex, and the key is written as the file's first record.

**Attribution.** Each task becomes one user message, `<owner>: <text>`, or `<owner> via
<requester>: <text>` when they differ: the owner is the payer the kernel certifies, the requester the
principal whose action relayed it. `text` is `partial_args.message`, else `partial_args.request`,
else the whole `partial_args` as JSON. Owner is attribution within a persona, never part of its key.

**One job per persona.** A task for a persona with a job in flight waits for a later poll, and a
persona stays busy until its task's completion is accepted or refused, so a conversation's tasks are
answered in order and never interleave; personas run concurrently on
`concurrency` workers. A task is taken at most once per run; the kernel's task status is the durable
guard, so a task still waiting after a restart is redone and a completed one never returns.

**Completion.** A job's outcome completes its task. The reply tool's `{answer}` is shaped onto the
task's `allowed_input`: an unconstrained onward (a `sink`) receives `{answer}`, which rides in the
completion's arguments readable by the sender; a declared text field (`text`, `message`, `answer`,
`body`, `reply`, `content`, or the only string field) receives the answer; otherwise the reply passes
unchanged and the kernel validates. A failed job still completes, so the sender's process is
released, and it explains itself: the policy layer makes one more call of the reasoning
affordance over the job's root view, asking what the agent attempted, what went wrong and what
would be needed, in plain text. The reply is the outcome's `explanation` (§3), recorded in the
persona's file and delivered as the answer is: a sink receives `{answer, status, reason}`, a
declared text field receives the explanation alone, so a strict onward schema still accepts the
completion. The call is paid from the job's budget like the rating pass (§11); one that does not fit,
or fails, is skipped and the task completes with `{status, reason}` alone. Completion runs as the
requester; the agent pays nothing for it (kernel D6). A completion that fails transiently is resent at every later poll until the kernel
accepts or refuses it, and the job is never run again for it; a refused completion leaves the task
`waiting`, visible to the operator.

**One-shot.** `juice-agent task <login> "<prompt>"` runs one job in the persona `cli` and prints the
outcome as JSON; successive one-shot runs continue that persona.

## 11. Daemon lifecycle and configuration

### Identity and session (ecosystem §5, §6)

1. The agent is named by its **login**, `handle@kernel`, the one positional argument after the verb.
   Both halves are local names (letters, digits, dot, dash, underscore; at most 64 characters; no
   leading dot), the juice CLI's rule. There is no default login and no environment fallback: a
   selector naming no login is an error, never a guess.
2. The daemon holds no secret. It reads the kernel's endpoint and public key from
   `$JUICE_HOME/client/config.json` by the login's kernel half, and its session (access token,
   refresh token, principal id) from `$JUICE_HOME/client/credentials/<login>.json`. A session is
   obtained with `juice auth login <login>`; the daemon never logs in with a password.
3. On a `401` the daemon rotates the session under an exclusive `flock` on the credential file, as
   the CLI does: if the stored token differs from the refused one, another process rotated first and
   its token is adopted; otherwise the stored refresh token is exchanged (`POST /v1/auth/refresh`) and
   the rotated pair written back, keeping every other field the file holds. A stored session whose
   principal id differs from the one the daemon started with is never adopted or refreshed: the
   file name is a label, a later login under it may be another account, and the daemon's home and
   personas belong to the first. The daemon keeps its own session and reports that a restart is needed
   to serve the other account.
4. At startup the daemon fails, naming the command to run, when the kernel is not recorded (`juice
   kernel add <url>`), when the login holds no session or its refresh is refused (`juice auth login
   <login>`), or when `GET /v1/me` answers for another address or principal than the login and its
   credential record name.

### Filesystem layout (ecosystem §2)

```text
$JUICE_HOME/agents/<login>/        one agent, e.g. agents/research@work/
├── agent.md                       human-authored persona and knowledge (§9); never written by the daemon
├── config.json                    written whole at first start (ecosystem §3); then the operator's
├── serve.lock                     held by the serving daemon under an exclusive flock; its pid
└── personas/                      one append-only JSONL per persona (§7, §10)
$JUICE_HOME/schemas/juice-agent.schema.json   config.json's JSON Schema, rewritten by setup
```

1. The root is `$JUICE_HOME` (default `~/.juice`); the home is fixed, never cwd-relative.
2. The daemon `MkdirAll`s the home and its `personas/` (mode `0700`) at startup and writes a blank
   `agent.md` if absent. It never truncates a file but `serve.lock`, and writes `config.json` only
   as the next section says.
3. Files of earlier layouts (`persona.md`, `knowledge.md`, `memory.md`, `data/`) are not read; the
   README states the move.
4. **Intake always runs.** A non-positive `poll_seconds` or `concurrency` falls back to its default.

### Config

`config.json` follows ecosystem §3. Setup writes the file's JSON Schema to
`$JUICE_HOME/schemas/juice-agent.schema.json`, generated from the one default value in the
program: every key described, each stating its default but `reason` and `action_lookup`. Those two
name the login's kernel, which one schema for every agent cannot state, so they have no default and
first start creates them. First start writes the whole file, `$schema` first
(`../../schemas/juice-agent.schema.json`), every other key at its default and the two affordances
as the natives of the login's kernel. A file that lacks `$schema`, `reason` or `action_lookup` — one
written before this rule, by hand or by an interface — gets exactly those keys written in at start,
every other key kept and no default added; the file is otherwise never written. The file is laid
over the defaults: a key it omits keeps its default, and since every key is flat, decoding onto the
defaults is the standard's merge. A key the schema does not name (matched exactly, so `MAX_TURNS` is
not `max_turns`), a `null`, a value of the wrong type, an empty affordance or malformed JSON stops
the start, naming the key, so a retired key (`channels`, `memory_bytes`, `maker`, `max_repeats`,
`candidate_limit`, `lookup_limit`) cannot be silently ignored. **No secrets live in config.** The
only environment input is `JUICE_HOME`.

### Lifecycle

`juice-agent serve <login>` runs the daemon: the verb always comes first, and the login is the
positional after it (`task` and `bench` take it as their first positional, the prompt or suite
second). A bare `juice-agent` prints usage, wrapped to stderr's width up to 80
columns (80 when stderr is not a terminal); each command line stays whole, its description wrapped
below it. Serving resolves the home from the login → creates the
home and blank files if absent → runs setup (writes the schema) and, when absent, `config.json` or its created
keys → loads config → opens the session and verifies the account (above)
→ assembles the executor → takes the home's lock → runs startup recovery over `personas/` (mark interrupted jobs) → starts
intake and the worker pool. Each job settles with the rating pass (§8). `SIGTERM`/`SIGINT` must drain: stop intake, let in-flight turns
finish, persist, exit 0. There is no `juice-agent stop`. Harness tools are not registered on the
platform, so startup performs no bootstrap round-trip for them.

**One daemon per home** (ecosystem §4, applied to an agent's home, §7). `serve` opens
`serve.lock` in the home, takes an exclusive non-blocking `flock`, and writes its process id into
it; the lock is held until the process exits, and the file is never removed. A second `serve` for
the login is refused with "another juice-agent is already serving <login>" before recovery, so it
neither takes the first one's tasks, nor appends to its persona files, nor marks its running jobs
interrupted. The lock, not the number, says whether a daemon is serving: the file outlives the
process, the lock does not, so a program that signals the pid must first fail to take the lock.
`task` takes no lock: it polls no tasks, runs no recovery and writes only the `cli` persona.

**Setup** (ecosystem §3). `juice-agent setup`, absent from usage and taking no argument, writes the
schema and nothing else: juice-agent ships no defaults, and an agent's home and its `config.json`
are first start's. The installer (`make install`) writes `$JUICE_HOME/bin/juice-agent`, where the
family's executables live (ecosystem §2), and then runs `juice-agent setup`, so `schemas/` holds the
installed binary's schema before any agent starts; `serve` and `task` run it at every start.
`make uninstall` removes the binary only.

### Config keys

| Key              | Default                  | Meaning                                                                  |
| ---------------- | ------------------------ | ------------------------------------------------------------------------ |
| `reason`         | none; first start writes `sys@<kernel>/llm/chat` | the reasoning affordance: any action meeting the reason contract         |
| `action_lookup`  | none; first start writes `sys@<kernel>/lookup` | the action-lookup affordance: any action meeting its contract            |
| `default_budget` | `100`                    | base units a job may spend — the primary runaway rail                    |
| `max_turns`      | `32`                     | secondary rail bounding zero-price loops                                 |
| `concurrency`    | `4`                      | worker-pool size: personas served at once                                |
| `poll_seconds`   | `5`                      | interval between task polls                                              |
| `compact_bytes`  | `262144`                 | view byte budget triggering compaction (§7)                              |
| `rate_tasks`     | `true`                   | run the post-job rating pass (§8)                                        |

`<kernel>` is the login's kernel half. These are the only two actions the harness depends on, so they
are the only action addresses in configuration; every other capability is an ordinary action the
model finds through the action lookup. The login, the home and the `agent.md` filename are not config
keys. The loop's tuning — `MaxRepeats` 3 (§6), a working set of 24 candidates and 8 rows per lookup —
shapes the loop rather than the operator's policy and is fixed in code.

### Affordance contracts

Each affordance is filled by whatever action its key names, so each has a contract: the input the
agent sends and the output it reads, in the kernel's canonical schema form (kernel "Contract"; the
manual's "Action schemas"). At startup the daemon reads both actions (`GET /v1/actions?ref=`) and
compares each one's declared schemas with its contract. If an action is missing or falls short, the
daemon stops with `config.json: <key> = <address>: <path>: <what is wrong>`. Each job reads both
again and checks them the same way: their terms (price and `quote_hash`) are the ones every call
of the job is checked against and pinned to, so a change of terms reaches the next job without a
restart, and a call is never made at terms the job did not read. A job whose affordance no longer
meets its contract fails with `affordance: <key> = <address>: …`.

The comparison is of declared compatibility. What the agent sends must be accepted: every sent
property declared, with a type that is or includes the sent one (a nullable `[T, "null"]` accepts
`T`), an `enum` that admits the sent values, no `minLength`, `maxLength`, `pattern`, `minItems` or
`maxItems` (the agent's text and lists are unbounded, and the kernel refuses arguments that break
them), numeric bounds that admit the range the agent sends (`limit` is 1 to 10; `multipleOf` only
1), and no `required` property the agent does not send. `format` is not enforced by the kernel and
is ignored. An
object without declared properties, and an untyped node, accept anything. What the agent reads must
be guaranteed: every read property declared, with exactly the read type, so a nullable declaration is
refused, since the action could return `null`. A replacement may declare more than the contract; the
agent ignores the rest. The check does not prove that every response carries every field, since the
kernel's own schemas leave them optional. The client reports a reasoning reply without
`choices[0].message.content`, or a lookup reply without a `results` list, as `execution_failed`; an
empty `results` list is a search that matched nothing.

`reason` input:

```json
{
  "type": "object",
  "properties": {
    "messages": {
      "type": "array",
      "description": "The conversation so far",
      "items": {
        "type": "object",
        "properties": {
          "role": {"type": "string", "description": "system, user or assistant",
                   "enum": ["system", "user", "assistant"]},
          "content": {"type": "string", "description": "The text of the turn"}
        },
        "required": ["role", "content"],
        "additionalProperties": false
      }
    }
  },
  "required": ["messages"],
  "additionalProperties": false
}
```

`reason` output (the first choice's `message.content` is the reply):

```json
{
  "type": "object",
  "properties": {
    "choices": {
      "type": "array",
      "description": "The model's reply; the first choice is read",
      "items": {
        "type": "object",
        "properties": {
          "message": {
            "type": "object",
            "properties": {"content": {"type": "string", "description": "The reply text"}},
            "additionalProperties": false
          }
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}
```

`action_lookup` input:

```json
{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "What the agent is looking for"},
    "limit": {"type": "integer", "description": "At most this many results",
              "minimum": 1, "maximum": 10}
  },
  "required": ["query"],
  "additionalProperties": false
}
```

`action_lookup` output:

```json
{
  "type": "object",
  "properties": {
    "results": {
      "type": "array",
      "description": "Matching actions, best first",
      "items": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "description": "The action's address, owner@kernel/name"},
          "description": {"type": "string", "description": "What the action does"},
          "input_schema": {"type": "object", "description": "The action's input, as JSON Schema"}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}
```

The `enum` on `role` constrains the replacement only if it declares one: its `enum` must then admit
all three roles. The kernel's own `llm/chat` and `lookup` meet both contracts.

**Safety rails.** Every real tool call is priced and prepaid, so budget bounds the daemon globally
(the primary rail; a rail the kernel makes possible). Only offered candidates run, each gated on the
price it was read at; a moved quote is refused by the pin; value-bearing actions are never offered;
and `spent` counts what the kernel charged. For an action on the agent's own kernel the bound is
exact: its price bounds its charge, and an execution failure charges only the sub-work delivered.
For an action on another kernel it holds in expectation: the charge adds the import fee and, when
the call's lottery ticket wins, the ticket's face value — the buyer kernel's `lottery` setting,
readable only by its operator — whose expected value is the quoted payment (kernel P10). A winning
ticket can therefore carry `spent` past `budget`; no further call is dispatched once it has. The
harness's own calls (each turn's reasoning, the opening lookup, compaction, a failed job's
explanation, rating) are priced like any other: `llm/chat` costs the model's price, which the kernel's operator sets per model, `0`
included (kernel `manual/reference/config.md`), and a replacement affordance costs what its owner
charges. Each is checked against what the job has left before it runs, pinned to the terms the job
read (§11), run under one `external_key` across its retries, and counted in `spent` at its charge,
or at its price when its settlement went unreported. A turn that does not fit fails the job with
`budget`, one whose terms moved with `terms-changed`; an opening lookup, a compaction, an
explanation or a rating that does not fit is skipped. The explanation and the rating pass run before
`spent` is final, and nothing is paid after: `spent` is the job's whole cost. The turn cap is the secondary rail for
zero-price actions. The repeat guard (§6) catches the free-and-succeeding action both miss. Policy-
layer retries (3×, exponential backoff) apply only to transport-level and retryable kernel errors
(network, 5xx, 429) — never to `ErrInsufficientFunds`, schema violations, or aborts, and never to a
`timeout`, which reports a dispatched call the kernel has parked and will settle. Every dispatch
carries its own `external_key`, kept across its retries, so a retry after a lost reply is answered
with the first run's outcome instead of running the action a second time. A failure that reports a
settlement (`tx_id` and `charge`) is not retried, since the call ran, and its charge is the one
counted; after the last retry, the last attempt's settlement, if any, is counted. A
single invalid model selection is fed back for self-correction; the run fails after `MaxDecideErrors`
consecutive. Model-output errors (`execution_failed`, `schema_violation`) are not retried as
transient.

**Logging.** Structured JSON to stderr; stdout is reserved for resource payloads. Fields:
`time level event job persona turn action tx_id status duration_ms error`. The kernel ledger
remains the financial ground truth.

## 12. Testing: three tiers

**Unit (`go test ./...`).** Offline, no kernel, no LLM. `agent` is tested against fake
`Decider`/`Catalog`/`Runner`/`Transcript`: stop rule, budget refusal, turn cap, repeat guard (an
identical re-selection is dropped and hard-stops; the same action over other arguments, a retry
after a failure and after a settled timeout dispatch; a re-selection after a parked call is a
repeat), steering injection, candidate merge (authoritative read, effect exclusion) and recency eviction,
offered-only dispatch, quote pinning and eviction on moved terms, `spent` from the reported charge,
a distinct run key per dispatch, subtask bracket roll-up and shared counters. The product package
covers intake — the persona key in its three cases, namespacing by requester and creating action,
the unambiguous file-name encoding, attribution, reply shaping, one job per persona with personas
concurrent, the required-caller filter, a failed job still completing — with its explanation shaped
onto a sink and onto a declared text field, and without one as `{status, reason}` — and the
completion resend after a transient failure without a second run; persona files — continuation without job-scoped
records, an exact rebuild after compaction across jobs, bracket exclusion and numbering, the hashed
long name with its header, and `All()` holding only the job's records; the executor — `agent.md` in
the system prompt, a second job continuing the first, rating only the job's own calls, and a failed
job's explanation (made once, charged to `spent`, recorded before the outcome; skipped when the
budget cannot pay; never made for an answered job); one run key across a dispatch's retries, a parked `timeout` returned without retry, the usage text's
wrapping and width rule (the terminal-size query passed in, so no terminal is needed), the rating
pass, and the §11 contract: login parsing and home resolution under `JUICE_HOME`, defaults with the
affordances on the login's kernel and per-key overrides, rejection of unknown and retired keys, home
creation at mode 0700 with a blank `agent.md` and no config written, and startup refusal of a
missing session or a mismatched account. `juice` is tested against `httptest` kernels and a temporary
`JUICE_HOME` holding client records, with affordance addresses that are not the kernel's natives:
session rotation on a `401` written back under the lock, adoption of a token another process
rotated, refusal of another principal's session, a revoked refresh as no session, quote pinning and
`terms_changed` recognition, the external key and the reported charge (also from a failure's
`meta`), a `timeout` as not retryable and reported to the loop as such, with its settlement when
it has one, reasoning and structured output through the configured
reasoning affordance, lookup through the configured action-lookup affordance, the authoritative
read, each affordance's contract check (the kernel's natives pass; each shortfall is named by path),
and task listing with `requester` and completion. Every production file has its `_test.go`.

**Flows (`flows/`).** End-to-end shell scripts against the compiled binary, booting a real kernel
(`juice kernel serve`, through the kernel repository's own flow harness) with the **real Ollama the
kernel ships with** (no fake model). Each account is created, funded and logged in with the juice
CLI, and the daemon runs as that login over the CLI's records; the agent's operator approves the
senders a flow uses. Mechanism flows make deterministic assertions: `bootstrap` (no harness tool is
registered; restart), `init` (`setup` writes only the schema into a fresh installation and is
absent from usage; a first launch creates the home with a blank `agent.md` and
`personas/` and none of the retired files, writes `config.json` with every key, `$schema` first and
the kernel's natives as the affordances, and the schema; it serves, never truncates on relaunch,
and a partial `config.json` gets exactly `$schema`, `reason` and `action_lookup` written in, the
operator's value kept), `config` (retired keys refused, naming the file; an affordance of the wrong shape refused, naming the key,
the address and the field), `no_session` (the three
startup refusals), `recovery`, `drain`, `lock` (the lock holds the daemon's pid; a second `serve`
is refused, naming the login, and leaves the pid alone; SIGTERM to that pid drains the daemon; the
lock is free once it exits), `tasks` (a task addressed to the agent is completed, its reply
rides in the completion's arguments, the completion runs as the sender, and a task addressed to
another stays waiting), `budget` (a reasoning action priced at 7 is paid each turn: `spent` is 7 and
the agent's balance falls by 7; its owner then raises the price above the budget while the daemon
runs, and the next job fails with `budget`, not `terms-changed`, paying nothing), `explain` (a
reasoning action that always selects the lookup with the same arguments dispatches it once, is
dropped three times and fails with `repeat-loop`; the completion carries the explanation that
endpoint returns beside the status and reason), `approval` (an unapproved sender's task is withheld and listed pending, and
served once approved), `personas` (one file per sender, the second message in the first's file,
another sender kept apart, attribution, and the log showing that a persona's first job loads nothing
and its next loads the history), `threads` (tasks created by applications through an action's
capability: a relayed task is withheld until the application, not the poster, is approved; posts by
two people and an application's event in one room share a conversation whose jobs run one at a time;
another room, the same room name from another action or another application, a direct message,
another agent, and each reminder without a `thread_id` are each a conversation of their own; turns
are attributed `owner via requester`), `named`, `session_identity` (another account's session stored under
the agent's login is not adopted), and `usage` (fits 80 columns and a 50-column pseudo-terminal).
LLM flows assert robust properties, not exact text, and skip (never fail) when the chat model is
absent: `answer` (an ISO time, spend within budget and equal to the fall in the agent's balance),
`explanation` (a job cut to one turn fails with `turn-cap` and completes with a non-empty plain-text
explanation), `agent_md` (`agent.md` reaches the system prompt), `rating` (the time call is rated, and a second job
in the persona raises no rating error), and `conversation` (a persona recalls a codeword given in its
earlier message, and another sender's persona does not), and `thread_conversation` (a codeword
given in a room is recalled by another person in that room, and not in another room or by another
application naming the same room). The budget and turn-cap rails are code
invariants proven by `go test ./agent`. A parked remote call and a failed completion have no
deterministic trigger against one real kernel, so the external key, the `timeout` rule and the
completion resend are proven by unit tests. Nothing is committed until unit tests and flows pass.

**Experiments (`bench/`) — excluded from `go test ./...`.** `juice-agent bench <login> <suite.json> [N]`
runs a versioned, immutable task suite (id, prompt, budget, machine-checkable success criterion)
against a live kernel + real LLM, N repetitions per task (default 5). Per-task metrics: success rate,
mean credits (read from the ledger), mean latency, mean turns. One JSON report per run — keyed by
agent version (`git describe`), suite version, kernel version, and model config — is written to
`bench/results/`, so competence is a diffable time series.
