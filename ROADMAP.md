# Juice Agent Roadmap

Status: requested features, not yet scheduled
Codename: `juice-agent`

Each entry records a requested feature: its goal, the proposed change, and the trade-offs, so the
decision can be revisited without reconstructing the discussion. Entries are removed when
implemented (the normative spec then lives in `requirements.md`) or rejected (with a note).

---

## 1. Relevance-based memory recall ("bubble up similar situations")

Status: accepted, awaiting implementation
Requested: 2026-07-12
Origin: review of openclaw's `active-memory` push-recall design

### Goal

The agent should spontaneously surface memories of *similar past situations* when it starts a new
task — and only those. Today `Memory.Digest()` injects the **entire** `memory.md` into every system
prompt regardless of relevance. As the agent accumulates experience, every task carries more noise,
the context grows, and the useful memory is diluted. The goal is recall that scales: relevant
memories surface, irrelevant ones stay out.

### The change

Three steps, all in the policy layer (`cmd/juice-agent/prompt.go`); the pure loop is untouched.

1. **Embed the task.** At task start, embed the incoming prompt with one `sys/llm/embed` call.
2. **Rank the memory entries.** `memory.md` is already a list of dated `## <stamp> — <taskID>`
   entries; each is a recall unit. Rank them by cosine similarity to the task embedding. Entry
   embeddings are cached in a small sidecar file (keyed by entry hash and embed model) so only new
   entries are ever embedded; the sidecar is derived and rebuildable — `memory.md` remains the sole
   source of truth.
3. **Inject only the winners.** The top-k entries above a similarity floor go into the `# Memory`
   section. Below the floor, nothing is injected. If the embedder is unavailable, fall back to
   today's behavior (inject the recent tail), so the feature can never fail a task.

The embedding-plus-cosine pattern already exists in the codebase — the maker's repetition trigger
uses it — so this reuses proven code rather than adding a dependency. It is also the escalation
`agent-design.md` §5c explicitly reserved: "escalate to a `sys/llm/embed` index only if a readable
file stops scaling."

Recall runs at task start and, optionally, when a subtask opens; per-turn recall adds cost for
marginal benefit and is deferred.

### Pros

- **Relevance replaces bulk.** The prompt carries memories that match the situation, not everything
  ever learned. Recall quality improves precisely as memory grows — the case where today's design
  degrades.
- **Cheap.** One embed per task, one embed per new memory entry; brute-force cosine over even
  thousands of entries is negligible. No vector database, no new service.
- **Legibility preserved.** `memory.md` stays a human-readable, hand-editable file. The index is a
  disposable cache; delete it and it rebuilds.
- **Bounded context.** Top-k with a floor caps the memory section's size, protecting the token
  budget regardless of how large memory becomes.
- **Graceful degradation.** Without the embedder, behavior reverts to the current one — no new
  failure mode.

### Cons

- **Recall can miss.** Semantic similarity is imperfect; a relevant memory phrased very differently
  may fall below the floor, whereas today's whole-file injection technically never misses. The floor
  and k trade recall for precision and need tuning.
- **Two artifacts to keep consistent.** The sidecar index can go stale relative to hand-edits of
  `memory.md`. Hash-keying makes this self-healing, but it is still added machinery over "one file."
- **Model dependence.** Changing the embed model invalidates cached vectors; the cache must be keyed
  by model to force re-embedding.
- **New tests and a flow required.** Per project law, the change lands only with unit tests
  (ranking, cache invalidation, fallback) and a flow exercising real embeddings — a real but modest
  cost, comparable to `flow_maker`.

### Recommendation

The trade is favorable: the main risk (missed recall) is bounded by the fallback and tunable
thresholds, while the main gain (relevance that scales) fixes a degradation already latent in the
design.
