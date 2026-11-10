<p align="center"><img src="juice-logo.svg" alt="juice" width="96"></p>

# juice-agent

`juice-agent` is a Go daemon that answers tasks through a [Juice](https://github.com/daios-ai/juice) kernel. It runs as
an ordinary kernel account, discovers tools in the kernel's action catalog, and pays for the
ones it uses. Conversation history is stored locally so the agent can continue earlier work.

The kernel controls who can send the agent work. You manage its account, approve senders,
and write its instructions in `agent.md`.

## Requirements

- A Juice kernel at contract 0.16 or later, with a language model available through its
  reasoning action. The project uses the kernel's shipped Ollama endpoint with `gemma4:26b`.
- The `juice` command line, configured to reach that kernel.
- Go 1.22.2 or later and Make to build from source.

## Installation

```sh
make install
```

This builds the executable, installs it in `$JUICE_HOME/bin`, defaulting to `~/.juice/bin`, and
writes its configuration schema to `$JUICE_HOME/schemas/juice-agent.schema.json`.
Add that directory to your `PATH` if necessary. To build without installing, run `make build`;
the executable is written to `./juice-agent`.

## Quickstart

These examples use a kernel named `work` at `http://localhost:4040`, an agent account named
`agent@work`, and a sender named `alice@work`. Substitute your own kernel and account names.

Register the kernel, create the accounts, and log in to each:

```sh
juice kernel add http://localhost:4040
juice user create agent@work --password 'choose-an-agent-password'
juice user create alice@work --password 'choose-an-alice-password'
juice auth login agent@work
juice auth login alice@work
```

Account creation prints a recovery phrase once. Keep it with the account's credentials.
The agent needs funds for its tool calls, and Alice needs funds for any charges when sending work.
Have the kernel operator fund the accounts. For a local development kernel, the operator can use:

```sh
juice admin user deposit agent@work 100 --ref agent-initial-funding --as sys@work
juice admin user deposit alice@work 100 --ref alice-initial-funding --as sys@work
```

Start the daemon in one terminal:

```sh
juice-agent serve agent@work
```

In another terminal, send it a message:

```sh
juice run sys@work/message '{"to":"agent@work","message":"What is 2+2? Reply with just the number."}' --as alice@work
```

The result includes a `task_id`. Alice's first task waits until you approve her as a sender:

```sh
juice user requesters --as agent@work
juice user approve alice@work --as agent@work
```

The daemon picks up the task on a subsequent poll. Use the returned task ID to check its status:

```sh
juice task show <task_id> --as alice@work
```

When its status is `done`, read the transaction identified by its `tx_id`:

```sh
juice tx show <tx_id> --as alice@work
```

The reply is in the transaction's `args.answer` field. Press Ctrl-C in the daemon's terminal to
stop it after its current jobs finish.

## Instructions and configuration

Each agent has a directory named after its login under `$JUICE_HOME/agents/`:

```text
~/.juice/agents/agent@work/
├── agent.md       # your instructions and background knowledge
├── config.json    # configuration, written at first launch
└── personas/      # conversation histories
```

On first launch, the daemon creates the directory, a blank `agent.md`, `personas/`, and
`config.json` with every setting at its default. It preserves existing files. Edit `agent.md` to
describe the agent's role, instructions, and relevant background. Changes take effect at the next
job.

Installation and every start write `$JUICE_HOME/schemas/juice-agent.schema.json`, which describes
every setting and its default. The `$schema` key at the top of `config.json` points to it, so an
editor that supports JSON Schema completes and checks the keys as you type. A key you remove takes
its default. Unknown keys (key names are case-sensitive), `null` values, values of the wrong type,
and invalid JSON prevent startup, and the error names the key. The file written for an account on
`work` is:

```json
{
  "$schema": "../../schemas/juice-agent.schema.json",
  "reason": "sys@work/llm/chat",
  "action_lookup": "sys@work/lookup",
  "default_budget": 100,
  "max_turns": 32,
  "concurrency": 4,
  "poll_seconds": 5,
  "compact_bytes": 262144,
  "rate_tasks": true
}
```

| Setting | Purpose |
| --- | --- |
| `reason` | Reasoning action. |
| `action_lookup` | Action the agent uses to find tools. |
| `default_budget` | Spending budget per job, in base units, model calls included. |
| `max_turns` | Maximum reasoning turns per job. |
| `concurrency` | Number of conversations the daemon can serve at once. |
| `poll_seconds` | Interval between checks for waiting tasks. |
| `compact_bytes` | Conversation size in bytes that triggers summarisation of older turns. |
| `rate_tasks` | Whether to rate tool calls after each job. |

`reason` and `action_lookup` have no default: the first launch writes the kernel's own actions for
the account's kernel, and if the file lacks either key, or `$schema`, the daemon adds it at startup,
leaving every other setting as you wrote it. Other tools are found during execution.

### Replacing `reason` or `action_lookup`

Either can be set to another action, as long as that action takes and returns what the agent uses:

| Key | It must accept | It must return |
| --- | --- | --- |
| `reason` | `messages`: a list of `{role, content}` strings, with roles `system`, `user` and `assistant` | `choices`: a list whose first item's `message.content` is the reply text |
| `action_lookup` | `query` (string) and `limit` (integer) | `results`: a list of `{action, description, input_schema}` |

The action may accept and return more; the agent ignores the rest. It must not limit the length or
pattern of what the agent sends, and its `limit` must allow 1 to 10. Fields the agent reads must not
be declared as possibly `null`. At startup the daemon reads both actions' schemas from the kernel and
stops if one falls short, naming the key, the address and the field:

```
error: config.json: action_lookup = sys@work/time: input.properties.limit: missing (want integer)
```

Compare an action's schemas with `juice action show <action>`. The full contracts are in
`requirements.md` §11. Each job reads both actions again, so a change of price or schema applies
from the next job, with no restart; a job whose action no longer fits fails with reason
`affordance: …`.
`JUICE_HOME` is the daemon's only environment setting; its default is `~/.juice`.

### Spending and ratings

Every paid call a job makes comes out of its budget: the tools the model chooses, and the
agent's own calls of `reason` and `action_lookup` (each turn, the opening search, summarising a long
conversation, and rating). A model's price is set by the kernel's operator and may be `0`; a
replacement action costs what its owner charges. Before each call the agent checks its price
against what the job has left. A turn that does not fit ends the job with reason `budget`; a
search, summary or rating that does not fit is skipped. The price is the one the job read when it
started: if it changed since, the kernel refuses the call without charging, and a turn so refused
ends the job with reason `terms-changed`.

For local actions the budget is a ceiling. Remote calls settle through the kernel's lottery system:
a winning ticket can cost more than the quoted expected price and take the job over budget. The
agent then makes no further calls. Actions that transfer value beyond their price are excluded.

The turn limit also bounds loops using free tools.

With `rate_tasks` enabled, the agent records a binary rating and a short note for its tool calls.
These notes are visible to anyone who can read the action. Failed calls receive the fixed note
`call failed`. For successful calls, the model is instructed to discuss tool performance without
including request content or identifying people or accounts. This reduces disclosure risk but does
not guarantee privacy. Set `rate_tasks` to `false` to disable the rating pass.

## Sending work and managing access

Users, agents, and applications send work by creating a kernel task addressed to the agent.
Direct messages use `sys@work/message`, as in the quickstart. Applications can create tasks through
the kernel API; no separate chat or HTTP intake is required in the daemon.

The kernel withholds tasks from unapproved requesters. Manage access using the agent's account:

```sh
juice user requesters --as agent@work
juice user approve alice@work --as agent@work
juice user revoke alice@work --as agent@work
juice user block alice@work --as agent@work
```

Approval admits the requester's waiting tasks. Revocation withholds their tasks again. Blocking
also declines their waiting tasks and refuses new ones. The daemon does not approve requesters.

For a direct message, the requester is the sender. When an application creates a task, the requester
is the application's owner. Approving that owner therefore admits work from users of the application.

### Conversations

Conversation histories are kept separately under `personas/`. The daemon processes one task at a
time for each conversation, while other conversations can run concurrently.

- Direct messages from the same sender through the same creating action share a conversation.
- An application can supply `thread_id` in a task's arguments to group messages into a conversation.
  Threads are scoped to the requester and creating action, so identical thread names from different
  applications do not share history.
- A relayed task without `thread_id` gets its own conversation.

The model sees who sent each message, including the intermediary when an application relays it.
Only the current conversation's history is loaded for a job; `agent.md` supplies shared instructions
for all conversations. An application is responsible for assigning its thread identifiers correctly.

### Completion and failures

The daemon completes a task with the job's answer. For direct messages, the sender reads that answer
from the completion transaction. Completion runs as the requester and uses the task's reserved
funds; the agent pays for the tools it calls while working.

If delivery fails transiently, the daemon retains the answer and retries at later polls without
rerunning the job. A rejected completion can leave the task `waiting`; inspect it with
`juice task show`. A failed job also completes its task, and explains itself: the daemon asks the
model once what it attempted, what went wrong and what would be needed, and delivers that text as
the answer beside the status and reason (`turn-cap`, `budget`, `repeat-loop`, …). The explanation
is one more reasoning call, paid from the job's budget; when the budget cannot pay for it, or the
call fails, the completion carries the status and reason alone.

A task still waiting after a daemon restart may be executed again. A tool call the kernel reports
as `timeout` without a transaction is still running elsewhere and will settle later; the daemon
refuses to run the identical call again while it is pending. A `timeout` that reports a
transaction is a settled failure, and the model may retry the call. Inspect a pending call's
process with `juice process show <process_id>`.

## Running a single job

To run a job directly and print its outcome as JSON:

```sh
juice-agent task agent@work "What is the current time? Reply with an ISO-8601 timestamp."
```

Successive invocations use the same conversation, `cli`, separate from tasks received by the daemon.
A successful outcome looks like:

```json
{ "status": "answered", "answer": { "answer": "2026-07-10T00:50:45Z" }, "spent": 0, "turns": 2 }
```

| Field | Meaning |
| --- | --- |
| `status` | `answered`, `failed`, or `interrupted`. |
| `answer` | Reply object for an answered job. |
| `reason` | Failure class of a failed or interrupted job (`turn-cap`, `budget`, `repeat-loop`, …). |
| `explanation` | The model's account of a failed job: what it tried, what went wrong, what would be needed. Absent when the budget could not pay for it. |
| `spent` | Tool charges in base units; an unsettled call counts at its quoted price. |
| `turns` | Number of reasoning turns. |

## Operation and troubleshooting

Run one `juice-agent serve <login>` process per agent. The daemon enforces this: while it serves,
it holds `$JUICE_HOME/agents/<login>/serve.lock`, which contains its process ID, and a second
`serve` of the same login exits with "another juice-agent is already serving <login>". The file
remains after the daemon exits, so its process ID may be stale. A program that stops the daemon by
that ID must first check that it cannot take the lock itself. Otherwise, stop the daemon with
Ctrl-C or through its supervisor. The daemon writes structured JSON logs to
stderr. On SIGTERM or SIGINT it stops taking new work and lets queued and running jobs finish.
At startup, it marks interrupted jobs in the stored histories.

The daemon uses the sessions maintained by the `juice` CLI under `$JUICE_HOME/client/` and renews
them when needed. It does not need the account's password in configuration.

| Problem | What to check |
| --- | --- |
| Kernel is not recorded | Run `juice kernel add <url>`. |
| Session is missing or revoked | Run `juice auth login <login>`. |
| "another juice-agent is already serving" | A daemon for that login is running. Stop it before starting another. |
| Session belongs to another account | Log in to the intended account and restart the daemon. It will not adopt another account's session while running. |
| Task is waiting | Check requester approval, the task's required caller, and the daemon's logs for execution or completion errors. |

Logs include job, persona, action, and transaction identifiers where applicable. The `persona_opened`
event reports how much earlier history a job loaded. Conversation files under `personas/` retain
the local record; `juice tx list --as <login>` shows the kernel's record of calls, charges, and ratings.

## Upgrading from 0.6

At its first start, the new version writes `$schema`, `reason`, and `action_lookup` into an existing
`config.json` that lacks them, using the kernel's own actions, and leaves your other settings as
they are. A `null` value or a key written in a different case, such as `MAX_TURNS`, was accepted
before and now prevents startup; correct it to the key's exact name or remove it.

## Upgrading from 0.3

The daemon no longer reads `persona.md`, `knowledge.md`, `memory.md`, or `data/`. It also rejects the
retired configuration keys `channels`, `memory_bytes`, `maker`, `max_repeats`, `candidate_limit`, and
`lookup_limit`. Remove those keys from `config.json` before starting the new version.

Back up the agent directory first. Merge the instructions and knowledge you want to retain into
`agent.md`, including useful entries from `memory.md`. Keep the old files until you have checked the
new setup, then archive or remove them. Existing `agent.md` content should be preserved during the
merge. The former HTTP and chat intakes have been replaced by kernel tasks.

## Development

The [design document](agent-design.md) explains the architecture;
[requirements.md](requirements.md) defines the expected behaviour.

### Tests

```sh
make test     # unit tests; no kernel or model required
make flows    # integration flows against a real kernel
```

Flows use the kernel checkout at `JUICE_SRC` (default `../juice`) and its compiled binary at
`JUICE_BIN` (default `$JUICE_SRC/juice`). The kernel must support contract 0.16 or later. Flows that
exercise model behaviour use the real Ollama endpoint and skip when Ollama or the chat model is
unavailable; the remaining flows check daemon and kernel behaviour without a model.

### Benchmarks

```sh
juice-agent bench agent@work bench/tasks.json 5   # or: make bench LOGIN=agent@work
```

This runs five repetitions of each task in the versioned suite against a live kernel and model.
Each repetition uses a fresh conversation. Reports include success rate, spending, latency, and
turn count, and are written to stdout and `bench/results/`, keyed by agent and suite version.
The agent version comes from `git describe`, so tag releases to keep report names clean; a report
named `*.local.json` is ignored by git. Rerunning the same version pair replaces its report.
Benchmarks are separate from the unit tests.

### Versions

The current version and the history of changes are recorded at the top of
[requirements.md](requirements.md).
