<p align="center"><img src="juice-logo.svg" alt="juice" width="96"></p>

# juice-agent

A Go daemon implementing a general task-solving agent whose entire tool set is a
[Juice](../juice) kernel's action catalog. It acts as one ordinary kernel account, discovers actions
with the kernel's `lookup`, reasons with its `llm/chat`, and dispatches every tool through
`POST /v1/run` — so it pays for what it runs, rates the tools it uses, and (optionally) publishes new
actions to automate recurring work. The loop is pi-style: a small hand-owned cycle that stops when the
model selects the terminal `reply` tool.

See `agent-design.md` for the full design and rationale.

## Prerequisites

1. **A running Juice kernel with a real LLM.** The agent is validated with the kernel's shipped
   Ollama endpoint, `gemma4:26b` (chat) and `nomic-embed-text` (embed); smaller models often cannot
   select tools reliably enough to drive the loop.
2. **The `juice` command line**, to record the kernel and log the agent's account in.
3. Go 1.22+ to build.

## Build

```sh
make build        # -> ./juice-agent
make install      # -> $JUICE_HOME/bin/juice-agent  (JUICE_HOME ?= ~/.juice)
```

## Quickstart

With a Juice kernel running at `http://localhost:4040` that calls itself `work`:

```sh
make install

juice kernel add http://localhost:4040            # record the kernel under its own name
juice user create agent@work --password '…'       # the agent's account; prints a recovery phrase once
juice admin user deposit agent@work 100 --ref initial-funding   # as the operator: fund it
juice auth login agent@work                       # the agent's session, kept by the juice CLI

juice-agent task agent@work "What is 2+2? Reply with just the number."
```

The last line runs one job and prints its outcome as JSON. To run the agent as a long-lived daemon
that accepts work, see *Running* and *Giving the daemon work* below.

## Identity and session

The agent is named by its **login**, `handle@kernel`: the account it acts as, on the kernel that
account lives on. The daemon holds no password and no token of its own. It reads the records the
`juice` command line keeps under `$JUICE_HOME/client/`: the kernel's address from `config.json`,
and the session from `credentials/<login>.json`, which it renews in place when the kernel asks.

- If the kernel is not recorded, the daemon stops and says to run `juice kernel add <url>`.
- If the login holds no session, or the kernel has revoked it, the daemon stops and says to run
  `juice auth login <login>`.
- If the session belongs to a different account than the login names (the account was renamed or
  recreated), the daemon stops rather than act as someone else.
- If, while the daemon runs, `credentials/<login>.json` is replaced by another account's session (a
  later `juice auth login` under the same label), the daemon keeps its own session and never
  adopts the new one; its kernel calls report that a restart is needed. Restart it to serve the new
  account.

The agent's `reply`, `publish` and `subtask` tools are handled inside the daemon and are not
registered on the kernel, so nothing else is needed kernel-side.

## Filesystem layout and configuration

Each agent has one home under the juice-family root, `$JUICE_HOME` (default `~/.juice`), named by its
login:

```
$JUICE_HOME/agents/<login>/        e.g. agents/agent@work/
├── config.json                    optional; absent means every default
├── persona.md  knowledge.md       written by you; read before every job
├── memory.md                      written by the agent; what it learned
└── data/                          transcripts (one JSONL per job), the maker index, channel state
```

The home is fixed, never relative to the working directory. On first launch the daemon creates it
(mode 0700) with blank `persona.md`, `knowledge.md` and `memory.md`; it never truncates a file and
never writes `config.json`. Each `memory.md` entry is headed with the kernel key and account it was
learned under, and only entries matching the current login's are loaded, so memory from another
kernel or account never reaches the prompt.

`config.json` is optional, and every key in it is optional. An unknown key or malformed JSON stops the
daemon at startup. The defaults:

```json
{
  "default_budget": 100,
  "max_turns": 32,
  "max_repeats": 3,
  "concurrency": 4,
  "candidate_limit": 24,
  "lookup_limit": 8,
  "compact_bytes": 262144,
  "memory_bytes": 65536,
  "rate_tasks": true,
  "maker": { "enabled": false, "min_repeats": 3, "window": 50, "budget": 40, "threshold": 0.85 },
  "channels": {
    "task": { "poll_seconds": 5 },
    "http": { "addr": "127.0.0.1:871" }
  }
}
```

- `default_budget` — base units a job may spend when its channel sets none. The primary runaway
  rail. Only actions offered to the model can be run, and only while their price fits what is left
  of the budget; an action whose terms change before it runs is refused by the kernel and must be
  read again. Actions that move value beyond their price (a `transfer`) are never offered. For
  actions on the agent's own kernel the budget is a hard ceiling. A call to an action on another
  kernel also pays that kernel's import fee and settles by lottery ticket: on average it costs its
  quoted price, but a winning ticket pays the full face value set by the kernel operator's
  `lottery`, which can take a job past its budget; the job then dispatches nothing further.
- `max_turns` — secondary rail, bounding zero-price loops.
- `memory_bytes` — size bound on `memory.md`; `0` turns learned memory off.
- `rate_tasks` — after each job, the agent grades every tool call it made (0/1 and a note) via
  `POST /v1/transactions/{id}/rate`.
- `maker.enabled` — when on, a recurring request that no catalog action serves triggers the agent to
  synthesize and publish a WASM action automating it.
- `channels` — the task channel is always on; `http` runs only when `addr` is set; `chat` only
  when `enabled` is true.

The only environment input is `JUICE_HOME`.

**Upgrading from the earlier layout.** An agent home at `$JUICE_HOME/agent/agents/<handle>/` is
not read any more. Move it to its login's home, and in its `config.json` remove `server_url`,
replace `memory` with `memory_bytes`, and rename `channels.step` to `channels.task`:

```sh
mv ~/.juice/agent/agents/agent ~/.juice/agents/agent@work
```

## Running

### As a daemon

```sh
juice-agent serve agent@work
```

It runs startup recovery, starts the configured channels and a worker pool, and serves until
`SIGTERM`/`SIGINT` — on which it drains in-flight jobs, persists, and exits 0. Structured logs go to
stderr. Several agents are several logins, each served by its own `juice-agent serve`.

### One-shot (no channel, no daemon)

```sh
juice-agent task agent@work "What is the current time? Reply with an ISO-8601 timestamp."
```

Runs one job and prints its outcome as JSON to stdout:

```json
{ "status": "answered", "answer": { "answer": "2026-07-10T00:50:45Z" }, "spent": 0, "turns": 2 }
```

## Giving the daemon work (channels)

Where tasks come from is a replaceable boundary. Three channels ship:

### HTTP channel (Juice-independent)

Enable `channels.http.addr`, then submit tasks over plain HTTP:

```sh
# submit — returns {"task_id":"http-1"}
curl -s -X POST http://127.0.0.1:871/task \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Translate \"good morning\" into Spanish.","budget":40}'

# poll — 202 {"status":"running"} until ready, then 200 with the outcome
curl -s http://127.0.0.1:871/task/http-1
```

`budget` is optional (falls back to `default_budget`).

### Task channel (kernel-native, always on)

Any user or action can hand the agent work by parking a kernel *task* addressed to the agent's
address — no integration between them beyond the kernel. The simplest way is the `message` native:

```sh
# as any funded kernel user
juice run sys@work/message '{"to":"agent@work","message":"Summarise the latest release notes."}'
```

The daemon polls for waiting tasks whose required caller is its login, works each as a job, and
completes the task with its answer; the settlement transaction names the agent as caller. This is
also how other agents delegate to it. If completing fails because the kernel is briefly
unreachable, the daemon resends the same answer at each later poll without redoing the job; if the
kernel refuses the answer, the task stays `waiting`, visible with `juice task list`.

A call the kernel reports as `timeout` has been dispatched and parked; the kernel settles it on its
own (`juice process show <process_id>`), so the daemon does not run it again. Any retried call is
sent under the same external key, so the kernel answers a repeat with the first run's outcome.

### Chat channel (juice-ui chat)

Enable `channels.chat` and the agent answers messages in juice-ui `chat` rooms:

```json
"chat": {
  "enabled": true,
  "poll_seconds": 2,
  "context_messages": 20,
  "loop": { "enabled": true, "max_events": 20, "window_seconds": 60, "cooldown_seconds": 60 }
}
```

The chat service is the account `chat` on the agent's kernel. Add the agent's address to a room (as a
room admin) and it handles its own access on first use — no other setup.
It remembers the recent conversation: each reply sees the last `context_messages` messages in the room
(default 20; set `-1` to answer each message with no memory of earlier ones). The optional `loop` block
keeps the agent from getting stuck replying back and forth with another bot: after `max_events` replies
to the same sender within `window_seconds`, it goes quiet for `cooldown_seconds`. Omit a number for the
default shown; omit the block to turn the limit off.

## Outcomes

Every job yields:

| field    | meaning                                                        |
| -------- | ------------------------------------------------------------- |
| `status` | `answered`, `failed`, or `interrupted`                         |
| `answer` | the reply object the model produced (for `answered`)          |
| `reason` | why it failed (`turn-cap`, `decide-error: …`) when not answered|
| `spent`  | base units the kernel charged (a parked call counts at its price) — reconstructible from the kernel ledger |
| `turns`  | model round-trips taken                                        |

## Observability

- **Logs** — structured JSON to stderr (`event`, `turn`, `action`, `tx_id`, …).
- **Transcripts** — one append-only JSONL per job under the agent's `data/` dir; the durable record
  and replay log. A job interrupted at shutdown is marked on the next start's recovery scan.
- **Ledger** — the kernel is the financial ground truth: `juice tx list` shows the agent's calls,
  their costs, and the ratings it left.

## Measuring performance across versions

The agent will change over time, so competence is tracked quantitatively, not by impression:

```sh
juice-agent bench agent@work bench/tasks.json 5   # 5 repetitions per task, against a live kernel + LLM
```

`bench/tasks.json` is a versioned suite of tasks with machine-checkable success criteria. Each run
writes a report (success rate, mean credits from the ledger, latency, turns) keyed by `git describe`
+ suite version under `bench/results/`, so performance is a diffable time series. The bench needs a
live kernel and real LLM, so it is excluded from `go test`.

## Tests

```sh
make test     # go test ./... — offline, deterministic, no kernel or LLM
make flows    # end-to-end against a real kernel + real Ollama (JUICE_SRC ?= ../juice)
```

The flows boot a kernel from the kernel checkout `JUICE_SRC` (its binary `JUICE_BIN`, default
`$JUICE_SRC/juice`) through that checkout's own flow harness. Mechanism flows (bootstrap, init,
no_session, recovery, drain, task_channel, named, session_identity) need no model; LLM flows (task, persona, rating,
role_law, memory, maker) drive a real model and skip cleanly if Ollama or the chat model is
unavailable.
