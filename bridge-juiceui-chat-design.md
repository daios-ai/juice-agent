# Bridge Design — juice-ui "Chat with the agent"

Spec for a chat-with-`@agent` surface in **juice-ui** (`../juice-ui`). This is a
spec only — do not edit juice-ui from this repo. It follows the settled channel
design (`channel-design.md`): a message to the agent is a **step addressed to
`@agent`**; the reply is that step's **completion**. The agent side needs nothing
new beyond the harness delta already implemented here (the step channel picks up
the UI user's steps).

## Shape

A new app in the `apps/` plugin seam (`apps/apps.go:83` `Deps{Client, Sessions,
RenderPage, RenderFragment, LoadAppState/SaveAppState, …}`), modeled on the
working `apps/chat/` reference. Call it `apps/agent/`. Transport = **`@sys/message`
pull**: the signed-in user *is* the sender (`owner_handle`), so no bridge user, no
funding trace, and no delivery action are needed — the UI reads the reply from the
step's completion.

## Flow

1. **Send** — on submit, run `@sys/message {to: "@agent", message: <text>}` as the
   signed-in user, reusing the existing per-user run path (`apps/chat/chat.go:616`
   `run()`; `server/funded.go:123`). The run reply's `result.step_id` is the
   outstanding step to watch. Persist `{step_id, text}` in the conversation store.
   - Multi-thread option: put a stable `thread` in a richer envelope so the agent
     keys distinct conversations; with plain `@sys/message` (one `message` field)
     there is one conversation per `(user, @agent)`, which is fine for v1.
2. **Watch** — poll `GetStep(step_id)` (`client/client.go:794`) until
   `Status == "done"`, reusing the htmx interval pattern already used by the chat
   view (`apps/chat/templates/chat_conversation.html:37`, every 4s) and the steps
   badge (`server/handlers.go:805`). A `waiting`/`running` step renders as
   "thinking…".
3. **Read the reply** — when `done`, read the completing transaction (`step.tx_id`,
   `client/client.go:286`) via `GetTransaction` (`:754`). The agent's reply is in
   the transaction's **args** under `answer` (the harness completes an
   `@sys/message`→`@sys/sink` step with `{answer: <text>}`, which merges into the
   step's `partial_args` and lands in `args_json`). Render it as the agent's turn.
4. **Render** — a conversation view of `(user message, agent reply)` pairs, adapted
   from `apps/chat/templates/chat_conversation.html` + `_chat_messages.html`, fed
   from the app's own store (`LoadAppState/SaveAppState`) rather than the chat
   daemon.

## Why this is small

Everything except three items already exists in juice-ui:

| Need | Status in juice-ui | Reference |
|---|---|---|
| Run an action as the signed-in user (`@sys/message`) | present | `apps/chat/chat.go:616`, `server/funded.go:123` |
| Step create/get (if a richer envelope is ever used) | present | `client/client.go:794,810` |
| Get a transaction | present (but see gap) | `client/client.go:754` |
| Interval polling (htmx) | present | `chat_conversation.html:37`, `handlers.go:805` |
| Conversation UI to adapt | present | `apps/chat/templates/` |
| Plugin seam + per-user token exec | present | `apps/apps.go`, `session.Manager.Do` |

## Gaps to fill in juice-ui

1. **Expose the transaction `args`.** `GetTransaction` today surfaces
   `Transaction.Result` (`client/client.go:230`), but the agent's reply is in the
   transaction **args** (the completion input merged with `partial_args`). Add an
   `Args` field to the decoded `Transaction` (it is already in the kernel's
   `args_json`). This is the one client change required.
2. **The watch-for-completion loop.** Poll `GetStep(step_id)` → on `done`, fetch
   the tx and read `args.answer`. New handler + template, ~the shape of the steps
   detail view (`handlers.go:678-776`).
3. **The conversation store.** Keep `[]{role, text, step_id, status}` per user via
   `LoadAppState/SaveAppState` (`apps/apps.go`), so a refresh restores history.

**No funding trace, no delivery action, no WebSocket/SSE** — `@sys/message`
self-funds (price 0) and htmx polling is the existing live-update mechanism. The
push alternative (mint a step whose onward is a UI delivery action) is possible
but needs a funding trace and a hosted endpoint, both currently absent — not worth
it for a UI that already polls.

## What the agent side already does (no change needed)

The harness step channel (this repo, `cmd/juice-agent/channels.go`) already:
- picks up the UI user's `@sys/message` step (`required_caller = @agent`),
- keys the channel/session off `owner_handle` (the UI user),
- runs the loop, and **completes** the step with the reply, which the UI reads
  from the completion transaction's args.

So the juice-ui chat channel is entirely a juice-ui-side surface over the existing
kernel primitives; the agent treats it identically to SMS, Slack, or another
agent.
