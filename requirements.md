# Juice Agent Requirements

Version: 0.3.0
Status: implementation requirements
Codename: `juice-agent`

This document states what the agent daemon **must** be; `agent-design.md` states why. Where this
document and the design differ, this document governs the implementation. It depends on, and never
restates, two documents mirrored from the kernel repository by `copy-juicedocs.sh`: the kernel
contract `juicedocs/requirements.md` and the ecosystem standard `juicedocs/ecosystem-standard.md`.
A requirement here must violate neither. Citations use the kernel's own labels: *kernel §N* for a
section, *kernel D/P/G/U N* for a design decision, protocol clause, guarantee, or user story, and
*ecosystem §N* for the standard.

Revision 0.3.0 resynchronises with the kernel contract 0.15 and adopts the ecosystem standard.
Every name on the wire is an address (`handle@kernel`, `owner@kernel/name`; kernel D15), so the
kernel's natives are reached as `sys@<kernel>/…`. The kernel's suspended call is a *task* (kernel
D6); the agent's own unit of work is therefore a *job*. The stats surface is gone: an action's terms
and its `evidence` (kernel D20, never summed or scored, U39) come from an authoritative read, and
the `quote_hash` from that read pins the terms on dispatch. The agent is named by its login and
holds no secret: it reads the kernel's address and its session from the juice CLI's client records
(ecosystem §4) and lives at `$JUICE_HOME/agents/<login>/` (ecosystem §2, §6).

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
kernel's one signal to an account is a task addressed to it (kernel D6). A task is a single primitive
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
and completion merely acknowledges it, or a task carrying a conversation id and history: in both cases
any reply travels by a separate action, since nothing is left open to carry it.

`juice-agent` is therefore a Go daemon implementing a general task-solving agent whose entire
capability surface is a Juice kernel's action catalog. It is an ordinary kernel client: it
acts as one login, `handle@kernel` (ecosystem §4), and derives every agent affordance from that one
fact — reasoning from the `llm/*` natives, tools from the actions callable under `CanCall` (kernel
D5), dispatch from `POST /v1/run`, budget from its wallet, address from its login, and
accountability from the transaction role law (kernel D4, D6).

The daemon must add no agent-specific API to the kernel and require none. It must hold no model
credentials, no API keys, and no provider adapters; the kernel is its sole external interface. Many
agents are one binary: `juice-agent serve <login>` launches the same binary as a distinct kernel
account rooted in its own home (§9), and agents compose only through the kernel, never through a
private protocol.

## 2. Packages and implementation constraints

Use Go. `go build ./...` and `go test ./...` must pass **offline** — no kernel, no LLM, no network.
The Go packages:

```text
agent/           the loop: pure, interface-driven
juice/           kernel client: the only package that speaks HTTP to the kernel
channels/        the intake/egress framework: Channel interface + the Kernel seam (channel.go),
                 the generic service-channel driver with its loop guard (service.go, loopguard.go);
                 one subpackage per channel — channels/task, channels/http, channels/chat, …
cmd/juice-agent/ package main: everything else (lifecycle, config, home resolution, the one
                 kernelAdapter over the client, the policy shell, the maker, the bench runner)
bench/           experiment-suite data (tasks.json, results/) — no Go
flows/           end-to-end shell flows against the compiled binary — no Go
```

Dependency law, mirroring the kernel's own (kernel D1):

- `agent` must import neither `net/http` nor `juice`. It depends only on the small interfaces it
  defines (`Decider`, `Catalog`, `Runner`, `Transcript`); tests use fakes. This keeps the loop pure.
- The `channels` framework root and every channel subpackage import `agent` (subpackages also import
  `channels`) — never `juice` or `net/http` on the kernel path. A channel reaches the kernel only
  through the `channels.Kernel` interface (list/complete tasks, run actions, grants), implemented once
  by a single `cmd` `kernelAdapter` and injected into every channel — so channels stay kernel-free like
  the loop, and no per-channel kernel code lives outside `cmd`. Notification-driven services share one
  `Service` driver (poll→read→post→complete, at-least-once via a persisted read cursor, a
  per-(session, sender) loop guard); such a channel supplies only a `ServiceActions` adapter
  (read/post/ack + session key + auth), never the delivery logic. A channel claims the agent's tasks by
  their `owner` address; the generic task channel excludes claimed owners.
- `juice` is the only package that speaks HTTP to the kernel.
- `cmd/juice-agent` depends on all of them, wires the client into the channel adapters, and is the sole
  `package main`.

Keep file and package counts small; name files for their function; do not split a file for size
alone. **Every production Go file must have a corresponding `_test.go`** with independent tests for
its logic.

## 3. Data model

The agent adds no durable object to the kernel; its own state is the Job, the Outcome, and the
per-job transcript (§7). IDs are stable opaque strings; budgets and prices are non-negative integers
in the kernel's base units.

| Object    | Fields                                                                        | Rules                                                                                                                                                                                                             |
| --------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Job`     | `id`, `channel`, `messages`, `budget`                                         | One request for the agent: an id, the originating channel (§10), an initial conversation, and a spend ceiling. One job = one loop run = one transcript.                                                            |
| `Outcome` | `status`, `answer`, `reason`, `spent`, `turns`                               | `status ∈ {answered, failed, interrupted}`. `answer` is the reply object (on `answered`). `reason` gives the failure class (`turn-cap`, `repeat-loop`, `decide-error: …`) when not answered. `spent` is what the kernel charged — the sum of each call's reported `charge` (on success, and in a failure's `meta`), `0` for a call refused with `terms_changed`, and the price for a call whose settlement was not reported (a parked call may still draw it); reconstructible from the kernel ledger (the agent is the payer of every one, kernel D4). `turns` is the count of model round-trips. |

A job's outcome must be delivered back through its originating channel. The daemon serves jobs from a
bounded worker pool; each job's loop is serial, jobs are concurrent.

## 4. The loop (`agent/`)

Each turn substitutes the kernel at both ends. In pseudocode — the whole of the pure loop:

```text
run(job):
  cands = merge(Catalog.Lookup(job.prompt, k))              # seed the working set; always ∪ cands is offered
  loop:
    inject queued steering messages                         # polled before every model call
    sel = Decider.Decide(messages, offered, tools)          # ONE llm/chat round-trip, JSON extracted
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

**Turn primitive.** A turn is exactly one `llm/chat` call over the transcript plus a tool list the
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

**Harness tools are not catalog rows.** `reply`, `publish`, and `subtask` are plain-named
affordances the daemon offers and intercepts; they must **not** be registered on the platform, so
the catalog stays a truthful registry of real capabilities. Harness-tool names contain no `/` and
no `@`, so they are unambiguous against action addresses. Each carries a schema only to
standardise the args the model supplies. The loop keeps an intrinsics table keyed by tool name;
adding an agent-native capability is a table entry plus a `Tool` definition, with no platform
registration and no artifact.

**Discovery is in-band.** The lookup native, `sys@<kernel>/lookup` (`Config.LookupAction`), is a
permanent candidate. The model expands its own tool set by selecting it; the harness runs it as an
ordinary call and merges the results into the working set, bounded by `candidate_limit` and evicted
least-recently-merged first. The working set is per-frame and starts fresh each run.

**Offered terms are the dispatched terms.** A lookup row is indicative (kernel D17), so every merged
row is replaced by the action's authoritative read (`GET /v1/actions?ref=`): price, `quote_hash`,
`effect`, and `evidence`. Only an offered candidate may be dispatched — a harness tool, a working-set
member, or an always-offered candidate — and it is dispatched pinned to the `quote_hash` it was read
with, so the kernel refuses it with `terms_changed` before locking funds if the terms moved (kernel
P2). A refused candidate leaves the working set until read again. An action carrying an `effect`
moves the caller's own balance beyond its price (kernel D18), so it is never offered.

**Steering and delegation.** Channels may push mid-job messages; the loop must inject them before
the next model call. A follow-up after completion is a new job that reopens the prior transcript.
Delegation to another agent stays out of the loop: use its published actions or park a task — the
kernel is the orchestrator. In-loop decomposition is the subtask bracket (§5).

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

**Shared bound.** Turns and spend are one `counters{turns, spent}` per job, threaded by pointer
through the recursion, so a subtask tree totals ≤ `max_turns` and ≤ `budget` — never per-frame.
Exhaustion inside a bracket rolls up an error result the parent can act on; at the root it fails the
job.

**Durable but clean.** Every record is teed to the one job transcript, tagged with its bracket id.
`Transcript.All()` (rating and memory) sees bracketed records; the per-frame `Messages()` view (the
model's context) excludes sibling and child brackets, exposing at each node only the root→node path.

## 6. Repeat guard

Budget and the turn cap both fail against a *free, succeeding* action the model re-selects forever
(observed: the same message posted 31× with no `reply`). An exact-content detector is useless — a
sampled model paraphrases its own prior output (measured: 19 posts, 16 distinct strings, 0
byte-identical). The signal must therefore be the **action name**, not its arguments.

- A selection whose action equals the last *dispatched* action is a repeat (arguments ignored). The
  reply tool is exempt.
- A repeat is **neither persisted nor dispatched**: the loop drops it, ensures a single reworded
  challenge nudge sits at the tail (*"you just ran `<action>`, which already completed; select
  `reply` if the task is done"*), and re-queries. Leaving the repeat in context would self-prime an
  autoregressive model; because the repeat never dispatches, the side effect is bounded to the one
  legitimate call.
- Each re-query costs a turn, so `max_turns` bounds the rephrase loop. After `max_repeats`
  consecutive drops (`Config.MaxRepeats`, default 3) the frame hard-stops with `reason = repeat-loop`
  (a bracket rolls up `{error: "repeat-loop"}`). State is per-frame. `max_repeats < 0` disables the
  guard.

A known cost: a legitimate run of the same action over different inputs is nudged and, past the
floor, hard-stopped; exempting specific actions is deferred.

## 7. Persistence, compaction, and within-task memory

**Transcript.** Append-only JSONL, one file per job under the data dir, written as each record is
produced. Aborts and interruptions must be persisted as records, never merely thrown: a job found at
startup whose transcript lacks a terminal record is marked `interrupted` — the agent-side mirror of
the kernel's `reason = interrupted` recovery (kernel D3, G4). Recovery must be idempotent.

**Compaction.** When a transcript nears `compact_bytes`, older turns are replaced by a summary
written by `llm/chat` (fixed template: Goal / Constraints / Progress / Key Decisions / Next
Steps / Critical Context), keeping a recent tail verbatim and cutting only at turn boundaries. The
kernel reports no token usage, so the trigger is a configurable byte budget.

## 8. Evidence, ratings, and self-extension

**Evidence rides with the choice.** Each offered candidate carries the `evidence` block of its
authoritative read (kernel D20). The tool list renders this kernel's own record of the action
(`local_experience`) beside its price, so the model weighs what a person would see. The three
provenances are never summed or scored (kernel U39), so the agent ranks and evicts by nothing but
recency.

**Rating pass.** After a job ends — in the policy layer, never in the loop, mirroring the kernel law
that execution must not rate (kernel G8) — one `llm/json` call judges each dispatched call over the
finished transcript, and the agent posts `rating ∈ {0,1}` plus a one-line note per transaction
(`POST /v1/transactions/{id}/rate`). A kernel-failed call is a mechanical `0`; duplicate ratings are
skipped. The agent pays for every call it makes, so it is the rightful rater (kernel D11). Gated by
`rate_tasks`.

**Self-extension (the maker).** The agent may publish actions. It keeps a legible job index under the
data dir (prompt, outcome, spent). After a job, when ≥`min_repeats` semantically similar prompts
(cosine over `llm/embed`) have recurred within `window`, a synthesis job is enqueued to the agent
itself. Synthesis runs the ordinary loop with the `publish` harness tool, naming the
`sys@<kernel>/tinygo/compile` native, which the model offers itself through lookup. `publish` is a
transactional publish over the action mutation endpoints, targeting the new action by id: create
private → enable → run declared `test_inputs` → make public on pass, or delete and return diagnostics
on fail. Once published, future repetitions find the new action through ordinary lookup. Gated by
`maker.enabled` (default off).

## 9. Identity, persona, knowledge, and learned memory

An agent has two durable memories beyond the per-task transcript, both **legible files** (readable,
greppable, diffable, hand-editable), differing only in authorship:

- **Authored** — `persona.md` (tone, standing instructions) and `knowledge.md` (durable domain
  knowledge, the agent's "skills"). Human-written; the agent only ever reads them, at job start, so
  they may be edited at any time and take effect on the next job.
- **Learned** — `memory.md`, the agent's own running summary of past jobs.

All three are fixed filenames (never operator-configurable) and live in the **agent home**,
`$JUICE_HOME/agents/<login>/` (ecosystem §2, §6), which also holds the optional `config.json` and
`data/`. `juice-agent serve research@work` resolves this home, so a differently-behaved agent is a
login and a directory, not new code.

**Initialization.** The first time an agent is launched, the daemon creates its home (§11) and
**writes `persona.md`, `knowledge.md`, and `memory.md` as empty files** if absent, so the operator
always has concrete files to edit. Creation is idempotent — an existing file is never overwritten or
truncated. An empty authored file contributes nothing to the system prompt (equivalent to absent);
`memory.md` accumulates from empty.

**System prompt.** Before each job the policy layer (not the pure loop) assembles one `system`
message — role line + `persona.md` + `knowledge.md` + `memory.md` digest + the operating contract —
and prepends it. The loop is unchanged; it receives messages and never knows a prompt exists.

**Learned memory.** After a job settles (alongside the rating pass), one `llm/chat` call summarises
what the agent did and learned into a dated `memory.md` entry; the next job loads `memory.md` into
the system prompt. The update is best-effort and must never fail the job. `memory.md` is bounded by
`memory_bytes`; over budget, the oldest entries fold into a digest. `memory_bytes = 0` reverts to the
strict stance (no automatic cross-job memory).

**Identity of memory.** Each entry's heading, digests included, carries the identity it was learned
under: the kernel's public key and the account's principal id, both read from the client records
(ecosystem §4: memory about a kernel is keyed by kernel key and principal, never by a name). Only
entries carrying the current identity are loaded into the prompt or folded by compaction; others are
omitted (logged once) and left verbatim, so a stale memory is detectable rather than silently wrong.
Text before the first heading is the operator's and always loaded. Because the
agent also writes `memory.md`, hand-edits made while the daemon is running are last-writer-wins and
may later be folded into a compaction digest; edit `memory.md` with the agent stopped for a durable
change. `persona.md` and `knowledge.md` are never written by the agent, so they are safe to edit at
any time.

## 10. Channels

Intake is a replaceable boundary. A channel converts an external conversation into Jobs and delivers
Outcomes (and optional steering) back:

```text
Channel { Start(ctx, submit func(Job)) error;  Deliver(jobID, Outcome);  Steering(jobID) []Message }
```

The loop must never know which channel a job came from. Only enabled channels run; the choice is
configuration, not architecture. Implementations:

- `task` — kernel-native and always on: poll `GET /v1/tasks?status=waiting` and keep the tasks whose
  `required_caller` is the agent's address (the listing also returns tasks the agent funded); the
  task's `partial_args` carry the request; completing the task delivers the outcome under the role
  law (kernel D6). A completion that fails transiently is resent at every later poll until the kernel
  accepts or refuses it, and the job is never run again for it; a refused completion leaves the task
  `waiting`, visible to the operator. This makes the `message` native and any other agent an input with zero pairwise
  integration.
- `service` — a dedicated notification-driven service (e.g. juice-ui `chat`, the account
  `chat@<kernel>`): the generic `Service` driver polls the service's notification tasks, reads each
  conversation's new messages, runs the loop, posts the reply, and completes the task (at-least-once
  via a persisted read cursor), with a
  per-(session, sender) loop guard. A channel supplies only its read/post/ack actions, session key, and
  grant recipe (the cmd adapter); the delivery logic is shared. Parties are addresses: the agent
  registers and is recognised as its login, and the grant selector is `chat@<kernel>/msg`. For
  continuity, the driver keeps a
  bounded per-session **conversation window** and seeds a job with the last `context_messages` visible
  turns (the incoming messages and the agent's posted answers only — no tool traces) ahead of the new
  message; the read cursor stays the dedup watermark, separate from context. The guard is configured
  per channel under `channels.<name>.loop`: more than `max_events` replies to one counterpart within
  `window_seconds` trips a `cooldown_seconds` pause; omitted numbers default to `20 / 60s / 60s`.
- `http` — Juice-independent: a local HTTP endpoint accepting tasks directly, enabled only when
  `channels.http.addr` is non-empty.
- `cli` — a one-shot dev surface (`juice-agent task <login> "<prompt>"`), needing no channel; it runs
  one job and prints the outcome as JSON.

**Adding a channel.** Create a subpackage `channels/<name>/` (importing `agent` + `channels`, never
`juice`). For a notification-driven service, implement `channels.ServiceActions` (list tasks, read,
post, complete, session key, transient classification) over the injected `channels.Kernel`, put any
auth (grant) recipe there, and return `channels.NewService(...)` to reuse the shared driver — the
delivery loop, cursor, and loop guard come for free. A simple message-in-task bridge (SMS/Slack) needs
no new package: the `task` channel already carries it. Then add one case to `buildChannels` in `cmd`
and a config block. No kernel code is written outside the single `cmd` `kernelAdapter`.

## 11. Daemon lifecycle and configuration

### Identity and session (ecosystem §4, §5)

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
   memory belong to the first. The daemon keeps its own session and reports that a restart is needed
   to serve the other account.
4. At startup the daemon fails, naming the command to run, when the kernel is not recorded (`juice
   kernel add <url>`), when the login holds no session or its refresh is refused (`juice auth login
   <login>`), or when `GET /v1/me` answers for another address or principal than the login and its
   credential record name.

### Filesystem layout (ecosystem §2)

```text
$JUICE_HOME/agents/<login>/        one agent, e.g. agents/research@work/
├── config.json                    optional; never written by the daemon
├── persona.md  knowledge.md  memory.md    hand-authored + learned files (§9)
└── data/                          transcripts (one JSONL per job), the maker index, channel state
```

1. The root is `$JUICE_HOME` (default `~/.juice`); the home is fixed, never cwd-relative.
2. The daemon `MkdirAll`s the home and its `data/` (mode `0700`) at startup and writes blank
   `persona.md`/`knowledge.md`/`memory.md` if absent. It never truncates a file and never writes
   `config.json`: a written file would freeze the defaults of the version that wrote it.
3. Homes under the retired `$JUICE_HOME/agent/agents/<handle>/` are not read; the README states the
   move.
4. **Every agent always has a task channel.** If config yields none (or a non-positive poll
   interval), a default is injected, so a daemon always polls for work.

### Config

`config.json` is optional and every key is optional; a key it omits keeps its default. An unknown
key or malformed JSON is an error at startup, so a retired key (`server_url`, `memory`,
`channels.step`) cannot be silently ignored. **No secrets live in config.** The only environment
input is `JUICE_HOME`.

### Lifecycle

`juice-agent serve <login>` runs the daemon: the verb always comes first, and the login is the
positional after it (`task` and `bench` take it as their first positional, the prompt or suite
second). A bare `juice-agent` prints usage. Serving resolves the home from the login → creates the
home and blank files if absent → loads config → opens the session and verifies the account (above)
→ assembles the executor and the system prompt → runs startup recovery (mark interrupted jobs) →
starts the enabled channels and the worker pool. Each job settles with the rating pass (§8) and a
best-effort memory update (§9). `SIGTERM`/`SIGINT` must drain: stop intake, let in-flight turns
finish, persist, exit 0. There is no `juice-agent stop`. Harness tools are not registered on the
platform, so startup performs no bootstrap round-trip for them.

The installer (`make install`) writes only `$JUICE_HOME/bin/juice-agent`, where the family's
executables live (ecosystem §2); `make uninstall` removes the binary only.

### Config keys

| Key                | Default                | Meaning                                                                       |
| ------------------ | ---------------------- | ----------------------------------------------------------------------------- |
| `memory_bytes`     | `65536`                | byte bound on `memory.md`; `0` turns learned memory off                        |
| `default_budget`   | `100`                  | base units a job may spend when its channel sets none — the primary runaway rail |
| `max_turns`        | `32`                   | secondary rail bounding zero-price loops                                       |
| `max_repeats`      | `3`                    | consecutive repeats before hard-stop (§6); `< 0` disables                      |
| `concurrency`      | `4`                    | worker-pool size                                                              |
| `candidate_limit`  | `24`                   | working-set cap (least recently merged evicted)                               |
| `lookup_limit`     | `8`                    | results per lookup                                                             |
| `compact_bytes`    | `262144`               | transcript byte budget triggering compaction (§7)                             |
| `rate_tasks`       | `true`                 | run the post-job rating pass (§8)                                             |
| `maker`            | `{enabled:false, min_repeats:3, window:50, budget:40, threshold:0.85}` | self-extension trigger (§8) |
| `channels`         | task always on         | `task {poll_seconds}` (always present), optional `http {addr}`, and optional `chat {enabled, poll_seconds, context_messages, loop{enabled, max_events, window_seconds, cooldown_seconds}}` (the juice-ui chat channel) |

The login, `data_dir`, and the persona/knowledge/memory filenames are not config keys: the login
names the home, `data_dir` is `<home>/data`, and the filenames are fixed.

**Safety rails.** Every real tool call is priced and prepaid, so budget bounds the daemon globally
(the primary rail; a rail the kernel makes possible). Only offered candidates run, each gated on the
price it was read at; a moved quote is refused by the pin; value-bearing actions are never offered;
and `spent` counts what the kernel charged. For an action on the agent's own kernel the bound is
exact: its price bounds its charge, and an execution failure charges only the sub-work delivered.
For an action on another kernel it holds in expectation: the charge adds the import fee and, when
the call's lottery ticket wins, the ticket's face value — the buyer kernel's `lottery` setting,
readable only by its operator — whose expected value is the quoted payment (kernel P10). A winning
ticket can therefore carry `spent` past `budget`; no further call is dispatched once it has. The policy layer's own calls (the decide turn, rating, memory, embedding) run at the kernel's
native prices, `0` for a local endpoint by kernel policy, and are bounded by the turn cap rather than
counted in `spent`. The turn cap is the secondary rail for
zero-price actions. The repeat guard (§6) catches the free-and-succeeding action both miss. Policy-
layer retries (3×, exponential backoff) apply only to transport-level and retryable kernel errors
(network, 5xx, 429) — never to `ErrInsufficientFunds`, schema violations, or aborts, and never to a
`timeout`, which reports a dispatched call the kernel has parked and will settle. Every dispatch
carries its own `external_key`, kept across its retries, so a retry after a lost reply is answered
with the first run's outcome instead of running the action a second time. A
single invalid model selection is fed back for self-correction; the run fails after `MaxDecideErrors`
consecutive. Model-output errors (`execution_failed`, `schema_violation`) are not retried as
transient.

**Logging.** Structured JSON to stderr; stdout is reserved for resource payloads. Fields:
`time level event job channel turn action tx_id status duration_ms error`. The kernel ledger
remains the financial ground truth.

## 12. Testing: three tiers

**Unit (`go test ./...`).** Offline, no kernel, no LLM. `agent` is tested against fake
`Decider`/`Catalog`/`Runner`/`Transcript`: stop rule, budget refusal, turn cap, repeat guard,
steering injection, candidate merge (authoritative read, effect exclusion) and recency eviction,
offered-only dispatch, quote pinning and eviction on moved terms, `spent` from the reported charge,
a distinct run key per dispatch, subtask bracket roll-up and shared counters, compaction trigger,
recovery idempotence. The channels cover the task channel's completion resend after a transient
failure and its single attempt after a refusal. The product package additionally covers one run
key across a dispatch's retries, a parked `timeout` returned without retry,
system-prompt assembly, the byte-bounded memory update with its identity filter, the rating pass,
and the maker trigger, plus the §11 contract: login parsing and home resolution under `JUICE_HOME`,
defaults with per-key overrides, rejection of unknown keys, the guaranteed task channel, home
creation at mode 0700 with blank files and no config written, and startup refusal of a missing
session or a mismatched account. `juice` is tested against `httptest` kernels and a temporary
`JUICE_HOME` holding client records: session rotation on a `401` written back under the lock,
adoption of a token another process rotated, refusal of another principal's session, a revoked
refresh as no session, quote pinning and `terms_changed` recognition, the external key and the
reported charge (also from a failure's `meta`), a `timeout` as not retryable, the `llm/*` request and reply shapes, the authoritative read, the
action mutations by target id, and task listing and completion. Every production file has its
`_test.go`.

**Flows (`flows/`).** End-to-end shell scripts against the compiled binary, booting a real kernel
(`juice kernel serve`, through the kernel repository's own flow harness) with the **real Ollama the
kernel ships with** (no fake model). Each account is created, funded and logged in with the juice
CLI, and the daemon runs as that login over the CLI's records. Mechanism flows (`bootstrap`, `init`,
`no_session`, `recovery`, `drain`, `task_channel`, `named`, `session_identity`) touch no model and are deterministic —
`init` asserts that a first launch creates `$JUICE_HOME/agents/<login>/` with blank
`persona.md`/`knowledge.md`/`memory.md`, writes no `config.json`, still runs a task channel, and
leaves pre-existing files untouched on relaunch; `no_session` asserts the three startup refusals;
`named` asserts two agents serve as distinct logins; `session_identity` stores another account's
session under the agent's login, forces a re-read by suspending the agent, and asserts the daemon
still completes the agent's tasks as the agent. LLM flows (`task`, `persona`, `rating`,
`role_law`, `memory`, `maker`) assert robust structural properties, not exact model text, and skip
(never fail) when the chat model is absent. The budget and turn-cap rails
are code invariants proven by `go test ./agent`; the flows assert the surviving invariants (spend
never exceeds budget, and equals the fall in the agent's balance). A parked remote call and a failed
completion have no deterministic trigger against one real kernel, so the external key, the `timeout`
rule and the completion resend are proven by unit tests. Nothing is committed until unit tests and flows pass.

**Experiments (`bench/`) — excluded from `go test ./...`.** `juice-agent bench <login> <suite.json> [N]`
runs a versioned, immutable task suite (id, prompt, budget, machine-checkable success criterion)
against a live kernel + real LLM, N repetitions per task (default 5). Per-task metrics: success rate,
mean credits (read from the ledger), mean latency, mean turns. One JSON report per run — keyed by
agent version (`git describe`), suite version, kernel version, and model config — is written to
`bench/results/`, so competence is a diffable time series.
