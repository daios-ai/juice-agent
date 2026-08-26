<p align="center"><img src="juice-logo.svg" alt="juice" width="96"></p>

# juice-agent

A Go daemon implementing a general task-solving agent whose entire tool set is a
[Juice](../juice) kernel's action catalog. It registers as a kernel *user*, discovers actions with
`sys/lookup`, reasons with `sys/llm/chat`, and dispatches every tool through `POST /v1/run` — so
it pays for what it runs, rates the tools it uses, and (optionally) publishes new actions to
automate recurring work. The loop is pi-style: a small hand-owned cycle that stops when the model
selects the terminal `reply` tool.

See `agent-design.md` for the full design and rationale.

## Prerequisites

1. **A running Juice kernel with a real LLM.** The kernel's `native.llm` must point at an Ollama
   instance with a chat model and an embed model. The agent is validated with the kernel default
   `gemma4:26b` (chat) + `nomic-embed-text` (embed); smaller models often can't tool-call reliably
   enough to drive the loop.
2. **An agent account on that kernel, funded with credits** (below).
3. Go 1.22+ to build.

## Build

```sh
make build        # -> ./juice-agent
make install      # -> $PREFIX/bin/juice-agent  (PREFIX ?= ~/.local; puts it on your PATH)
```

## Quickstart

With a Juice kernel already running (see Prerequisites), from zero to one answered task:

```sh
make install                                              # build + put juice-agent on PATH

# create and fund the agent's kernel account (as the kernel operator)
juice user create agent --password 's3cret-pass'   # prints a recovery phrase once — store it
juice admin deposit agent 100000

# run a single task to confirm it works — prints the outcome as JSON
AGENT_PASSWORD='s3cret-pass' juice-agent task agent "What is 2+2? Reply with just the number."
```

To then run it as a long-lived daemon that accepts work over channels, see *Running* and *Giving the
daemon work* below.

## One-time setup on the kernel

The daemon logs in as an ordinary user, so create and fund that user once (as the kernel operator):

```sh
# create the agent's account
juice user create agent --password 's3cret-pass'   # prints a recovery phrase once — store it

# fund it — every tool call the agent makes is prepaid from this balance
juice admin deposit agent 100000
```

The agent's `reply`/`publish`/`subtask` tools are plain-named harness affordances it intercepts
itself — they are **not** registered on the platform, so nothing else is needed kernel-side.

## Filesystem layout & configuration

Each agent lives in its own home under the juice-family root, `$JUICE_HOME` (default `~/.juice`):

```
$JUICE_HOME/agent/                 the juice-agent component root
├── config.json                    OPTIONAL host-wide defaults shared by every agent
└── agents/
    └── <handle>/                  e.g. research/  (handle = directory name)
        ├── config.json            this agent's config (auto-dumped; overrides shared defaults)
        ├── persona.md  knowledge.md  memory.md    hand-authored + learned files
        └── data/                  transcripts (one JSONL per task) + the maker index
```

The home is **fixed, never cwd-relative**, and is provisioned on first launch (mode 0700): blank
`persona.md`/`knowledge.md`/`memory.md` so you always have files to edit, plus a **standard
`config.json` dumped automatically** — with a step channel enabled — so a fresh agent is functional
out of the box (an existing file is never overwritten). Config is layered: the component-level
`config.json` supplies host-wide defaults (e.g. `server_url`), and the per-agent `config.json`
overrides them key-by-key. Every agent always has a step channel (the kernel-native intake), even if
config omits one. Secrets never live in config (see *Running* below). The handle is the positional
argument after the verb (else `AGENT_HANDLE`, else `agent`) — it names the directory, so it is not a
config key. Handles are bare: a value carrying `@` or `/` is rejected.

```json
{
  "server_url": "http://localhost:4040",
  "default_budget": 100,
  "max_turns": 32,
  "concurrency": 4,
  "lookup_limit": 8,
  "candidate_limit": 24,
  "compact_bytes": 262144,
  "rate_tasks": true,
  "maker": { "enabled": false, "min_repeats": 3, "window": 50, "budget": 40, "threshold": 0.85 },
  "channels": {
    "http": { "addr": "127.0.0.1:871" },
    "step": { "poll_seconds": 5 }
  }
}
```

- `default_budget` — credits a task may spend when its channel doesn't set one. The primary runaway
  rail: a task can never spend more than its budget.
- `max_turns` — secondary rail, bounding zero-price loops.
- `rate_tasks` — after each task, the agent grades every tool call it made (0/1 + a note) via
  `POST /v1/transactions/{id}/rate`; those ratings feed `sys/lookup` and future tool selection.
- `maker.enabled` — when on, a recurring request that no catalog action serves triggers the agent to
  synthesize and publish a WASM action automating it.
- `channels` — only the channels you list are enabled. Omit the block for `juice-agent task` (below),
  which needs no channel.

## Running

### As a daemon

```sh
juice-agent serve agent       # the verb first, then the agent's handle
juice-agent serve             # same, using the default handle (or AGENT_HANDLE)
```

With no `--password-stdin`, `AGENT_PASSWORD`, or `AGENT_PASSWORD_FILE` set, it prompts for the
password on the terminal (no echo). For unattended/systemd use, supply one of the non-interactive
sources instead:

```sh
AGENT_PASSWORD='s3cret-pass' juice-agent serve agent          # env
echo 's3cret-pass' | juice-agent serve agent --password-stdin  # pipe
```

The password precedence is `--password-stdin` → `AGENT_PASSWORD` → `AGENT_PASSWORD_FILE` →
interactive prompt. It authenticates, runs startup recovery, starts the configured channels and a
worker pool, and serves until `SIGTERM`/`SIGINT` — on which it drains in-flight tasks, persists, and
exits 0. Structured logs go to stderr.

### One-shot (no channel, no daemon)

```sh
AGENT_PASSWORD='s3cret-pass' juice-agent task agent "What is the current time? Reply with an ISO-8601 timestamp."
```

Runs one task and prints its outcome as JSON to stdout:

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

### Step channel (kernel-native)

Enable `channels.step`, and any user or action can hand the agent work by parking a *step* addressed
to the agent — no integration between them beyond the kernel. The simplest way is `sys/message`:

```sh
# as any funded kernel user
juice run sys/message '{"to":"agent","message":"Summarise the latest release notes."}'
```

The daemon polls for waiting steps addressed to it, works each as a task, and completes the step
with its answer — the settlement transaction obeys the kernel's role law (caller `agent`). This is
also how *other agents* (e.g. `chat`) delegate to it.

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

Add the agent to a room (as a room admin) and it handles its own access on first use — no other setup.
It remembers the recent conversation: each reply sees the last `context_messages` messages in the room
(default 20; set `-1` to answer each message with no memory of earlier ones). The optional `loop` block
keeps the agent from getting stuck replying back and forth with another bot: after `max_events` replies
to the same sender within `window_seconds`, it goes quiet for `cooldown_seconds`. Omit a number for the
default shown; omit the block to turn the limit off.

## Outcomes

Every task yields:

| field    | meaning                                                        |
| -------- | ------------------------------------------------------------- |
| `status` | `answered`, `failed`, or `interrupted`                         |
| `answer` | the reply object the model produced (for `answered`)          |
| `reason` | why it failed (`turn-cap`, `decide-error: …`) when not answered|
| `spent`  | credits spent — reconstructible from the kernel ledger        |
| `turns`  | model round-trips taken                                        |

## Observability

- **Logs** — structured JSON to stderr (`event`, `task_id`, `turn`, `action`, `tx_id`, …).
- **Transcripts** — one append-only JSONL per task under the agent's `data/` dir; the durable record and replay
  log. A task interrupted at shutdown is marked on the next start's recovery scan.
- **Ledger** — the kernel is the financial ground truth: `juice tx list` shows the agent's calls,
  their costs, and the ratings it left.

## Measuring performance across versions

The agent will change over time, so competence is tracked quantitatively, not by impression:

```sh
juice-agent bench agent bench/tasks.json 5 # 5 repetitions per task, against a live kernel + LLM
```

`bench/tasks.json` is a versioned suite of tasks with machine-checkable success criteria. Each run
writes a report (success rate, mean credits from the ledger, latency, turns) keyed by `git describe`
+ suite version under `bench/results/`, so performance is a diffable time series. The bench needs a
live kernel and real LLM, so it is excluded from `go test`.

## Tests

```sh
make test     # go test ./... — offline, deterministic, no kernel or LLM
make flows    # end-to-end against a real kernel + real Ollama (JUICE_BIN=../juice/juice)
```

Mechanism flows (bootstrap, recovery, drain) are deterministic; LLM flows (task, role law, rating,
maker) drive a real model and skip cleanly if Ollama or the chat model is unavailable.
