# Channel Mechanism — Design Notes (settled & verified)

Status: **settled**, every claim driven on a throwaway kernel (2026-07-14). No
agent code written. Supersedes all earlier framings in this file's history.

## 1. Purpose

A channel mechanism for `juice-agent` that is **simple, robust, and general** —
the intake/egress boundary — with no WASM and no per-platform code in the agent.

## 2. The model — the agent is a user, reached by continuations

`@agent` is a **user** (the agent's handle, kernel §1), **not** an action — you do
not `run @agent`. A message to the agent is a **step (continuation) addressed to
it** (`required_caller = @agent`). A message *back* is the **completion** of that
step: completing runs the step's **onward action**, which is the reply's delivery
path. This is what a continuation is — "when you have the result, run this with it."

- **Intake** — poll `GET /v1/steps?status=waiting`. The kernel surfaces only steps
  whose `required_caller` is the agent, so the inbox *is* "messages addressed to
  me." The **channel** is `owner_handle` (the sender/bridge); the **envelope**
  (request + reply routing) is `partial_args`.
- **Egress** — the agent **completes the step** with its reply; the step's onward
  action runs **in the creator's process** and delivers. For a platform bridge the
  onward action is the bridge's push action (e.g. `@slackbridge/post`); for
  agent↔agent it can be `@sys/sink` and the peer polls a fresh reply message.
  `@sys/message → @sys/sink` is just the degenerate "notify, deliver nothing" case.
- **Funding** — reasoning is free (`@sys/llm/*` = 0); **delivery is funded by the
  step's parked price** (the sender/bridge prepaid it when minting); any **paid
  tools** the agent calls while reasoning are self-funded `run`s from its wallet
  ("agent pays its wallet" applies to tools only).

No trace, no capability, and no WASM in the agent. The one place a trace is needed
is on the *sender* side: minting a step with a real onward action uses
`POST /v1/steps`, funded from a **seed "outbox" trace** (one self-`@sys/message`
holds a process open; verified many steps off one seed). `@sys/message` is the
no-trace shortcut, at the cost of a sink onward.

## 3. Capabilities and how each is met (all verified unless noted)

| # | Capability | How | Verified |
|---|---|---|---|
| C1 | Reach the agent async | a step with `required_caller = @agent` | ✓ |
| C2 | Return the result | **complete the step** → its onward action delivers, in the creator's process | ✓ (SMS + Slack push) |
| C3 | Identity + conversation | channel = `owner_handle`; session = `(owner_handle, thread)` from the envelope | ✓ |
| C4 | Addressing ("is it for me?") | `required_caller = @agent` (kernel routes); the **bridge** decides *when* to address (mention/DM/policy) | ✓ (a step to `@other` was invisible to the agent) |
| C5 | Funding | reasoning free; delivery = step's parked price; paid tools self-funded | ✓ / by construction |
| C6 | Delegate / hand off | mint a step to a peer (`@sys/message` for the poll-back case) | ✓ |
| C7 | External platforms | a **bridge** (a Juice user + a delivery action) relays; zero agent code | ✓ (SMS + Slack) |
| C8 | Rich media | refs (URLs / blob ids) in `partial_args` (in) and completion args (out); agent passes through, interprets by calling a tool | ✓ (media round-tripped in the SMS send) |
| C9 | Robustness | kernel persists/recovers steps; agent dedups by step id, serializes per conversation, acks everything | kernel side proven (§5); harness side is code |
| C10 | Proactive | agent-initiated message = mint a step (self-funded) | by construction |

## 4. The mechanism (minimal)

```
loop:
  for each waiting step addressed to @agent, not already claimed:
    channel = step.owner_handle          # who/what platform (a bridge, a peer)
    session = (channel, thread?)         # thread decoded from the envelope
    if a task for `session` is running: hold        # one active task per conversation
    claim(step.id)                       # idempotent across restart
    request = step.partial_args          # the message + reply routing
    answer  = run the loop               # @sys/llm/chat (free) + run paid tools (self-funded)
    complete(step, answer)               # onward action delivers the reply (in the creator's process)
```

Durable harness state, and only this: **transcript** per session (reopened on a
follow-up), **claimed-step set** (dedup), **per-conversation lock** (one active
task). Funding, durability, recovery, and role-law attribution are the kernel's.
No plugin adapters, no per-platform objects, no WASM.

## 5. Bridges — how generality is achieved (verified)

A platform is reached through a **bridge**: a Juice user (`@smsbridge`,
`@slackbridge`, …) that owns a **delivery action** (`kind=http` → its server → the
platform API) and holds a seed outbox trace.

- **Inbound**: on a platform event (and only when the agent should respond —
  mention/DM/policy), the bridge mints a step to `@agent` via `POST /v1/steps`,
  with `action_id = @bridge/deliver` (the onward = delivery action) and
  `partial_args` = the request + pre-bound routing (channel/thread/recipient).
- **Egress**: the agent completes the step with its reply; `@bridge/deliver` runs
  in the bridge's process and pushes to the platform. The agent never calls the
  bridge's private action directly (that 403s) — it only completes the pre-wired
  continuation. More general *and* more secure.

**A new provider is a new bridge user + one delivery action — the agent binary
never changes.** Verified: SMS and Slack bridges both round-tripped (correct
channel/thread routing, rich media) through an unmodified agent loop; a step to a
non-agent was invisible; two same-thread turns correlated.

## 6. Honest frictions (accepted, not hidden)

- **Sender needs a funding trace** to mint a step with a real onward action —
  obtained by holding one open "outbox" seed. A bridge concern, not the agent's.
- **Envelope schema** — `partial_args` must fit the onward action's schema, so
  delivery actions use a permissive schema (they carry the request echo alongside
  the routing). Slightly loose; workable.
- **Mutual-ack discipline** — every message is a step that keeps the sender's
  process open until completed; both sides must ack what they read (no funds leak,
  price 0, but unacked steps accumulate open processes).
- **Agent-side dedup + per-conversation serialization** are harness code.

## 7. Why the trace-capability is not used by the agent

The capability (kernel §9, shipped/verified) lets a `kind=http` action compose on
the *caller's* dime. The agent would need it only to be a caller-prepaid **paid
action** — which "agent pays its own wallet" rejects. As a self-funded async
poller the agent needs no capability, trace, or composing action. The capability
stays valuable for **bridges and tools** (any `kind=http` action that subcalls or
mints steps) and is the escape hatch if a future variant makes the agent a
bounded-price paid service.

## 8. Corrections from this file's history

- `@agent` is a **user, not an action**; reached by continuations, not called.
- Egress is **completion of a step whose onward is the channel's delivery action**
  (runs in the creator's process), **not** the agent running a delivery action
  (private → 403).
- Channel identity is **`owner_handle`**, not `created_by`.
- `@sys/message` is the **degenerate** notify-with-sink case, not the mechanism.
- No WASM anywhere.
- Settles the four earlier decisions: agent pays wallet (tools) ✓; step channel is
  intake ✓; reply = completion (onward delivers) ✓; one active task per
  conversation ✓.

## Appendix — verified kernel/ABI facts (builds 2026-07-14 14:36 & 17:28)

- Egress via completion pushed real deliveries to SMS/Slack recorder endpoints with
  correct routing and rich media; the agent needed no permission on the bridges'
  private delivery actions.
- Addressing: a step to `@other` was invisible to `@agent` (kernel routes by
  `required_caller`).
- Rich steps mint without WASM via `POST /v1/steps` off a seed outbox trace
  (many steps per seed).
- Steps expose `owner_handle` (channel key), `partial_args`, `action` (onward),
  `required_caller`, `allowed_input`. `created_by` is not reliable identity.
- Trace-capability composition works for `kind=http` actions (not needed by the
  agent, §7).
- `step_create`/step-create accept `@handle` and `@owner/name`; tx list & show
  both return party handles; `juice health` uses its own `--url` (default `:4040`).
