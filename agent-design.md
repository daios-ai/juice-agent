# Juice Agent Design

Version: 0.1.2
Status: design document
Codename: `juice-agent`

This document is aligned with `requirements.md` 0.5.0: all work arrives as kernel tasks, each task
joins a persona keyed from the task alone (§6), the harness depends on two configured actions
(reasoning and action lookup), and the channel layer, the maker and the learned memory file are gone.

Revision 0.1.2 rewrites the turn primitive: the harness builds the tool list and drives selection
via `llm/chat` (JSON extracted), so plain-named tools (`reply`/`publish`/`subtask`) coexist with
catalog actions and NO fake echo actions pollute the catalog (the `reply.wasm.b64` blob is gone).

Revision 0.1.1 adds named-agent **identity** (`juice-agent serve <handle>`, §7), authored **persona +
knowledge** injected as a real system prompt (§5c), and durable **cross-task memory** (§5c).

## 1. Purpose and position

`juice-agent` is a Go daemon implementing a general task-solving agent whose entire capability
surface is a Juice kernel. It adapts pi's agent architecture — a small, hand-owned loop wrapped by
a policy shell, driven by replaceable surfaces — with one substitution that changes everything
downstream: **the tool set is not hand-coded; it is the Juice action catalog**, discovered at
runtime and invoked uniformly through `Call()`.

The daemon is an ordinary kernel client. It acts as one login, `handle@kernel` (e.g. `agent@work`),
over the juice CLI's session for it, and from that single fact inherits everything an agent needs:

| Need            | Kernel-native answer                                                          |
| --------------- | ----------------------------------------------------------------------------- |
| Reasoning       | a configured action meeting the reason contract, by default `sys@<kernel>/llm/chat` |
| Tools           | Active actions visible under `CanCall`, found via a configured action lookup  |
| Tool dispatch   | `POST /v1/run` — one primitive for every capability, local or federated        |
| Budget          | Its wallet: every call is priced and prepaid; spend is bounded by balance      |
| Address         | `agent@work` — reachable by tasks, the `message` native, and its own actions  |
| Accountability  | Every effect is a transaction: priced, receipted, attributed by the role law   |

No special agent API exists on the kernel side, and none is needed: the kernel already speaks
agent. Other agents and applications are simply other users; the multi-agent system is emergent,
mediated by the kernel, never orchestrated by a private protocol.

**One binary, many named agents.** `juice-agent serve research@work` and `juice-agent serve triage@work`
launch the same binary as two kernel accounts, each with its own **home** — `agent.md`, config, and
its personas (§5c, §7). Being ordinary accounts, they coexist and address one another through the
kernel. (pi runs processes under an orchestrator; here each agent is a named citizen of one kernel.)

The daemon holds **no model credentials, no API keys, no provider adapters**. pi needs ~34
provider transports and a normalized event vocabulary because it faces many vendors; `juice-agent`
faces exactly one interface — the kernel — which validates structure on its behalf.

## 2. Layering (pi's architecture, transposed)

pi's load-bearing seam is that the loop lives in a tiny core that knows nothing about disk,
sessions, or UI. Only **two** seams are actually load-bearing, so there are exactly **three
packages** — one per seam plus the product that uses both. (pi has more packages because its core
and provider layers are separately *published libraries*; ours are not, so extra packages would be
ceremony.)

```text
agent/           THE LOOP (pi's agent-core): pure, interface-driven, no I/O imports
juice/           kernel client: the only package that speaks HTTP to the kernel
cmd/juice-agent/ the product (package main): everything else — lifecycle, config, home resolution,
                 intake and personas, the policy shell (persona files, compaction, retries,
                 system-prompt assembly, rating), and the bench runner. Files are named for their
                 function (intake.go, execute.go, transcript.go, prompt.go, rating.go, …).
bench/           experiment-suite data (tasks.json, results/) — no Go
flows/           end-to-end shell flows against the compiled surface (not Go)
```

Dependency law, mirroring the kernel's own (`kernel` must not import SQLite/wazero/…) — the two seams
that matter:

- `agent` imports **neither `net/http` nor `juice`**. It depends only on small interfaces it defines
  (`Decider`, `Catalog`, `Runner`, `Transcript`). Tests use fakes. This is what keeps the loop pure.
- `juice` is the only package that speaks HTTP to the kernel.
- `cmd/juice-agent` depends on both and is the sole `package main`.

File counts stay small; files are named for their function; no file is split for size alone. **Every
production Go file has a corresponding `_test.go` with independent tests.** `go build ./...` and
`go test ./...` pass offline with no kernel, no LLM, no network.

## 3. The unit of work: a Job

A **Job** is one request for the agent: an id, the **persona** whose history it continues, its new
messages, and a **budget** in credits. A kernel task becomes one job (§6). One job = one loop run,
appended to its persona's file. The daemon serves jobs from a bounded worker pool; each job's loop
is serial, jobs in different personas are concurrent, and a persona runs one job at a time.

```text
Job     { id, persona, messages, budget }
Outcome { status ∈ {answered, failed, interrupted}, answer, reason, spent, turns }
```

The outcome is delivered by completing the task. `spent` is what the kernel charged the agent for
every call made for the job, reasoning and rating included, reconstructible from the ledger. The completion itself runs as the task's
requester and costs the agent nothing (kernel D6).

## 4. The loop

The core is pi's `runLoop`, with Juice substituted at both ends of each turn. Annotated
pseudocode; this is the whole of `agent/`:

```text
run(task):
  cands = intrinsics ∪ Catalog.Lookup(task.prompt, k)     # seed the working set
  loop:
    sel = Decider.Decide(messages, candidates, tools)     # ONE llm/chat round-trip, JSON extracted
    if sel is error:            fail (after policy-layer retries)
    append sel to transcript                               # {role: assistant, tool:{action,args}}

    if sel.action == "reply":                              # ← THE STOP RULE (a harness tool)
        return answered(sel.args)                          # terminal; nothing dispatched

    if spent + price(sel.action) > task.budget:
        append budget-refusal notice; continue             # the model is told, not killed

    res = Runner.Run(sel.action, sel.args)                 # POST /v1/run — the tool call
    spent += price(sel.action)
    append res to transcript                               # {role: tool, tool:{result}}

    if sel.action == lookup:                              # discovery is itself a tool call
        merge res.results into cands (bounded, recency-evicted)

    if turns == max_turns: fail(turn-cap)
```

**Turn primitive.** Each turn is one call of the reasoning affordance (`llm/chat` shape) over the transcript plus a rendered tool
list the harness builds itself: the catalog candidates (each with description, price, evidence, and a
compact schema) followed by the plain-named **harness tools** (`reply`, `subtask`). The
model replies with a single JSON object `{action, args}`, which the client extracts with a
balanced-brace parser. We do *not* use `llm/decide`: it only offers action references (so
plain tool names can't be candidates) and, on Ollama, its grammar-constrained JSON corrupts copied
strings (measured — timestamps garbled). Unconstrained chat + extraction is both more capable and
higher-fidelity. Validation splits cleanly: harness-tool args get a required-keys check in-harness;
catalog args are validated kernel-side at dispatch (`POST /v1/run` rejects a schema violation),
fed back to the model on failure.

**Stop rule.** pi's finding — continuation decided by *content*, never a provider stop reason — is
kept: the run ends exactly when the model selects the **`reply` tool**. "Done" is a selection like
any other, not a heuristic over free text. There is no other normal exit.

**Harness tools are not catalog rows.** `reply`/`subtask` are plain-named affordances the
daemon offers and intercepts — never registered on the platform, so the catalog stays a truthful
registry of real capabilities. Names never contain `/`, so they're unambiguous against action refs.
Each has a schema only to standardise the args the model must supply.

**Discovery is in-band.** The action lookup sits permanently in the candidate set, offered with the
terms the kernel holds for it. The model expands
its own tool set by selecting it — the harness runs it like any action and merges the results into
the working set (bounded, e.g. 24 actions, evicted by recency) so the context stays disciplined.
The working set is *per task* and starts fresh each run: what the agent can do is whatever the
catalog offers at that moment, local, imported, or federated.

**Follow-ups.** A follow-up is a new task, hence a new job in the same persona. Its persona file
already holds the earlier exchange, so the model has continuity while the loop stays session-agnostic:
it just receives a view and messages. The loop keeps an optional steering hook, polled before each
model call; the daemon supplies none.

**Delegation to other agents** stays out of the loop (as in pi): to use another agent, call its
published actions or park a task — the kernel is the orchestrator. *In-loop* decomposition is the
`subtask` tool (§4b), which isolates context without an unbounded tree.

### The `reply` tool

`reply` is a harness tool (schema `{answer: string, required}`), not an action. The harness treats
its selection as terminal (at the root) or as the roll-up of a bracket (§4b), and never dispatches
it. Nothing is registered on the platform for it — no WASM artifact, no bootstrap round-trip.

## 4b. Brackets (subtasks)

A long task pollutes the chat with lookups and dead ends. The `subtask` tool lets the model open a
**bracket** — a stack frame: given inherited context + a specific instruction, it runs in its own
scope and **rolls up** to a single result in the parent chat.

- **Down.** Selecting `subtask {instruction, context?}` opens a child frame seeded with the parent's
  **full turn-by-turn history** (so the subtask knows the real context — what was read, tried, and
  decided) + the optional context + the instruction. Inheritance flows only *down*; the child's own
  turns still roll up as one result, so the parent chat stays clean regardless.
- **Inside.** The child runs the ordinary loop with its own working set, but is **not offered the
  `subtask` tool** — nesting is capped at one sublevel. A subtask must do the work itself, not
  re-delegate: since the child is a clone of the parent (same prompt, same context), an available
  `subtask` tool one level down makes it re-delegate the same instruction indefinitely (observed
  live — a 30-deep spiral to `turn-cap`). Withholding the tool is a structural cap, not a soft nudge.
  All its turns — lookups, results, dead ends — live in this frame only.
- **Up.** Selecting `reply` inside the bracket *pops* it: the parent chat gets exactly one
  tool-result (`{action: subtask, result: <reply args>}`); the child's internal turns never enter
  the parent's model view.

**The bound is a shared counter.** Turns are one `counters{turns}` per task and credits one `Meter`,
threaded by pointer through the recursion (the meter also through the reasoning, compaction and
rating calls) — a subtask tree totals ≤ `max_turns` and ≤ `budget`, not
per-frame: you cannot spend turns you do not have. (Depth is separately capped at one sublevel by
withholding the `subtask` tool from a child — see *Inside* — so the tree is at most root → subtasks,
never deeper.) Exhaustion inside a bracket rolls up an error result the parent can act on; at the
root it fails the task with `turn-cap`. This is the property none of pi/opencode/OpenClaw has — their
subagents each get a *fresh* budget; ours share one.

**Durable but clean.** Every record is teed to the persona's file, tagged with its bracket id,
so rating (`Transcript.All()`, this job's records only) sees bracketed calls too; only the parent's *model view*
(`Messages()`) excludes them. The chat the model reads at any node is the root→node path, never a
sibling's noise — the transcript reads like the reduced form (`subtask: find X → X = …`), with the
messy interior derivations already collapsed.

### Safety rails

pi ships no runaway rails and its report names that the gap that matters for unattended runs. This
daemon *is* unattended, so it takes both rails — but the primary one is economic:

1. **Budget (primary).** Every call the job pays for costs its price, prepaid: the tools the model
   chooses and the harness's own reasoning, lookup, compaction and rating calls, all checked against
   one per-job meter and counted at what the kernel charged. The model is not free: `llm/chat`
   costs the price the kernel's operator gives the model. A doom loop of priced calls halts on the
   task budget; the wallet bounds the daemon globally. This is a rail pi could not have: its tools
   were free. The affordances' terms are read for each job and every call is pinned to them, so an
   owner who raises a price mid-job gets a refused call, not the agent's wallet.
2. **Turn cap (secondary, default 32).** Required because zero-price actions (lookup,
   a model priced at 0, refused selections) escape the money rail. A configurable hard cap; hitting
   it fails the task honestly with `reason = turn-cap`.
3. **Repeat guard (§4c).** Catches the case the other two miss: a *free, succeeding* action selected
   over and over (a real observed failure — an agent posted the same chat message 31× and never
   stopped).

Retries (3×, exponential 2s/4s/8s) live in the policy layer (outside the pure loop), as in pi, and apply
only to transport-level and retryable kernel errors (network, 5xx, 429, `ErrTimeout`) — never to
`ErrInsufficientFunds`, schema violations, or a user abort.

## 4c. Repeat guard

Budget and the turn cap both fail against a cheap action that *succeeds* on every call: the model
keeps re-selecting the same action, never selects `reply`, and the task reads `failed / turn-cap`
even though it functionally succeeded — with the side-effect spammed. Two live runs fixed the design.
An exact-content detector is useless: shown its own prior output in the transcript, a sampled model
*paraphrases* rather than repeats verbatim (one run posted the same reply 19 times, 16 distinct
strings, zero byte-identical pairs). So the signal is the **action**, not its arguments.

- **Detect on the last action.** A selection whose action equals the last *dispatched* action is a
  repeat (arguments ignored). `reply` is exempt — it is the exit we *want* the model to take.
- **Drop and re-query, don't append.** A repeat is **neither persisted nor dispatched**: the loop
  discards it, ensures a single reworded nudge sits at the tail — *"you just ran `<action>`, which
  already completed; are you sure, or is the task complete? if complete, select `reply`"* — and
  re-queries. This keeps the chat clean and, crucially, avoids **self-priming**: an autoregressive
  model continues the patterns it sees, so leaving repeats in context makes the next repeat more
  likely. Because the repeat never dispatches, the side-effect is bounded to the **one** legitimate
  call.
- **The model's answer is the signal.** Back down (`reply`, or a different action) → resolved
  gracefully, the common case. Persist in the same action → dropped again.
- **Turn-counted, hard floor.** Each re-query costs a turn (the model pays for its own fumble), so
  `max_turns` bounds the rephrase loop. After `max_repeats` consecutive repeats (default 3) the frame
  hard-stops with `reason = repeat-loop` (a bracket rolls up `{error: repeat-loop}` its parent can
  act on). State is per-frame, so a bracket's repeats are its own and never trip the parent.

This is opencode's confirm-before-repeat (it asks the *human* at 3 identical calls) turned into a
*model*-in-the-loop rephrase-and-retry, with a deterministic floor underneath as the guarantee — a
shape none of pi (no rail), opencode (human prompt), or OpenClaw (count breaker at 30) has.
`max_repeats < 0` disables it. A known cost: a legitimate run of the same action over different
inputs is nudged and, past the floor, hard-stopped; exempting specific actions is a future refinement.

## 5. Persistence and compaction

**Persona file.** Append-only JSONL, one file per persona under `personas/`, written as each record
is produced (pi's model; no session tree). Every job of the persona appends to it, so the file is
both the conversation's memory and its replay log. Aborts and interruptions are *persisted as
records*, never just thrown: a persona file found at startup whose last record is not terminal is
marked `interrupted` — the agent-side mirror of the kernel's `reason = interrupted` recovery.
Recovery is idempotent.

**Continuing a persona.** Opening a persona replays its root records into the view the last job
held, with no model call: each job's system prompt starts a job and heads the view, the previous
job's system prompt and outcome are dropped, and a compaction summary replaces all but the tail it
records it kept. The rebuild is therefore exact, which a rule like "start at the last summary" is
not: the summary is written after the tail it keeps.

**Compaction.** When a view nears the byte budget, older turns are replaced by a summary written by
the reasoning affordance using pi's fixed template (Goal / Constraints / Progress / Key Decisions /
Next Steps / Critical Context), keeping a recent tail verbatim and cutting only at turn boundaries.
The kernel does not report token usage, so the trigger is a configurable byte budget — a proxy,
chosen for legibility over precision.

## 5b. Evidence and ratings

Two kernel facilities that a naive port ignores; each becomes a concrete behaviour.

**Evidence-informed selection.** Every action read carries `evidence` — what this kernel recorded
about it (`local_experience`: uses, successes, failures, latency, ratings), what its provider
reports, and what other kernels observed — labelled by provenance and never summed or scored
(kernel U39). Lookup rows are indicative, so on every candidate merge the harness reads each
candidate authoritatively (`GET /v1/actions?ref=`) and keeps its price, `quote_hash` and evidence;
the client renders this kernel's own record inline in the tool list it builds each turn
(`owner@kernel/name — desc; price=P; record: {…}; args: {…}`), so track record rides with the
choice — no separate message. The candidate's `quote_hash` travels with its dispatch, so the price
the model saw is the price charged or the call is refused.

Eviction is by recency: when the working set exceeds `candidate_limit`, the least recently merged
candidate is dropped. Ranking by evidence would manufacture the opaque score the kernel refuses to
compute.

**Rating pass.** Only a transaction's caller may rate it (kernel D11, U14) — and the agent is the
caller of every call it dispatches, so it is the rightful rater of all its tool use. A task's
completion runs as its requester, so the agent never rates it. After a job ends, a rating pass (in
the policy layer, never in the loop — mirroring the kernel law that execution must not rate) judges
each call the job dispatched with one structured call of the reasoning affordance and posts
`rating ∈ {0,1}` plus a one-line note per transaction (`POST /v1/transactions/{id}/rate`); a
kernel-failed call is a mechanical `0`. Only the job's own records are judged: a persona's earlier
jobs were rated once already, and the kernel refuses a second rating. These notes are the agent's
durable memory of tool quality, held not in a private store but in the kernel's own reputation
system: they enter the action's evidence, which every later reader, this agent included, sees.
Because every reader sees them, a note describes only how the tool performed: a failure's note is
the fixed `call failed`, since an error text can echo the call's arguments, and the judge is told
never to repeat the request or name anyone, which lowers the risk of disclosure without
guaranteeing it.

**Harness tools.** `reply` and `subtask` (§4b) are plain-named tools the loop offers and intercepts —
not registered actions. The core keeps an intrinsics table keyed by tool name; `reply` is terminal,
`subtask` opens a bracket. Adding an agent-native capability is a table entry plus a `Tool`
definition — no platform registration, no artifact.

## 5c. Identity and the agent file

An agent has one human-authored file, `agent.md`: persona, standing instructions and domain
knowledge (pi's `AGENTS.md`). It is legible, read at every job start, never written by the agent, and
the only state shared by all personas. The agent home is `$JUICE_HOME/agents/<login>/`, holding
`agent.md`, an optional `config.json` and `personas/`, so a differently-behaved agent is a login and
a directory, not new code.

**System prompt.** Before each job the policy layer (not the pure loop) assembles one `system`
message — role line + `agent.md` + the operating contract (selection protocol, budget, terminal
tool) — and prepends it. One builder, no per-model variation (the kernel is the one provider); the
catalog isn't restated (each candidate already carries its schema, price and evidence, §5b).

**No learned cross-persona memory.** An earlier design summarised every job into a `memory.md`
injected into every later job. Under personas that file is a leak: what Alice said in private would
reach a room Bob reads, or an action Bob owns that Alice happens to call. A persona's own history is
the agent's learned state, and nothing crosses personas but what the operator writes in `agent.md`.

## 6. Intake and personas

All work arrives as kernel tasks. Intake polls `GET /v1/tasks?status=waiting` and keeps the tasks
whose `required_caller` is the agent. The kernel lists a task only if its requester was approved by
the agent's operator (kernel U52), so approval is the access policy and the daemon adds none. The
agent knows no application: an earlier design had a channel layer with a service driver written for
one chat application's room ids, read and post actions and grant recipe, which specialised the agent
to conventions the kernel does not define. It is removed.

**Why the key is what it is.** When a task arrives the agent must decide which conversation it
belongs to, from the task alone, before it knows anything about the application that created it. The
kernel certifies `requester` (whom the task acts for), `owner` (the payer) and `created_by` (the
creating action), but not a conversation. Take an application, Rooms, relaying posts: Alice's post
in room *team* and her post in room *secret* have identical certified fields, and Carol's post in
*team* differs from Alice's only in `owner`. Every key built from certified fields therefore either
merges the two rooms or splits one room by member. Separation must come from the application, as
OpenClaw takes it from each platform's chat id and email from `In-Reply-To` (RFC 5322); unlike
OpenClaw the agent can write no adapter per application, because the set of applications is open. So
the convention is one argument name, `thread_id`, and the key is:

```text
thread_id present          → (requester, created_by, "t:" + thread_id)
no thread_id, direct       → (requester, created_by)            requester = owner
no thread_id, relayed      → (requester, created_by, "task:" + id)
```

Errors are asymmetric: merging two conversations discloses one audience's turns to another, while
separating them only loses continuity. The rule therefore fragments whenever continuity is not
established by certified fields — a direct sender has no intermediary, a relayed task without
`thread_id` does. The identifier is namespaced by requester and creating action, so an application
cannot reach another's persona by naming its thread. An application can misstate its own threads;
the agent certifies attribution, never an application's assertions, and under the kernel's ex-post
trust model (G10) a misbehaving requester is judged afterwards and its approval revoked.

**Attribution.** Owner is not in the key, because one thread has many members, but every turn names
its sender: `alice@k: …`, or `alice@k via rooms@k: …` when an application relays it. The owner is
certified; the requester's code wrote the words and receives the reply, so both are shown.

**Delivery.** A job's answer completes its task, shaped onto the task's `allowed_input` so that the
onward action's own schema, not knowledge of the application, decides where the text goes. A failed
job still completes, releasing the sender's process. A transient completion failure is resent at
later polls without running the job again.

## 7. Daemon lifecycle and configuration

`juice-agent serve <login>` runs continuously: resolve the agent home from the login → load config
→ open the login's session from the juice CLI's records and check that `GET /v1/me` answers as that
login's account → assemble the harness tools (`reply`, `subtask`), which are plain-named and
intercepted, never registered on the platform (§4) → run startup recovery over `personas/` → start
intake and the worker pool. Each job reads `agent.md` into its system prompt and settles with the
rating pass (§5b). `SIGTERM`/`SIGINT` drains as `juice kernel serve` does: stop intake, let in-flight
jobs finish and complete their tasks, persist, exit. There is no `juice-agent stop`.

The design follows the ecosystem standard rather than inventing a credential model. A login,
`handle@kernel`, is what every juice-family program names; the juice CLI obtains its session and
keeps it in `$JUICE_HOME/client/credentials/<login>.json`, and the daemon reads and rotates that file
under the same lock the CLI uses. The daemon therefore holds no password, offers no password flag or
prompt, and needs no terminal library. An agent's home is `$JUICE_HOME/agents/<login>/`, so its
`agent.md` and personas survive any kernel.

Config is an optional `<home>/config.json` — kernel-style: flat keys, safe defaults, an unknown key
rejected at startup, no secrets. The daemon never writes it, so defaults change with the binary:

```json
{
  "reason": "sys@<kernel>/llm/chat",
  "action_lookup": "sys@<kernel>/lookup",
  "default_budget": 100,
  "max_turns": 32,
  "concurrency": 4,
  "poll_seconds": 5,
  "compact_bytes": 262144,
  "rate_tasks": true
}
```

`reason` and `action_lookup` are the only two actions the harness depends on, so they are the only
action addresses in configuration, and either can be swapped for any action meeting its contract:
the input the agent sends and the output it reads, written in the kernel's canonical schema form
(requirements §11). The daemon checks both at startup against the schemas the kernel declares for
them. The kernel is the authority on an action's terms, so the check compares declared schemas
rather than calling the action, and it runs once, outside the loop. Every other capability — tools,
delegation, compilation, embedding — is an ordinary action the model finds through the action
lookup. The loop's tuning (repeats, working-set size, lookup rows) is fixed in code: it
shapes the loop, not the operator's policy. The login and the `agent.md` filename are not config
keys.

Logging mirrors the kernel: structured lines to stderr, stdout reserved for resource payloads;
required fields `time level event job persona turn action tx_id status duration_ms error`.
Every turn logs the selection, price, and outcome; the ledger remains the financial ground truth.

## 8. Testing: three tiers

**Unit (`go test ./...`).** Offline, no kernel, no LLM. `agent` is tested against fake `Decider`/
`Catalog`/`Runner`/`Transcript` implementations: stop rule, budget refusal, turn cap, steering
injection, candidate-set merging and eviction, compaction triggering, recovery idempotence.
The product-package tests additionally cover intake (the persona key, attribution, one job per
persona, completion and its resend), persona files (exact rebuild after compaction, job-scoped
rating), system-prompt assembly from `agent.md`, and agent-home / login resolution. `juice` is tested against `httptest` kernels.
Every production file has its `_test.go`.

**Flows (`flows/`).** End-to-end shell scripts against the *compiled* surface, exactly as
`../juice/flows`: a `lib.sh` (start a real `juice serve` on a temp DB, start `juice-agent`, `assert_*`
helpers) and `flows_agent.sh` under a `flows_test.sh` orchestrator. The kernel runs the **real LLM
it ships with** — Ollama behind the `llm/*` natives — because that is the shipping configuration; there is
no fake model. Consequences:

- *Mechanism flows* make deterministic assertions about intake, approval, personas and lifecycle.
- *LLM-in-the-loop flows* (`answer`, `agent_md`, `rating`, `conversation`) assert robust
  **structural** properties — the answer matches a shape (an ISO timestamp), a dispatched call is
  rated, a persona recalls what its sender said earlier and another persona does not — not exact
  model text. They guard on `require_ollama` and *skip* (not fail)
  when the chat model is absent. Timeouts are generous (a real model takes tens of seconds/turn).
- The **budget and turn-cap rails are code invariants**, proven exhaustively by `go test ./agent`;
  a real model cannot be coerced into the pathological choices that would stage them on demand.
  `flow_budget` stages the budget with a reasoning action that always replies; the model flows
  assert the surviving invariant (spend never exceeds budget). Loop resilience matters more with a real model: a single invalid model selection is fed
  back for self-correction (`MaxDecideErrors` consecutive before the run fails), and model-output
  errors (`execution_failed`/`schema_violation`) are not retried as if transient.

```text
flow_bootstrap   daemon logs in and serves; no harness tool registered in the catalog; restart  [no LLM]
flow_init        first launch creates agents/<login>/ with blank agent.md and personas/        [no LLM]
flow_config      a retired config key is refused, naming the file                              [no LLM]
flow_no_session  a login with no session, an unrecorded kernel, or a bare handle is refused    [no LLM]
flow_recovery    a mid-flight persona file at startup is marked interrupted, idempotently      [no LLM]
flow_drain       SIGTERM drains in-flight work, persists, exits 0                               [no LLM]
flow_tasks       a task addressed to the agent is completed as its sender; another's is left   [no LLM]
flow_budget      a paid reasoning turn is counted; a price raised mid-run fails the next job   [no LLM]
flow_approval    an unapproved sender's task is withheld until the operator approves it        [no LLM]
flow_personas    one file per sender; a sender's next job loads the history; others kept apart [no LLM]
flow_named       serve alice@k and serve bob@k launch as distinct logins, each in its own home  [no LLM]
flow_session_identity  another account's session stored under the login is not adopted       [no LLM]
flow_usage       usage fits 80 columns and a 50-column terminal                                [no LLM]
flow_answer      a real question answered with an ISO time; spend ≤ budget and = balance fall  [real LLM]
flow_agent_md    agent.md reaches the system prompt                                            [real LLM]
flow_rating      a dispatched tool call is rated; a second job in the persona does not re-rate [real LLM]
flow_conversation a persona recalls a codeword from its earlier message; another sender's does not [real LLM]
```

Per project law, nothing is committed until unit tests and flows pass.

**Experiments (`bench/`) — excluded from `go test ./...`.** Correctness is binary; competence is
graded and stochastic. The experiment suite is the quantitative instrument for tracking the agent
across versions:

- **Task set.** A versioned suite file (`bench/tasks.json`): each task has an id, a prompt, a
  budget, and a machine-checkable success criterion over the final answer (`equals`, `regex`,
  `jsonpath`) and optionally over kernel state (e.g. "an action matching X exists and is active").
  The suite is immutable per version; changing tasks bumps the suite version.
- **Runner.** `juice-agent bench` runs the suite against a live kernel with a real LLM, N repetitions
  per task (default 5) to respect stochasticity.
- **Metrics.** Per task: success rate, mean credits spent (read from the kernel ledger, not
  instrumented), mean wall latency, mean turns. Aggregate: suite success rate as the headline
  number, cost and latency as efficiency measures.
- **Results.** One JSON report per run — `{agent_version (git describe), suite_version, kernel
  version, model config, per-task and aggregate metrics}` — written to `bench/results/` and
  committed, so performance is a diffable time series and a regression is a number moving, not an
  impression.

## 9. Design rationale

The agent is a user because the kernel's user model already is an agent model: authentication,
budget, address, and audit arrive as one primitive, and every other agent or human composes with
it through the same economy. The loop is hand-owned, minimal, and content-stopped because pi
demonstrated those are the properties that survive contact with unreliable signals — and Juice
lets each of pi's defensive mechanisms become a kernel contract: validation by `decide`,
termination by a schema-checked terminal action, runaway protection by money. The tool set is
discovered, not declared, because a fixed menu would forfeit the platform's central property — an
open, priced, rated action space that grows without the agent changing. Intake is tasks alone because the kernel's one signal to an account is a task, and every
application, agent and person reaches the agent through it with no pairwise integration; a
transport written for one application would bind the agent to conventions the kernel does not
define. The experiment suite exists because this agent will be
developed over time: without a fixed task set and a committed metric series, "better" would be
anecdote; with them, it is a measurement.

**Relation to MCP.** An MCP server exposes typed, described, discoverable, remotely invokable
capabilities (tools) to an LLM harness. An agent that publishes actions satisfies that definition
exactly — an application (actions sharing the prefix `owner@kernel/<name>/`) ↔ a server, action ↔
tool, the application's index `owner@kernel/<name>/index` (kernel U50) ↔ `tools/list`, `POST /v1/run` ↔
`tools/call`, grants ↔ per-server
auth, a `required_caller` task ↔ elicitation, the trace-scoped capability ↔ the server→client callback —
so at the interface level the correspondence is an inclusion, not an analogy: the kernel is the
transport. It is however an embedding, not an isomorphism. Four kernel properties have no MCP image,
and each removes a known MCP limitation: **symmetry** (every participant is both client and server under
one identity; MCP fixes an asymmetric split), **global discovery** (one catalog replaces N×M pairwise
configuration), **economics** (every call is priced and prepaid — the budget rail MCP lacks), and
**durability/reputation** (tasks are funded continuations; actions carry ledger-backed evidence). The
kernel's private actions (non-public rows, grant-gated) sharpen the picture: an agent whose public
actions front a private toolbox is an MCP server *implemented by* an MCP client — the two roles MCP
must run as separate processes collapse into one identity, with the public/private flag as the
encapsulation boundary. (Agents publishing such private toolboxes is positioning, not yet built.)
Honest gaps in the correspondence: MCP resources, prompts, and per-session notifications have no
first-class Juice analog.

Identity is a *directory* because what distinguishes `research` from `triage` is data, not code — so
a new agent is a new home and the binary stays single, and multi-agent composition falls out of the
shared-kernel identity model rather than a supervisor. `agent.md` is an *authored file* because tone
and priming are what an operator should own and version. Personas are keyed from the task alone
because the agent must thread work from applications it has never seen, and they never share learned
state because the cost of a wrong merge is disclosure.
