# Agent Harnesses: A Comparative Study of opencode, OpenClaw, and pi

*A synthesis of three ground-up reports (`opencode/report.md`, `openclaw/report.md`,
`pi/report.md`), written in plain language. It describes the common skeleton these systems share,
the pseudocode that drives them, and the ways they deliberately differ.*

---

## Abstract

A large language model (LLM) can do exactly one thing: it takes text and returns text. It cannot
open a file, run a command, or send a message. An **agent harness** is the program that stands
between a stateless model and the real world — it offers the model a menu of tools, executes the
tool the model asks for, feeds the result back, and repeats until the model is done. This report
examines three such harnesses that were built independently but converge on the same core design:
**opencode**, **OpenClaw**, and **pi**. We first describe the *canonical template* all three
implement — the loop, its stop condition, streaming, persistence, and context management. We then
analyse the *design axes* along which they diverge, profile each system, and catalogue the features
that are genuinely distinctive or unusual. The central finding is that the hard part of an agent
harness is not calling the model; it is owning the loop around the model — deciding when it is
finished, keeping its limited memory full of the right things, and staying safe while it runs
unattended. The three systems agree on the skeleton and disagree, revealingly, on exactly these
three questions.

---

## 1. Introduction

The three harnesses share an origin idea and even a house explanatory style, but they target
different products: opencode is a single-user coding tool, OpenClaw is an always-on chat assistant
that fans work out to helpers, and pi is a small reusable agent *core* that several front-ends
drive. Studying them side by side is useful precisely because they solve the *same* problem under
different constraints. Where they agree, we are probably looking at something essential to the
problem. Where they disagree, we are looking at a design decision with real trade-offs.

We use a few terms throughout, defined once here:

- **Tool** — a capability the harness exposes to the model (read a file, run a shell command, spawn
  a helper). The model does not run it; it *requests* it by name and arguments.
- **Turn / step** — one round-trip: the harness sends the whole conversation to the model, the
  model replies (possibly with tool requests), the harness processes the reply.
- **Stop reason / finish reason** — a label the model's reply carries saying why it stopped
  (`stop`, `toolUse`/`tool-calls`, `length`, `error`, `aborted`).
- **Context window** — the maximum text the model can consider at once. Everything sent must fit.
- **Compaction** — summarising older conversation so it keeps fitting in that window.
- **Subagent** — a helper the model can spawn; when present, it is a full second run of the same
  loop in its own session.

---

## 2. The canonical harness (the shared template)

Strip away the products and all three systems reduce to the same seven-part design. This is the
"basic template."

**(1) The model is a brilliant amnesiac.** It remembers nothing between calls. Therefore the
harness must re-send the entire relevant history on *every* turn. The harness is the model's hands
and its memory; the model only ever supplies the next decision.

**(2) The harness owns the loop by hand.** Popular libraries (notably the Vercel AI SDK) offer a
"run the tools for me and just give me the final answer" mode. **All three refuse it** for the outer
loop. Each calls the library (or its own transport) to perform exactly *one* model round-trip and
manages the repetition itself. The reason is the same in every report: only the harness can decide
"are we finished?", and the providers cannot be trusted to answer that question consistently
(see §3.1).

**(3) The loop.** In pseudocode, ignoring the product-specific extras, every one of them runs this:

```
loop(history):
  repeat:
    response = call_model_once(system_prompt, history, tools)   # exactly ONE round-trip, streamed
    append response to history

    if response is a failure or was aborted:
        stop

    tool_calls = the tool-request parts of response
    if tool_calls is empty:
        stop                     # <- the normal exit: the model asked for nothing more
    else:
        for each call:
            result = run_tool(call)          # the harness does the real work
            append result to history
    # otherwise: go round again, now with the tool results included
```

The single most important line is `if tool_calls is empty: stop`. Getting *that* condition right,
across providers that describe "done" inconsistently, is what §3.1 is about.

**(4) One turn is streamed into typed parts.** The model's reply does not arrive as one blob. It
streams in pieces, and each harness sorts those pieces into *typed* records — visible **text**,
hidden **reasoning/thinking**, and one or more **tool calls** (with their arguments streamed in).
This typing pays off twice: the same stream of events both drives the live screen and is saved to
disk, so there is no separate "now render it" code path, and a session can be perfectly replayed.

**(5) The transcript is persisted.** Everything — messages, tool results — is written down, so a run
survives a crash and can be resumed, observed, or replayed. This is the harness's short-term memory.

**(6) The context window is actively managed.** Real tasks outlast the window. When the
conversation nears the limit, the harness **compacts**: it replaces the older turns with an
LLM-written structured summary (goal, decisions, progress, next steps, relevant files) and keeps the
most recent turns verbatim. This is lossy on purpose — it trades detail for room to continue.

**(7) The system prompt and project files steer behaviour.** A block of instructions is placed
before the conversation, and project-level Markdown files (`AGENTS.md`, `CLAUDE.md`, …) are loaded
so the agent inherits local conventions without rediscovering them.

If you understand these seven parts, you understand the shared skeleton. Everything below is
variation on it.

---

## 3. Design axes: where they diverge

### 3.1 The stop rule (and the "provider lies" problem)

The naive loop trusts the model's finish reason: stop when it says `stop`. **All three reject
this**, because some providers report `stop` *while still asking for tools* — trusting them would
abandon pending tool calls. Their fixes differ in elegance:

- **opencode** puts the stop check at the *top* of the loop and makes it stricter than the finish
  reason: stop only if the last message finished normally *and* has no tool calls waiting.
- **OpenClaw** dispatches tools only when the stop reason is exactly `toolUse` *and* tool-call parts
  exist, and actively strips stray tool-call parts off any message that is not `toolUse`.
- **pi** is the most robust: it ignores the stop reason for this decision entirely and keys
  continuation off the *content* — "are there tool-call blocks?" — while also normalising
  `stop`-with-tools into `toolUse` at the provider boundary as a belt-and-braces measure.

All three arrive at the same place: **continuation is decided by whether real tool calls exist, not
by what the provider claims.**

### 3.2 State between turns: reload vs. keep-and-append

- **opencode** keeps *nothing* in memory between steps. It **reloads the whole conversation from a
  database every step**. This makes the loop a pure function of saved history — elegant, and
  trivially resumable, at the cost of per-step overhead.
- **OpenClaw and pi** keep the transcript in memory during a run and **append each finished message
  to a JSONL file** as it is produced. Lower overhead, still durable and replayable. pi adds a
  twist: its log is a *tree*, so sessions can fork and branch.

Both approaches are "honest under interruption" — they persist an explicit *aborted* marker — but
they draw the durability line in different places.

### 3.3 Memory beyond the conversation: legible vs. smart

This is the sharpest philosophical split.

- **opencode and pi** deliberately have **no "smart" memory** — no embeddings, no vector search, no
  semantic recall. Memory is (a) the conversation log, (b) the lossy compaction summary, and (c)
  hand-editable Markdown files found by *walking up the folder tree*. "Learning" is the agent
  editing one of those files, with the user's approval. Their stated value is **legibility over
  cleverness**: a human can open, read, diff, and correct the entire memory.
- **OpenClaw** refuses the trade-off. It keeps the legible file layer *and* adds a genuine semantic
  store — an embeddings-plus-full-text database with hybrid retrieval, exposed to the model as a
  `memory_search` tool, plus a "wiki" of durable facts. This is real "what do I know that's
  relevant?" recall, at the cost of more moving parts.

### 3.4 Delegation: subagents, or none

- **opencode** has a `task` tool that spawns a helper as a full child run of the same loop. It
  **blocks** (the parent waits), asks permission first, and denies the child its own `task` tool so
  recursion is bounded. The point is *context isolation*: a helper can burn forty steps and hand
  back a five-line summary.
- **OpenClaw** has `sessions_spawn`, but it is **asynchronous/fire-and-forget**: the parent gets an
  immediate "accepted" and keeps working; the child's result later arrives as a steering event.
  Depth and fan-out are bounded; leaf children are denied the spawn tools.
- **pi has no in-loop subagent tool at all.** Its multi-agent story lives *outside* the loop: a
  separate, experimental orchestrator process spawns whole `pi` processes and supervises them over a
  socket — with no depth or fan-out bounds, because there is no in-model recursion to bound.

### 3.5 Prompt construction: per-family files vs. one builder

- **opencode** keeps *one base prompt file per model family* (`anthropic.txt`, `gpt.txt`,
  `gemini.txt`, `beast.txt`, `default.txt`) and selects by matching the model's name. These differ
  in attitude and persistence, not ability.
- **OpenClaw** uses *one builder* split by a cache boundary, and lets a provider *contribute*
  variations (a stable prefix, a dynamic suffix, and overrides for three named sections).
- **pi** uses *one builder* with **no per-family variation at all**; provider differences are
  handled purely in the transport layer, never in the prompt text.

A related point: opencode stores each **tool's description as an editable `.txt` file**; pi defines
tool descriptions inline in code. Situational "you are in plan mode, read-only" steering is attached
to the last user message in opencode, and delivered as an injected message via a hook/extension in
pi and OpenClaw.

### 3.6 Provider portability

All three support many model vendors by **normalising every provider's stream into one shared event
vocabulary** (`text_start/delta/end`, `thinking_*`, `toolcall_*`, `done`, `error`) and reshaping the
tool schema per provider. The loop above the transport is therefore provider-agnostic. pi carries
this furthest, supporting on the order of thirty-plus providers behind a single event union.

### 3.7 Safety rails: how much do they distrust the model?

This axis orders the three neatly from most cautious to most trusting:

| Rail | opencode | OpenClaw | pi |
|------|----------|----------|-----|
| Step cap | Soft cap (nudges the model to wrap up at N) | None | **None** |
| Doom-loop guard (repeated identical tool calls) | Hard stop at 3 repeats | Opt-in circuit breaker (warn 10 / critical 20 / break 30) | **None** |
| Retry with backoff | Yes (2s, 4s, …) | Yes (3×, 2/4/8s) | Yes (3×, 2/4/8s) |
| Mid-run model fallback | — | Yes (candidate chain + auth cooldowns) | No (startup only) |
| Permission gate on risky tools | Yes | Yes (before-tool hooks) | Yes (before-tool gate) |

pi's lack of *any* runaway guard — no step cap, no repetition detector — is its most notable
omission: a stuck pi model is bounded only by its own stopping, the context window, retries, and the
user hitting escape.

### 3.8 Delivery surface

- **opencode** is a CLI/TUI for one user.
- **pi** is a reusable core with several front-ends (interactive TUI, print mode, an RPC mode, the
  orchestrator).
- **OpenClaw** is a long-running *service*: a gateway streams a run's output to chat channels
  (Telegram, Discord, web), the agent can talk back mid-run, and "heartbeats" let it wake itself.
  This is the one system built to be messaged by many people all day.

---

## 4. The three systems in brief

**opencode — the lean, database-backed coding loop.** Its signature move is reloading the entire
conversation from a database on every step, which makes the loop a pure function of saved history.
It is the most cautious of the three (soft step cap plus a hard doom-loop stop), keeps memory
deliberately dumb and legible, and ships blocking subagents for context isolation. Its own report
frames it against Addy Osmani's "Loop Engineering" essay: opencode provides all the parts for an
automated outer loop but ships, by default, as a single self-checking agent.

**OpenClaw — the always-on assistant as a service.** It takes the same hand-written core and wraps
it in a multi-channel service. Its distinctive features cluster around *being a service*:
asynchronous fire-and-forget subagents, a real semantic memory with vector search and a wiki,
provider-contributed prompt variations, model fallback chains, and a gateway that pipes runs into
chat apps. It is the most capable and the most complex.

**pi — the small, portable, embeddable core.** It isolates the loop in a tiny package with no
knowledge of disk, sessions, or UI, and wraps policy (retries, compaction, persistence, tools)
around it via callbacks. Its distinctive choices are a stop rule that trusts *content* over provider
claims (the most robust of the three), a session log shaped as a fork-able tree, no in-loop
subagents (an external orchestrator spawns whole processes instead), and — strikingly — no runaway
safety rails at all. It sides with opencode on legible, no-embeddings memory.

---

## 5. Distinctive and unusual features, catalogued

Features that stand out as unusual, one system at a time:

**opencode**
- *Reload-from-disk every step*: the loop holds no in-memory state; it is literally a function of the
  saved log. Unusually pure.
- *File snapshot at each step boundary*: a step-start record snapshots files on disk so a change can
  be reverted later.
- *Doom-loop hard stop*: three identical tool calls in a row and it stops to ask the human — the
  strongest runaway protection of the three.
- *Prompt-per-model-family as editable text files*, including one tuned for "maximum autonomy."

**OpenClaw**
- *Fire-and-forget subagents*: spawning returns immediately; completions arrive later as steering
  events, so the parent can fan out and keep working.
- *Genuine semantic memory*: embeddings + vector search + a fact wiki, the only one of the three with
  "what's relevant?" recall.
- *Self-waking heartbeats and a mid-run `message` tool*: the agent can speak before it finishes and
  can wake itself — features that only make sense for an always-on service.
- *Provider prompt contributions* and *model fallback chains* for resilience across vendors.
- *Terminal-outcome normalisation*: a run's messy end-state (timeout, cancel, abort, error) is mapped
  to a single, "sticky" outcome so late cleanup can't misreport it.

**pi**
- *Continuation decided by content blocks, not stop reason*: the most provider-robust stop rule.
- *Truncation guard*: if a reply is cut off by the output limit, its (possibly half-formed) tool
  calls are refused rather than executed — a subtle correctness feature the others don't emphasise.
- *Session as a tree*: transcripts fork and branch, not just append.
- *No in-loop subagents*: delegation is pushed out to a separate orchestrator that spawns whole OS
  processes.
- *No runaway rails*: uniquely, neither a step cap nor a doom-loop detector.
- *Smallest reusable core*: the loop is a standalone package multiple surfaces embed.

---

## 6. Comparative summary

| Dimension | opencode | OpenClaw | pi |
|-----------|----------|----------|-----|
| Owns the loop by hand | Yes | Yes | Yes |
| Stop rule | Stricter than finish reason | `toolUse` + strips stray tool calls | Keyed off **content blocks**; providers normalised |
| State between turns | **Reload from DB every step** | In memory + append JSONL | In memory + append JSONL (as a **tree**) |
| Transcript store | Database | Append-only JSONL | Append-only JSONL |
| Compaction | Yes (structured summary) | Yes (state-preserving template) | Yes (7-section template) |
| "Smart" memory | **None** (by design) | **Embeddings + vector + wiki** | **None** (by design) |
| Project files found by | Walking up the tree (+ adjacent files) | Flat load of a fixed set from root | Walking up the tree (+ global) |
| Subagents | `task`, **blocks** | `sessions_spawn`, **async** | **None in-loop** (external processes) |
| Step cap | Soft (nudge at N) | None | **None** |
| Doom-loop guard | Hard stop at 3 | Opt-in breaker at 30 | **None** |
| Mid-run model fallback | — | Yes | No |
| Prompts | One file per family | One builder + provider contributions | **One builder**, no family variation |
| Provider portability | Normalised menu | Normalised event vocabulary | Normalised event vocabulary (~34 providers) |
| Delivery surface | CLI/TUI, one user | Gateway → chat channels, many users | Reusable core, many front-ends |
| Guiding value | Legibility, caution | Capability, always-on service | Portability, minimal core |

---

## 7. Discussion

Three tensions organise the whole comparison.

**Purity versus overhead (state).** opencode's reload-every-step makes its loop a mathematical
function of history — the easiest of the three to reason about and resume — but pays for it on every
turn. OpenClaw and pi keep state in memory and append to disk, accepting a slightly less pure model
for lower cost. There is no free lunch; each simply chose which property to prize.

**Legibility versus recall (memory).** opencode and pi bet that a memory a human can fully read in an
afternoon is worth more than one that can recall across thousands of notes, and they explicitly
forgo embeddings. OpenClaw bets the opposite. This is not a maturity gap — it is a genuine
disagreement about whether an agent should have an opaque, compounding memory at all. The answer
plausibly depends on the product: a coding tool working in one repository benefits less from
cross-session semantic recall than a personal assistant accumulating facts about a user over months.

**Trust versus supervision (safety).** The safety-rail table (§3.7) is really a spectrum of how much
each system distrusts its own model. opencode distrusts it most (soft cap + hard doom-loop stop);
OpenClaw offers a strong but opt-in circuit breaker; pi ships none. This tracks their deployment
models: a supervised CLI can afford to trust more than an unattended service — yet it is pi, the
embeddable core, that trusts most, effectively delegating runaway protection to whichever front-end
embeds it. That is a defensible layering decision, but it means "is this safe to run unattended?"
has no answer inside pi's core; it must be answered by the surface around it.

A cross-cutting observation ties the three together: **the difficulty of an agent harness is not the
model call.** Calling the model once is a solved, library-provided problem, and all three even use
similar transports. The engineering — and all the interesting disagreement — lives in the loop
*around* the call: deciding when the work is done despite unreliable provider signals, keeping a
finite context window full of the right summary, choosing what to remember across sessions, bounding
a model that might otherwise spin forever, and deciding whether to fan work out to helpers. These
are the questions the three systems answer differently, and they are the questions that matter.

---

## 8. Conclusion

opencode, OpenClaw, and pi are three independent answers to one question: how do you make a
stateless text predictor do sustained, real work safely? They agree completely on the skeleton — a
hand-owned loop that streams one round-trip at a time, sorts the reply into typed parts, runs the
requested tools, feeds results back, persists everything, and compacts when the window fills. They
diverge on exactly the three hard problems the skeleton exposes: **when to stop** (content over
provider claims, with varying strictness), **what to remember** (legible files versus semantic
recall), and **how much to trust the model unsupervised** (from a hard doom-loop stop to no rails at
all). Read together, the three make a compact map of the design space: opencode marks the
lean-and-cautious corner, OpenClaw the capable-service corner, and pi the minimal-portable-core
corner. The common template is now well understood; the frontier is in the choices around it.
