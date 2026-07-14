# Bridge Design — SMS (Twilio-shaped)

A worked example of a channel bridge for the design in `channel-design.md`. The
bridge lives entirely outside the agent; the agent binary is unchanged.

## Identity & components

The bridge is a Juice **user** `@smsbridge` (password account) plus a small HTTP
server. It owns exactly one action and holds one trace.

1. **Delivery action** `@smsbridge/deliver` — `kind=http`, `source` = the bridge's
   `/send` URL, permissive input schema, price 0 (or >0 to charge). This is the
   **onward action** of every inbound step: completing the step runs it and pushes
   the reply to SMS.
2. **Inbound webhook** `POST /inbound` — receives the provider's inbound
   (Twilio form fields `From`, `Body`, `MessageSid`). Applies an addressing policy
   (allowlist / always), dedups on `MessageSid`, then **mints a step to `@agent`**.
3. **Send endpoint** `POST /send` — the target of `@smsbridge/deliver`. The kernel
   calls it on completion with the merged args; it calls Twilio's send API.
4. **Seed outbox trace** — at startup, `run @sys/message {to:@smsbridge}` once and
   keep the returned `trace_id`; it funds `POST /v1/steps` for the life of the
   process (one open seed process, reused for all inbound).

## Inbound → step

For each inbound SMS the bridge mints (via `POST /v1/steps`, funded by the seed
trace):

```
action_id      = @smsbridge/deliver          # onward = the reply's delivery path
required_caller = @agent                      # addressed to the agent (kernel routes)
partial_args   = { "from":  "<sender phone>", # external identity
                   "incoming": "<text>",      # the request
                   "to":    "<sender phone>", # reply routing, pre-bound
                   "thread":"<sender phone>" } # SMS conversation key = the number
```

## Agent → completion → SMS

The agent (unchanged) polls, reads the envelope (channel = `owner_handle` =
`@smsbridge`, request = `incoming`), reasons, and **completes** the step with
`{ "text": "<reply>", "media"?: [...] }`. The kernel merges and dispatches
`@smsbridge/deliver` → the bridge's `/send`, which calls Twilio `sendMessage(to,
text)`. One step per exchange; the reply is the completion.

## Policies (bridge-owned, not the agent's)

- **Addressing** — the bridge decides *when* to mint (allowlist of numbers;
  group/short-code rules). `required_caller = @agent` is the kernel-level route.
- **Dedup** — on `MessageSid` (Twilio delivers at-least-once).
- **Funding** — the step parks `@smsbridge/deliver`'s price from the seed trace, so
  the bridge pays for delivery (set price >0 to bill the agent/caller instead).
- **Conversation** — `thread` = the sender's number; the agent keys the session on
  `(owner_handle, thread)`.

## What this shows

New provider = this file with Twilio swapped for another API. The agent never
changes; durability, funding, attribution, and the return route are the kernel's.
The only bridge-specific code is the two HTTP handlers and the provider SDK call.
