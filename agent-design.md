# Juice Agent Design

Version: 0.1.2
Status: design document
Codename: `juice-agent`

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
| Reasoning       | `sys@<kernel>/llm/chat` (and `/llm/json`, `/llm/embed`), called like any action |
| Tools           | Active actions visible under `CanCall`, found via `sys@<kernel>/lookup`       |
| Tool dispatch   | `POST /v1/run` — one primitive for every capability, local or federated        |
| Budget          | Its wallet: every call is priced and prepaid; spend is bounded by balance      |
| Address         | `agent@work` — reachable by tasks, the `message` native, and its own actions  |
| Accountability  | Every effect is a transaction: priced, receipted, attributed by the role law   |

No special agent API exists on the kernel side, and none is needed: the kernel already speaks
agent. Other agents (e.g. `chat`) are simply other users; the multi-agent system is emergent,
mediated by the kernel, never orchestrated by a private protocol.

**One binary, many named agents.** `juice-agent serve research@work` and `juice-agent serve triage@work`
launch the same binary as two kernel accounts, each with its own **home** — a directory of config, persona/knowledge,
and memory (§5c, §7). Being ordinary accounts, they coexist and address one another through the
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
                 channels (intake), the policy shell (transcripts, compaction, retries, system-prompt
                 assembly, memory, rating), the maker, and the bench runner. Files are named for
                 their function (execute.go, transcript.go, prompt.go, channels.go, maker.go, …).
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

## 3. The unit of work: a Task

A **Task** is one request for the agent: an id, an originating channel, an initial message (or
conversation), and a **budget** in credits. One task = one loop run = one transcript. The daemon
serves tasks from a bounded worker pool; each task's loop is serial, tasks are concurrent.

```text
Task    { id, channel_id, messages, budget, created_at }
Outcome { status ∈ {answered, failed, interrupted}, answer, reason,
          spent, turns, started_at, ended_at }
```

The outcome is delivered back through the task's originating channel. `spent` is exact: it is the
sum of the prices of the calls the agent made, reconstructible from the kernel ledger (every one
of the agent's calls is a transaction where it is `owner_user_id`).

## 4. The loop

The core is pi's `runLoop`, with Juice substituted at both ends of each turn. Annotated
pseudocode; this is the whole of `agent/`:

```text
run(task):
  cands = intrinsics ∪ Catalog.Lookup(task.prompt, k)     # seed the working set
  loop:
    inject queued steering messages                        # polled before every model call
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

**Turn primitive.** Each turn is one `llm/chat` call over the transcript plus a rendered tool
list the harness builds itself: the catalog candidates (each with description, price, evidence, and a
compact schema) followed by the plain-named **harness tools** (`reply`, `publish`, `subtask`). The
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

**Harness tools are not catalog rows.** `reply`/`publish`/`subtask` are plain-named affordances the
daemon offers and intercepts — never registered on the platform, so the catalog stays a truthful
registry of real capabilities. Names never contain `/`, so they're unambiguous against action refs.
Each has a schema only to standardise the args the model must supply.

**Discovery is in-band.** The lookup native sits permanently in the candidate set. The model expands
its own tool set by selecting it — the harness runs it like any action and merges the results into
the working set (bounded, e.g. 24 actions, evicted by recency) so the context stays disciplined.
The working set is *per task* and starts fresh each run: what the agent can do is whatever the
catalog offers at that moment, local, imported, or federated.

**Steering.** Channels may push mid-task messages; the loop injects them before the next model
call, exactly as pi does. Follow-ups (input after completion) are channel semantics: a follow-up is a
new task **seeded with the conversation's recent turns**, which a dedicated channel keeps in a small
per-session store and prepends to the task's messages — so the model has continuity while the loop
stays session-agnostic (it just receives messages, §4). Only the *visible* turns are seeded — the
incoming messages and the agent's posted answers — never the prior tool calls or reasoning.

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

**The bound is a shared counter.** Turns and credits are one `counters{turns, spent}` per task,
threaded by pointer through the recursion — a subtask tree totals ≤ `max_turns` and ≤ `budget`, not
per-frame: you cannot spend turns you do not have. (Depth is separately capped at one sublevel by
withholding the `subtask` tool from a child — see *Inside* — so the tree is at most root → subtasks,
never deeper.) Exhaustion inside a bracket rolls up an error result the parent can act on; at the
root it fails the task with `turn-cap`. This is the property none of pi/opencode/OpenClaw has — their
subagents each get a *fresh* budget; ours share one.

**Durable but clean.** Every record is teed to the one task transcript, tagged with its bracket id,
so rating and memory (`Transcript.All()`) see bracketed calls too; only the parent's *model view*
(`Messages()`) excludes them. The chat the model reads at any node is the root→node path, never a
sibling's noise — the transcript reads like the reduced form (`subtask: find X → X = …`), with the
messy interior derivations already collapsed.

### Safety rails

pi ships no runaway rails and its report names that the gap that matters for unattended runs. This
daemon *is* unattended, so it takes both rails — but the primary one is economic:

1. **Budget (primary).** Every real tool call costs its advertised price, prepaid. A doom loop of
   priced calls halts on the task budget; the wallet bounds the daemon globally. This is a rail pi
   could not have: its tools were free.
2. **Turn cap (secondary, default 32).** Required because zero-price actions (lookup,
   the `llm/*` natives, refused selections) escape the money rail. A configurable hard cap; hitting
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

## 5. Persistence, compaction, memory

**Transcript.** Append-only JSONL, one file per task under the data dir, written as each record is
produced (pi's model; no session tree — a daemon's tasks are linear). The transcript is the task's
working memory and its replay log. Aborts and interruptions are *persisted as records*, never just
thrown: a task found at startup whose transcript lacks a terminal record is marked
`interrupted` — the agent-side mirror of the kernel's `reason = interrupted` recovery (§5 of the
kernel requirements). Recovery is idempotent.

**Compaction.** When a transcript nears the context budget, older turns are replaced by a summary
written by `llm/chat` using pi's fixed template (Goal / Constraints / Progress / Key
Decisions / Next Steps / Critical Context), keeping a recent tail verbatim and cutting only at
turn boundaries. The kernel does not report token usage, so the trigger is a configurable byte
budget — a proxy, chosen for legibility over precision.

**Within-task memory.** This section covers only the *working* memory of a single task: the
transcript and its compaction summary. Two other memories — the **authored** persona/knowledge the
agent is given, and the **learned** cross-task memory it accumulates — are the subject of §5c, which
also specifies the system prompt that carries them into each task. A dedicated channel may also keep a
**per-conversation window** of recent visible turns that it seeds into follow-up tasks (§4 Steering,
requirements §10) — channel state for continuity, distinct from these agent memories and holding no
tool traces. Both stay legible files in pi's
spirit (readable, greppable, diffable), not a semantic store.

## 5b. Evidence, ratings, and self-extension

Three kernel facilities that a naive port ignores; each becomes a concrete behaviour.

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

**Rating pass.** Only a transaction's payer may rate it (kernel D11) — and the agent pays for every
call it makes, so it is the rightful rater of all its tool use. After a task ends, a rating
pass (in the policy layer, never in the loop — mirroring the kernel law that execution must not rate)
judges each dispatched call over the finished transcript with one `llm/json` call and posts
`rating ∈ {0,1}` plus a one-line note per transaction (`POST /v1/transactions/{id}/rate`); a
kernel-failed call is a mechanical `0`. Duplicate ratings are skipped. These notes are the agent's
durable memory of tool quality, held not in a private store but in the kernel's own reputation
system: they enter the action's evidence, which every later reader, this agent included, sees. The agent's experience thus compounds into platform-visible reputation.

**Self-extension (the maker).** The agent can publish actions, exactly as `build.py` does. It keeps
a legible task index (`data_dir/tasks.jsonl`: prompt, outcome, spent, actions used). After a task,
a trigger fires when ≥`min_repeats` semantically similar prompts (cosine over `llm/embed`) have
recurred within a window. It then enqueues an internal **synthesis job** to the agent itself via a
`self` channel. Synthesis runs in the ordinary loop with the harness tool `publish`; the job names
the `sys@<kernel>/tinygo/compile` native, which the model offers itself through lookup. `publish` implements `build.py`'s transactional publish: create private
→ enable → run declared `test_inputs` → make public on pass, or delete and return diagnostics on
fail. Once published, future repetitions find the new action through ordinary lookup — the
automation loop closes without changing the daemon.

**Harness tools.** `reply`, `publish`, and `subtask` (§4b) are plain-named tools the loop offers
and intercepts — not registered actions. The core keeps an intrinsics table keyed by tool name;
`reply` is terminal, `publish` the maker handler, `subtask` opens a bracket. Adding an agent-native
capability is a table entry plus a `Tool` definition — no platform registration, no artifact.

## 5c. Identity, persona, knowledge, and memory

Beyond the per-task working memory of §5, an agent has two durable memories, both **legible files**
(pi's stance: readable, greppable, correctable), differing only in authorship:

- **Authored** — `persona.md` (tone, standing instructions) and `knowledge.md` (durable domain
  knowledge, e.g. how Juice works). Human-written.
- **Learned** — `memory.md`, the agent's own running summary of past tasks.

Both live in the **agent home**, a directory keyed by handle under the juice-family root
(`$JUICE_HOME/agent/agents/<handle>/`, default `~/.juice`) that also holds `config.json` and `data/`
(transcripts, maker index). `juice-agent serve research` resolves this home, so a differently-behaved
agent is a directory, not new code. This splits pi's `AGENTS.md` along the authorship seam. The exact
filesystem convention is normative in `requirements.md` §11.

**System prompt.** Before each task the policy layer (not the pure loop) assembles one
`system` message — role line + `persona.md` + `knowledge.md` + `memory.md` digest + the operating
contract (decide protocol, budget, terminal action) — and prepends it. This is the builder 0.1's §5
claimed but never wired. One builder, no per-model variation (the kernel is the one provider); the
catalog isn't restated (decide already gets each candidate's schema/price, §5b carries evidence). The
loop is unchanged — it receives messages and never knows a prompt exists.

**Learned memory.** After a job settles (alongside the rating pass, §5b), one `llm/chat` call
summarises what it did and learned into a dated `memory.md` entry; the next task loads `memory.md`
into the system prompt, so the agent carries history forward. Update is best-effort (never fails the
task) and lives in the policy layer. `memory.md` is byte-bounded (`memory_bytes`): over budget, §5's
compaction folds the oldest entries into a digest. Representation is a summarised file, not an
embedding store — the middle path between pi (no automatic memory) and OpenClaw (vectors + wiki);
escalate to an `llm/embed` index only if a readable file stops scaling. `memory_bytes = 0`
reverts to pi's strict stance.

## 6. Channels (intake abstraction)

Where work comes from is a replaceable boundary — pi's "surfaces" seam. A channel converts an
external conversation into Tasks and delivers Outcomes (and steering) back:

```text
Channel {
  Start(ctx, submit func(Task)) error     // begin producing tasks
  Deliver(taskID, Outcome)                // report a result
  Steer(taskID) []Message                 // optional mid-task input
}
```

Channels are deliberately **not** fixed to kernel-native transports. Planned implementations:

- `task` — kernel-native: poll `GET /v1/tasks?status=waiting` for tasks addressed to the agent
  (`required_caller = agent@work`); the task's `partial_args` carry the request; completing the task
  delivers the outcome, with the transaction obeying the role law. This makes the `message` native
  and any other agent (e.g. `chat@work`) an input channel with zero pairwise integration.
- `http` — Juice-independent: a local HTTP endpoint accepting tasks directly.
- `cli` — a one-shot dev surface (`juice-agent task <login> "<prompt>"`), the analog of pi's print mode.

More channels will follow; the decision of which channels a deployment enables is configuration,
not architecture. The loop never knows which channel a task came from.

## 7. Daemon lifecycle and configuration

`juice-agent serve <login>` runs continuously: resolve the agent home from the login → load config
→ open the login's session from the juice CLI's records and check that `GET /v1/me` answers as that
login's account → assemble the harness tools (`reply`, `publish`, `subtask`), which are plain-named
and intercepted, never registered on the platform, so startup makes no bootstrap round-trip for them
(§4) → load `persona.md` / `knowledge.md` / `memory.md` from the home and build the system prompt →
run startup recovery (mark interrupted jobs) → start channels and the worker pool. Each job settles
with the rating pass (§5b) and a best-effort **memory update** (§5c). `SIGTERM`/`SIGINT` drains as
`juice kernel serve` does: stop intake, let in-flight turns finish, persist, exit. There is no
`juice-agent stop`.

The design follows the ecosystem standard rather than inventing a credential model. A login,
`handle@kernel`, is what every juice-family program names; the juice CLI obtains its session and
keeps it in `$JUICE_HOME/client/credentials/<login>.json`, and the daemon reads and rotates that file
under the same lock the CLI uses. The daemon therefore holds no password, offers no password flag or
prompt, and needs no terminal library. An agent's home is `$JUICE_HOME/agents/<login>/`, so its
memory, persona and transcripts survive any kernel; memory entries are tagged with the kernel key and
principal id they were learned under, so memory about another kernel or account is never loaded as
if it were about this one.

Config is an optional `<home>/config.json` — kernel-style: flat keys, safe defaults, an unknown key
rejected at startup, no secrets. The daemon never writes it, so defaults change with the binary:

```json
{
  "default_budget": 100,
  "max_turns": 32,
  "concurrency": 4,
  "candidate_limit": 24,
  "compact_bytes": 262144,
  "memory_bytes": 65536,
  "rate_tasks": true,
  "maker": { "enabled": false, "min_repeats": 3, "window": 50, "budget": 40 },
  "channels": { "task": { "poll_seconds": 5 }, "http": { "addr": "" } }
}
```

The login, the fixed `persona.md`/`knowledge.md`/`memory.md` filenames, and the derived
`<home>/data/` are not config keys. Transcripts and the maker's index live under `<home>/data/`;
`memory.md` under the home. `memory_bytes = 0` reverts to pi's strict stance (no automatic
cross-job memory).

Logging mirrors the kernel: structured lines to stderr, stdout reserved for resource payloads;
required fields `time level event job channel turn action tx_id status duration_ms error`.
Every turn logs the selection, price, and outcome; the ledger remains the financial ground truth.

## 8. Testing: three tiers

**Unit (`go test ./...`).** Offline, no kernel, no LLM. `agent` is tested against fake `Decider`/
`Catalog`/`Runner`/`Transcript` implementations: stop rule, budget refusal, turn cap, steering
injection, candidate-set merging and eviction, compaction triggering, recovery idempotence.
The product-package tests additionally cover the new 0.1.1 policy: system-prompt assembly (role + persona + knowledge
+ memory digest, with absent files skipped), the post-task memory update and its byte-bounded
self-compaction, and agent-home / handle resolution. `juice` is tested against `httptest` kernels.
Every production file has its `_test.go`.

**Flows (`flows/`).** End-to-end shell scripts against the *compiled* surface, exactly as
`../juice/flows`: a `lib.sh` (start a real `juice serve` on a temp DB, start `juice-agent`, `assert_*`
helpers) and `flows_agent.sh` under a `flows_test.sh` orchestrator. The kernel runs the **real LLM
it ships with** — Ollama behind the `llm/*` natives — because that is the shipping configuration; there is
no fake model. Consequences:

- *Mechanism flows* (`bootstrap`, `recovery`, `drain`) touch no model and are fully deterministic.
- *LLM-in-the-loop flows* (`task`, `role_law`, `rating`, `maker`) assert robust **structural**
  properties — a task terminates as `answered`, the answer matches a shape (an ISO timestamp), a
  task settles under the role law, a dispatched call is rated, the maker's repetition trigger fires
  on real embeddings — not exact model text. They guard on `require_ollama` and *skip* (not fail)
  when the chat model is absent. Timeouts are generous (a real model takes tens of seconds/turn).
- The **budget and turn-cap rails are code invariants**, proven exhaustively by `go test ./agent`;
  a real model cannot be coerced into the pathological choices that would stage them on demand, so
  the flows assert the surviving invariant (spend never exceeds budget) rather than forcing the
  rail. Loop resilience matters more with a real model: a single invalid model selection is fed
  back for self-correction (`MaxDecideErrors` consecutive before the run fails), and model-output
  errors (`execution_failed`/`schema_violation`) are not retried as if transient.

```text
flow_bootstrap  daemon logs in and serves; no fake reply/publish rows in the catalog; restart      [no LLM]
flow_init       first launch creates agents/<login>/ with blank files and no config; task channel on [no LLM]
flow_no_session a login with no session, an unrecorded kernel, or a bare handle is refused         [no LLM]
flow_task_channel a task addressed to the agent is completed; one addressed to another is left     [no LLM]
flow_recovery   a mid-flight transcript at startup is marked interrupted, idempotently             [no LLM]
flow_drain      SIGTERM drains in-flight work, persists, exits 0                                    [no LLM]
flow_task       real task in via http; terminates answered; ISO answer; spend ≤ budget             [real LLM]
flow_role_law   a task addressed to the agent is picked up, worked, and completed under the role law   [real LLM]
flow_rating     a dispatched tool call is rated 0/1 after the task, held in the kernel ledger       [real LLM]
flow_maker      two similar requests trip the repetition trigger (real embeddings)                  [real LLM+embed]
flow_persona    an agent whose persona.md demands a distinctive marker produces it in the answer    [real LLM]
flow_named      serve alice@k and serve bob@k launch as distinct logins, each in its own home       [no LLM]
flow_memory     after job 1 an identity-tagged entry lands in memory.md; job 2 (across restart)
                loads it                                                                          [real LLM]
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
open, priced, rated action space that grows without the agent changing. Channels are an
abstraction because intake is genuinely undecided and will include Juice-independent surfaces;
binding the loop to any transport would repeat the mistake the kernel avoided by keeping
federation out of `Call()`. The experiment suite exists because this agent will be
developed over time: without a fixed task set and a committed metric series, "better" would be
anecdote; with them, it is a measurement.

**Relation to MCP.** An MCP server exposes typed, described, discoverable, remotely invokable
capabilities (tools) to an LLM harness. An agent that publishes actions satisfies that definition
exactly — action ↔ tool, lookup ↔ `tools/list`, `POST /v1/run` ↔ `tools/call`, grants ↔ per-server
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

The 0.1.1 additions follow the same discipline. Identity is a *directory* because what distinguishes
`research` from `triage` is data, not code — so a new agent is a new home and the binary stays
single, and multi-agent composition falls out of the shared-kernel identity model rather than a
supervisor. Persona and knowledge are *authored files* because tone and priming are what an operator
should own and version. Learned memory is a *summarised legible file*: the middle path between pi (a
human transcribes every lesson) and a vector store (recall no one can read), keeping accumulation
automatic and auditable, with embeddings held in reserve for demonstrated scale.
