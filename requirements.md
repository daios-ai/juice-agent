# Juice Agent Requirements

Version: 0.1.3
Status: implementation requirements
Codename: `juice-agent`

This document states what the agent daemon **must** be; `agent-design.md` states why. Where this
document and the design differ, this document governs the implementation. It depends on, and never
restates, the kernel contract in `juicedocs/requirements.md` (cited as *kernel §N*); a requirement
here must not violate one there.

Revision 0.1.3 adopts the juice-family filesystem convention (§11): a fixed `$JUICE_HOME/agent/<handle>/`
home per agent (no cwd-relative or `named`/unnamed split), layered `config.json` in place of
`agent.json`, a password precedence chain ending in an interactive no-echo prompt
(`golang.org/x/term`), `serve` as the default action on a handle, a clean cut-over from the legacy
layout, and mandatory creation of blank `persona.md`/`knowledge.md`/`memory.md` at agent
initialization (§9). Token persistence is a deferred future item.

## 1. Purpose and position

`juice-agent` is a Go daemon implementing a general task-solving agent whose entire capability
surface is a Juice kernel's action catalog. It is an ordinary kernel client: it authenticates as a
registered user (its *handle*, e.g. `@agent`) and derives every agent affordance from that one fact —
reasoning from `@sys/llm/*`, tools from the actions callable under `CanCall` (kernel §4), dispatch
from `POST /v1/run`, budget from its wallet, address from its handle, and accountability from the
transaction role law (kernel §1).

The daemon must add no agent-specific API to the kernel and require none. It must hold no model
credentials, no API keys, and no provider adapters; the kernel is its sole external interface. Many
named agents are one binary: `juice-agent @handle serve` launches the same binary as a distinct
kernel user rooted in its own home (§9), and agents compose only through the kernel, never through a
private protocol.

## 2. Packages and implementation constraints

Use Go. `go build ./...` and `go test ./...` must pass **offline** — no kernel, no LLM, no network.
Only two seams are load-bearing, so there are exactly three Go packages:

```text
agent/           the loop: pure, interface-driven
juice/           kernel client: the only package that speaks HTTP to the kernel
cmd/juice-agent/ package main: everything else (lifecycle, config, home resolution, channels,
                 the policy shell, the maker, the bench runner)
bench/           experiment-suite data (tasks.json, results/) — no Go
flows/           end-to-end shell flows against the compiled binary — no Go
```

Dependency law, mirroring the kernel's own (kernel §2):

- `agent` must import neither `net/http` nor `juice`. It depends only on the small interfaces it
  defines (`Decider`, `Catalog`, `Runner`, `Transcript`); tests use fakes. This keeps the loop pure.
- `juice` is the only package that speaks HTTP to the kernel.
- `cmd/juice-agent` depends on both and is the sole `package main`.

Keep file and package counts small; name files for their function; do not split a file for size
alone. **Every production Go file must have a corresponding `_test.go`** with independent tests for
its logic.

## 3. Data model

The agent adds no durable object to the kernel; its own state is the Task, the Outcome, and the
per-task transcript (§7). IDs are stable opaque strings; budgets and prices are non-negative integers.

| Object    | Fields                                                                        | Rules                                                                                                                                                                                                             |
| --------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Task`    | `id`, `channel`, `messages`, `budget`                                         | One request for the agent: an id, the originating channel (§8), an initial conversation, and a spend ceiling in credits. One task = one loop run = one transcript.                                                |
| `Outcome` | `status`, `answer`, `reason`, `spent`, `turns`                               | `status ∈ {answered, failed, interrupted}`. `answer` is the reply object (on `answered`). `reason` gives the failure class (`turn-cap`, `repeat-loop`, `decide-error: …`) when not answered. `spent` is exact — the sum of the prices of the calls made, reconstructible from the kernel ledger (the agent is `owner_user_id` of every one, kernel §1). `turns` is the count of model round-trips. |

A task's outcome must be delivered back through its originating channel. The daemon serves tasks from
a bounded worker pool; each task's loop is serial, tasks are concurrent.

## 4. The loop (`agent/`)

Each turn substitutes the kernel at both ends. In pseudocode — the whole of the pure loop:

```text
run(task):
  cands = always ∪ Catalog.Lookup(task.prompt, k)          # seed the working set
  loop:
    inject queued steering messages                         # polled before every model call
    sel = Decider.Decide(messages, candidates, tools)       # ONE @sys/llm/chat round-trip, JSON extracted
    if sel is error:  fail after MaxDecideErrors consecutive # policy layer retries transport errors only
    if sel.action == ReplyTool:   return answered(sel.args)  # THE STOP RULE — nothing dispatched
    if repeat-guard trips (§6):   drop, nudge, re-query
    if spent + price(sel.action) > task.budget:  notify; continue   # the model is told, not killed
    append sel; res = Runner.Run(sel.action, sel.args); spent += price
    append res
    if sel.action == LookupAction: merge res into cands (bounded, recency-evicted)
    if turns == MaxTurns: fail(turn-cap)
```

**Turn primitive.** A turn is exactly one `@sys/llm/chat` call over the transcript plus a tool list
the client builds itself: the catalog candidates (each rendered with description, price, stats, and a
compact schema, §8) followed by the plain-named harness tools. The model must reply with a single
JSON object `{action, args}`, extracted client-side by a balanced-brace parser. The loop must **not**
use `@sys/llm/decide`: it accepts only `@owner/name` references (so plain-named harness tools could
not be candidates) and its grammar-constrained output corrupts copied strings on Ollama (measured).
Validation splits by tool kind: harness-tool args get a required-keys check in-harness; catalog args
are validated kernel-side at dispatch (`POST /v1/run`), and a schema violation is fed back to the
model.

**Stop rule.** The run ends exactly when the model selects the reply tool (`Config.ReplyTool`,
`"reply"`). Termination is decided by content — a selection like any other — never by a provider stop
reason and never by a heuristic over free text. At the root this answers the task; inside a bracket
it pops the frame (§5). The reply tool is never dispatched and nothing is registered for it.

**Harness tools are not catalog rows.** `reply`, `publish`, and `subtask` are plain-named
affordances the daemon offers and intercepts; they must **not** be registered on the platform, so
the catalog stays a truthful registry of real capabilities. Harness-tool names contain no `/`, so
they are unambiguous against `@owner/name` action references. Each carries a schema only to
standardise the args the model supplies. The loop keeps an intrinsics table keyed by tool name;
adding an agent-native capability is a table entry plus a `Tool` definition, with no platform
registration and no artifact.

**Discovery is in-band.** `@sys/lookup` (`Config.LookupAction`) is a permanent candidate. The model
expands its own tool set by selecting it; the harness runs it as an ordinary call and merges the
results into the working set, bounded by `candidate_limit` and evicted by value (§8). The working set
is per-task and starts fresh each run.

**Steering and delegation.** Channels may push mid-task messages; the loop must inject them before
the next model call. A follow-up after completion is a new task that reopens the prior transcript.
Delegation to another agent stays out of the loop: use its published actions or park a step — the
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

**Shared bound.** Turns and credits are one `counters{turns, spent}` per task, threaded by pointer
through the recursion, so a subtask tree totals ≤ `max_turns` and ≤ `budget` — never per-frame.
Exhaustion inside a bracket rolls up an error result the parent can act on; at the root it fails the
task.

**Durable but clean.** Every record is teed to the one task transcript, tagged with its bracket id.
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

**Transcript.** Append-only JSONL, one file per task under the data dir, written as each record is
produced. Aborts and interruptions must be persisted as records, never merely thrown: a task found at
startup whose transcript lacks a terminal record is marked `interrupted` — the agent-side mirror of
the kernel's `reason = interrupted` recovery (kernel §5). Recovery must be idempotent.

**Compaction.** When a transcript nears `compact_bytes`, older turns are replaced by a summary
written by `@sys/llm/chat` (fixed template: Goal / Constraints / Progress / Key Decisions / Next
Steps / Critical Context), keeping a recent tail verbatim and cutting only at turn boundaries. The
kernel reports no token usage, so the trigger is a configurable byte budget.

## 8. Stats, ratings, and self-extension

**Stats-informed selection.** On every candidate merge the harness fetches each candidate's price and
`Stats` (kernel §9) and renders them inline in the tool list built each turn, so track record rides
with the choice. When the working set exceeds `candidate_limit`, the lowest-value candidates are
evicted (Laplace-smoothed success rate + rating prior; unknown ≈ neutral).

**Rating pass.** After a task ends — in the policy layer, never in the loop, mirroring the kernel law
that execution must not rate (kernel §1) — one `@sys/llm/json` call judges each dispatched call over
the finished transcript, and the agent posts `rating ∈ {0,1}` plus a one-line note per transaction
(`POST /v1/transactions/{id}/rate`). A kernel-failed call is a mechanical `0`; duplicate ratings are
skipped. The agent is `owner_user_id` of every call it makes, so it is the rightful rater (kernel
§11). Gated by `rate_tasks`.

**Self-extension (the maker).** The agent may publish actions. It keeps a legible task index under
the data dir (prompt, outcome, spent, actions used). After a task, when ≥`min_repeats` semantically
similar prompts (cosine over `@sys/llm/embed`) have recurred within `window` **and** no catalog
action covers them well (a `@sys/lookup` score below `threshold`), a synthesis task is enqueued to
the agent itself. Synthesis runs the ordinary loop with `@sys/tinygo/compile` and the `publish`
harness tool available. `publish` is a transactional publish: create private → enable → run declared
`test_inputs` → make public on pass, or delete and return diagnostics on fail. Once published,
future repetitions find the new action through ordinary lookup. Gated by `maker.enabled` (default
off).

## 9. Identity, persona, knowledge, and learned memory

An agent has two durable memories beyond the per-task transcript, both **legible files** (readable,
greppable, diffable, hand-editable), differing only in authorship:

- **Authored** — `persona.md` (tone, standing instructions) and `knowledge.md` (durable domain
  knowledge, the agent's "skills"). Human-written; the agent only ever reads them, at task start, so
  they may be edited at any time and take effect on the next task.
- **Learned** — `memory.md`, the agent's own running summary of past tasks.

All three are fixed filenames (never operator-configurable, per the family convention, §11) and live
in the **agent home**, `$JUICE_HOME/agent/<handle>/`, which also holds `config.json` and `data/`.
`juice-agent @research serve` resolves this home, so a differently-behaved agent is a directory, not
new code.

**Initialization.** The first time an agent is launched, the daemon creates its home (§11) and
**writes `persona.md`, `knowledge.md`, and `memory.md` as empty files** if absent, so the operator
always has concrete files to edit. Creation is idempotent — an existing file is never overwritten or
truncated. An empty authored file contributes nothing to the system prompt (equivalent to absent);
`memory.md` accumulates from empty.

**System prompt.** Before each task the policy layer (not the pure loop) assembles one `system`
message — role line + `persona.md` + `knowledge.md` + `memory.md` digest + the operating contract —
and prepends it. The loop is unchanged; it receives messages and never knows a prompt exists.

**Learned memory.** After a task settles (alongside the rating pass), one `@sys/llm/chat` call
summarises what the agent did and learned into a dated `memory.md` entry; the next task loads
`memory.md` into the system prompt. The update is best-effort and must never fail the task.
`memory.md` is bounded by `memory_bytes`; over budget, the oldest entries fold into a digest.
`memory.enabled = false` reverts to the strict stance (no automatic cross-task memory). Because the
agent also writes `memory.md`, hand-edits made while the daemon is running are last-writer-wins and
may later be folded into a compaction digest; edit `memory.md` with the agent stopped for a durable
change. `persona.md` and `knowledge.md` are never written by the agent, so they are safe to edit at
any time.

## 10. Channels

Intake is a replaceable boundary. A channel converts an external conversation into Tasks and delivers
Outcomes (and optional steering) back:

```text
Channel { Start(ctx, submit func(Task)) error;  Deliver(taskID, Outcome);  Steer(taskID) []Message }
```

The loop must never know which channel a task came from. Only enabled channels run; the choice is
configuration, not architecture. Implementations:

- `step` — kernel-native: poll `GET /v1/steps?status=waiting` for steps addressed to the agent
  (`required_caller = @handle`); the step's `partial_args` carry the request; completing the step
  delivers the outcome under the role law (kernel §10). This makes `@sys/message` and any other agent
  an input with zero pairwise integration.
- `http` — Juice-independent: a local HTTP endpoint accepting tasks directly, enabled only when
  `channels.http.addr` is non-empty.
- `cli` — a one-shot dev surface (`juice-agent task "<prompt>"`), needing no channel; it runs one
  task and prints the outcome as JSON.

## 11. Daemon lifecycle and configuration

### Filesystem layout (juice-family convention)

The daemon conforms to the juice-family filesystem convention: **one root, one subdirectory per
component.** All state lives under `$JUICE_HOME` (env var, default `~/.juice`, identical on macOS),
in the component subdirectory `agent/`. juice-agent is the family's first *plural* component — one
binary hosting many named agents — so each agent nests one level deeper, under its handle:

```text
$JUICE_HOME/agent/                 the component root
├── config.json                    OPTIONAL host-wide defaults shared by every agent
├── research/                      one agent — its handle is the directory name
│   ├── config.json                this agent's config (overrides the shared defaults)
│   ├── persona.md  knowledge.md  memory.md    hand-authored + learned files (§9)
│   └── data/                      transcripts (one JSONL per task) + the maker index
└── triage/  …
```

(A `tokens/` credential store is a deferred future item — see the password section below.)

Requirements, mirroring the convention:

1. The root is `$JUICE_HOME` (default `~/.juice`); all state is under `$JUICE_HOME/agent/`.
2. **Fixed default, never cwd-relative** — the agent attaches to the same state wherever it is
   launched from. There is no `named`/unnamed distinction and no cwd fallback: the default handle
   `@agent` lives at `$JUICE_HOME/agent/agent/` like every other agent.
3. The daemon `MkdirAll`s the agent home (mode `0700`) at startup, before any write, and creates the
   blank authored/learned files (§9).
4. Filenames are uniform by role, namespaced by the subdirectory: `config.json` (operator,
   hand-written), the fixed `persona.md`/`knowledge.md`/`memory.md`, `data/`. No component-prefixed
   name such as `agent.json` is used.
5. Migration is a **clean cut-over**: the daemon reads only `$JUICE_HOME` and does **not** read the
   retired cwd-relative layout. If a legacy `./agent.json`, `./agent-data/`, or the retired
   `AGENT_HOME`/`AGENT_CONFIG` env var is detected, a one-line notice is logged telling the operator
   to move state under `$JUICE_HOME/agent/<handle>/`; the legacy inputs are otherwise ignored (never
   read, never auto-moved).

### Handle, config, and secrets

The handle is the CLI `@handle` arg, else `AGENT_HANDLE`, else the default `@agent` — it is the home
directory's name, not a config key. Config is layered: the component-level
`$JUICE_HOME/agent/config.json` supplies host-wide defaults (e.g. `server_url`), and the per-agent
`$JUICE_HOME/agent/<handle>/config.json` overrides them key-by-key (a merge, not a wholesale replace)
— a self-contained agent is one directory, and shared keys are written once. Both files use flat
keys, safe defaults, and are rejected if malformed at startup. **No secrets live in config.**

The kernel password is resolved by a precedence chain, from non-interactive to interactive — a bare
`--password <value>` flag is **not** offered (it would leak via shell history and
`/proc/<pid>/cmdline`). The interactive prompt uses `golang.org/x/term` for no-echo input (the
project's single third-party dependency):

1. `--password-stdin` — read one line from stdin (for pipes/scripts),
2. `AGENT_PASSWORD` — environment (for daemons/systemd),
3. `AGENT_PASSWORD_FILE` — a mode-0600 file,
4. **interactive no-echo TTY prompt** — used only when none of the above is set and stdin is a
   terminal; a `serve` launched without a TTY (systemd) therefore requires 1–3.

**Token persistence is deferred.** A running daemon already re-authenticates itself on token expiry
from the in-memory password (the client renews the bearer token on any `401`), so password-at-start
suffices for the process lifetime; it is re-supplied only on restart, for which the non-interactive
sources cover unattended operation. A `tokens/` store that would let a manual restart skip the prompt
is a documented future item (it requires kernel-issued refresh tokens the client does not yet consume)
and is not built.

### Lifecycle

`juice-agent @handle` runs the daemon: `serve` is the default action when a handle is given with no
subcommand (so `juice-agent @research` serves), `serve`/`task`/`bench`/`version` remain explicit, and
a bare `juice-agent` with no handle and no command prints usage. Serving resolves the agent home from
the handle → creates the home and blank files if absent → loads layered config → authenticates
(password by the chain above; re-login on revocation) → assembles the executor and the system prompt →
runs startup recovery (mark interrupted tasks) → starts the enabled channels and the worker pool.
Each task settles with the rating pass (§8) and a best-effort memory update (§9). `SIGTERM`/`SIGINT`
must drain: stop intake, let in-flight turns finish, persist, exit 0. There is no `juice-agent stop`.
Harness tools are not registered on the platform, so startup performs no bootstrap round-trip for
them.

The installer (`make install`) touches only `$PREFIX/bin` (`PREFIX ?= ~/.local`); `make uninstall`
removes the binary only and never `$JUICE_HOME`.

### Config keys

| Key                | Default                | Scope | Meaning                                                                       |
| ------------------ | ---------------------- | ----- | ----------------------------------------------------------------------------- |
| `server_url`       | `http://localhost:4040`| host  | kernel base URL (typically set once in the component `config.json`)           |
| `memory.enabled`   | `true`                 | agent | learned cross-task memory on/off                                              |
| `memory.memory_bytes` | `65536`             | agent | byte bound on `memory.md`                                                      |
| `default_budget`   | `100`                  | agent | credits a task may spend when its channel sets none — the primary runaway rail |
| `max_turns`        | `32`                   | agent | secondary rail bounding zero-price loops                                       |
| `max_repeats`      | `3`                    | agent | consecutive repeats before hard-stop (§6); `< 0` disables                      |
| `concurrency`      | `4`                    | agent | worker-pool size                                                              |
| `candidate_limit`  | `24`                   | agent | working-set cap (value-evicted)                                              |
| `lookup_limit`     | `8`                    | agent | results per `@sys/lookup`                                                     |
| `compact_bytes`    | `262144`               | agent | transcript byte budget triggering compaction (§7)                             |
| `rate_tasks`       | `true`                 | agent | run the post-task rating pass (§8)                                            |
| `maker`            | `{enabled:false, min_repeats:3, window:50, budget:40, threshold:0.85}` | agent | self-extension trigger (§8)       |
| `channels`         | —                      | agent | `step {poll_seconds}` and/or `http {addr}`; only listed channels run          |

`handle`, `data_dir`, and the persona/knowledge/memory filenames are not config keys: the handle is
the home directory's name (CLI arg / `AGENT_HANDLE`), `data_dir` is derived as `<home>/data`, and the
filenames are fixed by the convention. `JUICE_HOME`, `AGENT_HANDLE`, and the password variables
(`AGENT_PASSWORD`, `AGENT_PASSWORD_FILE`) are environment inputs, not config keys; `AGENT_HOME` and
`AGENT_CONFIG` are retired.

**Safety rails.** Every real tool call is priced and prepaid, so budget bounds the daemon globally
(the primary rail; a rail the kernel makes possible). The turn cap is the secondary rail for
zero-price actions. The repeat guard (§6) catches the free-and-succeeding action both miss. Policy-
layer retries (3×, exponential backoff) apply only to transport-level and retryable kernel errors
(network, 5xx, 429, `ErrTimeout`) — never to `ErrInsufficientFunds`, schema violations, or aborts. A
single invalid model selection is fed back for self-correction; the run fails after `MaxDecideErrors`
consecutive. Model-output errors (`execution_failed`, `schema_violation`) are not retried as
transient.

**Logging.** Structured JSON to stderr; stdout is reserved for resource payloads. Fields:
`time level event task_id channel turn action tx_id status duration_ms error`. The kernel ledger
remains the financial ground truth.

## 12. Testing: three tiers

**Unit (`go test ./...`).** Offline, no kernel, no LLM. `agent` is tested against fake
`Decider`/`Catalog`/`Runner`/`Transcript`: stop rule, budget refusal, turn cap, repeat guard,
steering injection, candidate merge and eviction, subtask bracket roll-up and shared counters,
compaction trigger, recovery idempotence. The product package additionally covers system-prompt
assembly, the byte-bounded memory update, the rating pass, and the maker trigger, plus the §11
filesystem contract: `JUICE_HOME`-rooted home resolution (fixed, never cwd-relative), layered
config (component defaults overridden per agent), the password precedence chain, idempotent home
creation at mode 0700, blank-file initialization that never truncates an existing file, and
keep-old-if-present legacy migration. `juice` is tested against `httptest` kernels. Every production
file has its `_test.go`.

**Flows (`flows/`).** End-to-end shell scripts against the compiled binary, booting a real
`juice serve` on a temp DB with the **real Ollama the kernel ships with** (no fake model). Mechanism
flows (`bootstrap`, `init`, `recovery`, `drain`) touch no model and are deterministic — `init` asserts
that a first launch under a temp `JUICE_HOME` creates `$JUICE_HOME/agent/<handle>/` with blank
`persona.md`/`knowledge.md`/`memory.md` and leaves pre-existing files untouched on relaunch. LLM
flows (`task`,
`role_law`, `rating`, `maker`, `persona`, `named`, `memory`) assert robust structural properties, not
exact model text, and skip (never fail) when the chat model is absent. The budget and turn-cap rails
are code invariants proven by `go test ./agent`; the flows assert the surviving invariant (spend
never exceeds budget). Nothing is committed until unit tests and flows pass.

**Experiments (`bench/`) — excluded from `go test ./...`.** `juice-agent bench <suite.json> [N]`
runs a versioned, immutable task suite (id, prompt, budget, machine-checkable success criterion)
against a live kernel + real LLM, N repetitions per task (default 5). Per-task metrics: success rate,
mean credits (read from the ledger), mean latency, mean turns. One JSON report per run — keyed by
agent version (`git describe`), suite version, kernel version, and model config — is written to
`bench/results/`, so competence is a diffable time series.
