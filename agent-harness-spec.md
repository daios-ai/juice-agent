# Agent-Side Harness Spec

Scope: the policy shell that turns the settled channel design
(`channel-design.md`) into the daemon's intake/egress + state. Grounded in the
current code; marks what **exists**, what to **refine**, and what to **add**. The
pure loop in `agent/` is unchanged.

## 1. Placement (requirements §2 package law)

- `agent/` — the pure loop. **Unchanged.** Types used here: `Task{ID, Channel,
  Budget, Messages}`, `Outcome{Status, Answer, Reason, Spent, Turns}`,
  `Message`, `Deps{Decider, Catalog, Runner, Transcript, Steering}`, and
  `Run(ctx, task, cfg, deps)`.
- `juice/` — kernel HTTP client. Provides `WaitingSteps` and `CompleteStep` (used
  via `stepAdapter`) and `Run` (for `Runner`).
- `cmd/juice-agent/` — the policy shell: `channels.go` (`Channel` iface, `Step`,
  `HTTP`), `runtime.go` (`Runtime` worker pool), `execute.go` (executor / loop
  wiring), `prompt.go` (system prompt §9), `rating.go` (§8), `transcript.go` (§7),
  `maker.go` (§8). **This is where all the work below lives.**

The channel model (recap): a message to the agent is a **step addressed to it**
(`required_caller = agent`); the reply is the **completion** of that step (its
onward action delivers); the **channel is `owner_handle`**. The agent registers
**nothing** — it is reached by steps, so startup does no bootstrap round-trip for
reachability. (A *bridge* registers its delivery action, not the agent.)

## 2. The channel seam (exists)

`Channel { Name() string; Start(ctx, submit func(agent.Task)) error;
Deliver(taskID string, out agent.Outcome); Steering(taskID string) []agent.Message;
Stop() }`. The **`Step`** channel is the primary kernel-native surface; `HTTP` and
`CLI` are secondary dev surfaces. Only enabled channels run; the step channel is
always present (§11 rule 6).

## 3. Intake — `Step.scan` (refine)

Per poll (`GET /v1/steps?status=waiting`; the token scopes results to the agent's
own steps, so the inbox *is* "addressed to me"):

For each step not already claimed:
1. **Channel & session.** `channel = owner_handle`; `session =
   (owner_handle, thread)` where `thread` is read from `partial_args` when the
   envelope carries one, else `owner_handle` alone.
2. **Envelope → Task.** Build `Task{ID: stepID, Channel: "step", Budget:
   default_budget, Messages: [envelope]}`, where the envelope message renders
   `partial_args` (exists: `promptFromStep`).
3. **Provenance** (kept out of `Task`, so the loop stays channel-blind, §10):
   record `taskID → {stepID, session, allowedInput}`. `allowedInput` = the step's
   `allowed_input` (the reply contract; §10), used at egress.
4. **Serialization** (add): if a task for `session` is active or queued, **hold**
   this step; submit it only after the active one settles, as a follow-up that
   **reopens the session transcript** (§9). Enforces one active task per
   conversation (the decided rule).
5. **Dedup** (refine): a persisted **claimed-set** keyed by `stepID` replaces the
   in-memory `seen` map, so a restart does not re-submit an in-flight step.
6. `submit(task)`.

## 4. Loop + prompt (exists; §9)

Before `agent.Run`, the policy shell prepends one `system` message (role line +
`persona.md` + `knowledge.md` + `memory.md` digest + operating contract; §9) and,
for a follow-up, reopens the session transcript. The loop is unchanged: it ends
when the model selects the reply tool (`Config.ReplyTool`), returning
`answered(reply.args)`. `reply.args` **is the reply payload**.

## 5. Egress — `Step.Deliver` (refine)

1. Resolve provenance by `taskID` → `stepID`, `allowedInput`.
2. Map `out.Answer` → completion args **matching `allowedInput`** (the onward
   action's schema minus the pre-bound keys). Default text channel: `{text: …}`.
   To let the model produce the right shape, surface `allowedInput` in the prompt
   / reply-tool schema for that task.
3. `CompleteStep(stepID, args)`. The step's **onward action delivers** (runs in the
   creator's process — the agent needs no permission on it).
4. On a **failed/interrupted** outcome: still complete the step (to release the
   sender's process) with an error/ack payload, so the exchange never dangles.
5. Mark the step `done` in the claimed-set; write the transcript **terminal
   record**. Order: **complete first, then finalize** (see recovery).

## 6. Durable harness state (add, in `data/`)

Only four things; everything else (funding, durability, recovery of steps,
attribution) is the kernel's:
- **Transcript** per task (JSONL, §7) — exists.
- **Conversation index**: `session → {latest transcript, open|closed, last
  activity}` — enables follow-up reopen and per-session serialization. **Add.**
- **Claimed-step set**: `stepID → {taskID, status: in-flight|done}` — idempotent
  intake across restart. **Add** (persist; replaces `seen`).
- **Per-session lock**: in-memory `session → active taskID`. **Add.**

## 7. Recovery (§5/§7)

The kernel keeps waiting steps and resets its own `running→waiting`. On startup:
- Mark every transcript lacking a terminal record `interrupted` (§7).
- Reconcile against a re-poll: a step still **waiting** whose `stepID` is
  claimed-in-flight with a non-terminal transcript → **redo** (re-submit; safe,
  because it was never delivered). A step the kernel now reports **done** is not
  re-polled — the completion already delivered.
- **No double-delivery** because egress order is *complete → finalize*: a crash
  between them leaves the step `done` (invisible to the next poll) and the
  transcript non-terminal (marked `interrupted`, harmless). The kernel's `done`
  state is the dedup guard; the claimed-set covers the read-but-not-completed
  window. Recovery is idempotent.

## 8. Concurrency (§3/§11)

`Runtime` is a bounded worker pool (`concurrency`); tasks across sessions run
concurrently, each loop serial. The **per-session lock** guarantees one active
task per conversation. `SIGTERM`/`SIGINT` drains: stop intake, finish in-flight
turns, persist, exit 0 (§11).

## 9. Steering (§4/§10)

v1: `Step.Steering` returns `nil`; multi-turn is served by **serialize +
transcript-reopen** (a later step in a session becomes a follow-up task that sees
the prior transcript). True mid-run injection (a same-session step acked and
injected before the next model call) is a documented later option, not v1 — the
honest friction from `channel-design.md`.

## 10. Config keys used (§11)

`default_budget` (task budget), `concurrency` (pool), step poll interval,
`candidate_limit`, `lookup_limit`, `max_turns`, `max_repeats`, `compact_bytes`,
`rate_tasks`, `memory.*`. No new keys are required by this spec.

## 11. Delta summary (what to build)

| Piece | Status |
|---|---|
| `Channel` iface, `Step`/`HTTP` channels, `Runtime` pool, `stepAdapter` | **exists** |
| Poll + dedup + submit + `Deliver`-by-completion | **exists** (refine below) |
| Session key `(owner_handle, thread)` + provenance record | **add** |
| Per-session lock + serialization + conversation index | **add** |
| Persisted claimed-set + recovery reconciliation | **refine** (`seen` → durable) |
| `Deliver` maps `out.Answer` → `allowed_input` shape | **refine** |
| Reply-tool schema surfaces `allowed_input` per task | **refine** (`prompt.go`/`tools.go`) |
| Pure loop `agent/` | **unchanged** |

## 12. Testing (§12)

Unit (offline, fakes; every file gets its `_test.go`):
- intake dedup + persisted claimed-set; session-key extraction from `partial_args`.
- per-session serialization (two steps, one session → serialized; two sessions →
  concurrent).
- `Deliver` → `CompleteStep` with `out.Answer` mapped to `allowed_input`
  (against a fake `StepSource`).
- recovery idempotence: complete-then-crash yields no re-delivery; read-then-crash
  redoes.
- follow-up reopens the session transcript.

Flow (against the compiled binary + a real kernel): the SMS-bridge round trip
(`bridge-sms-design.md`) — inbound step → agent completes → delivery action fires —
already exercised manually and should become a scripted flow.
