#!/usr/bin/env bash
# flows_test.sh — run the juice-agent flow suite end to end.
#
#   make flows            (builds binaries, then runs this)
#   JUICE_BIN=... ./flows/flows_test.sh [flow_name ...]
#
# With no arguments, runs every flow. Named arguments run only those flows.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/lib.sh"
# juice-agent flows: the daemon driven end to end against a real kernel + real
# Ollama. Sourced by flows_test.sh (which provides lib.sh).
#
# The budget and turn-cap rails are code invariants proven exhaustively by
# `go test ./agent` — a real model cannot be coerced into the pathological
# choices that would exercise them on demand, so the flows assert the invariant
# that survives regardless (spend never exceeds budget) rather than staging it.

# ---- mechanism flows (no LLM: fully deterministic) ------------------------

flow_bootstrap() {
    echo "=== FLOW bootstrap ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir" || { fail bootstrap.kernel "no kernel"; return; }
    create_agent
    start_agentd "$dir" || { fail bootstrap.agentd "no daemon"; return; }
    ok bootstrap.logged_in_and_serving

    # Harness tools are NOT registered on the platform — the catalog stays clean.
    local acts; acts=$(kj action list --all)
    assert_not_contains bootstrap.no_fake_reply   "@agent/reply"   "$acts"
    assert_not_contains bootstrap.no_fake_publish "@agent/publish" "$acts"

    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" || { fail bootstrap.restart "no daemon"; return; }
    ok bootstrap.restart
}

flow_recovery() {
    echo "=== FLOW recovery ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local adata; adata="$(agent_home "$dir")/data"
    mkdir -p "$adata"
    printf '%s\n%s\n' \
        '{"role":"user","content":"half-done task"}' \
        '{"role":"assistant","action":"@sys/time"}' > "$adata/orphan.jsonl"

    start_agentd "$dir" || { fail recovery.agentd "no daemon"; return; }
    if grep -q '"event":"recovery"' "$dir/agentd.log" && grep -q 'orphan' "$dir/agentd.log"; then
        ok recovery.marks_interrupted
    else
        fail recovery.marks_interrupted "no recovery log for orphan"
    fi
    assert_contains recovery.marker '"kind":"interrupted"' "$(tail -1 "$adata/orphan.jsonl")"
}

flow_init() {
    echo "=== FLOW init (home under agents/; blank files + auto-dumped config w/ step) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home "$dir")
    # Shared server_url via the component config; NO per-agent config, so the
    # daemon must auto-dump one (with a step channel) on first launch.
    mkdir -p "$dir/agent"
    echo "{ \"server_url\": \"$KURL\", \"memory\": {\"enabled\": false} }" > "$dir/agent/config.json"

    JUICE_HOME="$dir" AGENT_PASSWORD=agent-pass "$AGENTD" @agent serve >"$dir/agentd.log" 2>&1 &
    local pid=$!; track "$pid"
    wait_line "$dir/agentd.log" '"event":"\(serving\)"' >/dev/null || { fail init.serving "no daemon"; cat "$dir/agentd.log" >&2; return; }

    # The home lives under agents/, with blank persona/knowledge/memory + data/.
    local missing=""
    for f in persona.md knowledge.md memory.md; do
        [ -f "$home/$f" ] || missing="$missing $f"
        [ -s "$home/$f" ] && missing="$missing $f(nonempty)"
    done
    [ -d "$home/data" ] || missing="$missing data/"
    [ -z "$missing" ] && ok init.blank_files_created || fail init.blank_files_created "missing/nonblank:$missing"

    # A standard config.json was auto-dumped, and it carries a step channel.
    if [ -f "$home/config.json" ] && grep -q '"step"' "$home/config.json"; then
        ok init.config_dumped_with_step
    else
        fail init.config_dumped_with_step "no auto-dumped config.json with a step channel"
    fi

    # An operator edit must survive a restart (init never truncates).
    printf 'INIT-MARKER-9Z\n' > "$home/persona.md"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    JUICE_HOME="$dir" AGENT_PASSWORD=agent-pass "$AGENTD" @agent serve >"$dir/agentd2.log" 2>&1 &
    local pid2=$!; track "$pid2"
    wait_line "$dir/agentd2.log" '"event":"\(serving\)"' >/dev/null || { fail init.restart "no daemon"; return; }
    grep -q 'INIT-MARKER-9Z' "$home/persona.md" \
        && ok init.non_truncating \
        || fail init.non_truncating "persona.md truncated on restart"
}

flow_drain() {
    echo "=== FLOW drain ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail drain.agentd "no daemon"; return; }

    kill -TERM "$AGENTD_PID" 2>/dev/null
    local rc; wait "$AGENTD_PID"; rc=$?
    assert_eq drain.exit_zero 0 "$rc"
    grep -q '"event":"stopped"' "$dir/agentd.log" && ok drain.logged_stopped || fail drain.logged_stopped "no stopped event"
}

# ---- LLM-in-the-loop flows (real model; robust structural assertions) ------

flow_task() {
    echo "=== FLOW task (real LLM: answer + ledger) ==="
    require_ollama || { skip task.answered "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail task.agentd "no daemon"; return; }

    local id out; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    assert_nonempty task.task_id "$id"
    out=$(await_http "$id") || { fail task.await "no outcome (model too slow?)"; return; }
    assert_eq    task.answered "answered" "$(echo "$out" | jfield status)"
    assert_match task.iso_answer '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}' "$(echo "$out" | afield answer)"
    # Budget invariant: whatever the model chose, spend never exceeds the budget.
    assert_le    task.within_budget "$(echo "$out" | jfield spent)" 100
}

flow_role_law() {
    echo "=== FLOW role_law (task via step, real LLM) ==="
    require_ollama || { skip role_law.step_done "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail role_law.agentd "no daemon"; return; }

    echo '{"to":"@agent","message":"Please reply that you acknowledge this."}' > "$dir/msg.json"
    local sid; sid=$(kj run @sys/message "@$dir/msg.json" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['step_id'])" 2>/dev/null)
    assert_nonempty role_law.step_created "$sid"

    local deadline=$(( $(date +%s) + 300 )) st=""
    while [ "$(date +%s)" -lt "$deadline" ]; do
        st=$(kj step show "$sid" | jfield status)
        [ "$st" = "done" ] && break
        sleep 0.5
    done
    assert_eq role_law.step_done "done" "$st"

    local txid; txid=$(kj step show "$sid" | jfield tx_id)
    if [ -n "$txid" ]; then
        assert_eq role_law.caller_is_agent "@agent" "$(kj tx show "$txid" | jfield caller_handle)"
    else
        fail role_law.tx "step had no tx_id"
    fi
}

flow_step() {
    echo "=== FLOW step (intake + egress mechanics; no LLM required) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir" || { fail step.kernel "no kernel"; return; }
    create_agent
    create_user @other other-pass
    start_agentd "$dir" || { fail step.agentd "no daemon"; return; }

    # A step addressed to @agent is picked up and COMPLETED by the daemon even
    # with no LLM: a failed loop still acks the step (waiting -> done). The
    # reply/ack rides in the completion transaction's args, readable by the sender.
    echo '{"to":"@agent","message":"deterministic ping"}' > "$dir/m.json"
    local sid; sid=$(kj run @sys/message "@$dir/m.json" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['step_id'])" 2>/dev/null)
    assert_nonempty step.created "$sid"
    local st="" deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        st=$(kj step show "$sid" | jfield status); [ "$st" = "done" ] && break; sleep 0.5
    done
    assert_eq step.completed_by_daemon done "$st"
    local tx args; tx=$(kj step show "$sid" | jfield tx_id)
    args=$(kj tx show "$tx" | python3 -c 'import sys,json; print(json.dumps(json.load(sys.stdin).get("args",{})))' 2>/dev/null)
    assert_contains step.reply_in_tx_args "deterministic ping" "$args"

    # A step addressed to @other is not the agent's work: the daemon leaves it
    # waiting (only steps whose required caller is the agent are its to do).
    echo '{"to":"@other","message":"not for the agent"}' > "$dir/o.json"
    local oid; oid=$(kj run @sys/message "@$dir/o.json" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['step_id'])" 2>/dev/null)
    sleep 3 # several daemon polls
    assert_eq step.foreign_untouched waiting "$(kj step show "$oid" | jfield status)"
}

flow_rating() {
    echo "=== FLOW rating (real LLM: agent rates its tool use) ==="
    require_ollama || { skip rating.tx_rated "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail rating.agentd "no daemon"; return; }

    # A time question makes the model dispatch @sys/time, giving the post-task
    # rating pass a real call to grade.
    local id out; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    out=$(await_http "$id") || { fail rating.await "no outcome"; return; }
    assert_eq rating.answered "answered" "$(echo "$out" | jfield status)"

    sleep 2 # the rating pass runs just after the outcome is delivered
    local rated; rated=$(kj tx list | python3 -c "
import sys,json
for t in json.load(sys.stdin):
    if t.get('action_name')=='time' and t.get('rating') is not None:
        print(t['rating'].get('value')); break
" 2>/dev/null)
    assert_nonempty rating.tx_rated "$rated"
}

flow_maker() {
    echo "=== FLOW maker (repetition trigger, real embeddings) ==="
    require_ollama || { skip maker.trigger "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" '"maker": {"enabled": true, "min_repeats": 2, "threshold": 0.9}' \
        || { fail maker.agentd "no daemon"; return; }

    local id
    id=$(submit_http "Translate the phrase into pirate speak." 100); await_http "$id" >/dev/null
    id=$(submit_http "Translate the phrase into pirate speak." 100); await_http "$id" >/dev/null
    sleep 2
    grep -q '"event":"maker_trigger"' "$dir/agentd.log" \
        && ok maker.trigger_fires \
        || fail maker.trigger_fires "no maker_trigger after two similar tasks"
}

# ---- 0.1.1: persona, named agents, memory -----------------------------------

flow_persona() {
    echo "=== FLOW persona (persona.md reaches the system prompt) ==="
    require_ollama || { skip persona.injected "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    # persona.md lives in the agent home, carrying a distinctive marker. The home
    # must exist before the daemon starts; start_agentd creates it via mkdir.
    local home; home=$(agent_home "$dir"); mkdir -p "$home"
    printf 'Persona note PERSONA-MARKER-7Q: be terse.\n' > "$home/persona.md"
    start_agentd "$dir" || { fail persona.agentd "no daemon"; return; }

    local id out; id=$(submit_http "In one short sentence, what is the capital of France?" 100)
    out=$(await_http "$id") || { fail persona.await "no outcome"; return; }
    assert_eq persona.answered "answered" "$(echo "$out" | jfield status)"
    # Structural: the persona must appear in the task's injected system prompt.
    # (Whether the model obeys a style rule is the model's business, not the daemon's.)
    if grep -q 'PERSONA-MARKER-7Q' "$ADATA/$id.jsonl" 2>/dev/null && grep -q '# Persona' "$ADATA/$id.jsonl" 2>/dev/null; then
        ok persona.injected
    else
        fail persona.injected "persona.md not present in the task's system prompt"
    fi
}

flow_named() {
    echo "=== FLOW named (multiple named agents) ==="  # mechanism only, no LLM
    local dir; dir=$(new_dir)
    start_kernel "$dir"
    create_user @alice alice-pass
    create_user @bob bob-pass
    # One component-wide config.json ($JUICE_HOME/agent/config.json) supplies the
    # shared server_url; each named agent inherits it and gets its own home.
    mkdir -p "$dir/agent"
    echo "{ \"server_url\": \"$KURL\", \"memory\": {\"enabled\": false} }" > "$dir/agent/config.json"

    JUICE_HOME="$dir" AGENT_PASSWORD=alice-pass "$AGENTD" @alice serve >"$dir/alice.log" 2>&1 &
    track $!
    JUICE_HOME="$dir" AGENT_PASSWORD=bob-pass "$AGENTD" @bob serve >"$dir/bob.log" 2>&1 &
    track $!
    wait_line "$dir/alice.log" '"event":"\(serving\)"' >/dev/null
    wait_line "$dir/bob.log"   '"event":"\(serving\)"' >/dev/null

    # Each named agent logs in and serves under its own handle (distinct users on
    # one kernel; no fake per-agent actions are registered).
    assert_contains named.alice_identity '"handle":"@alice"' "$(cat "$dir/alice.log")"
    assert_contains named.bob_identity   '"handle":"@bob"'   "$(cat "$dir/bob.log")"
    local acts; acts=$(kj action list --all)
    assert_not_contains named.no_fake_actions "@alice/reply" "$acts"
}

flow_memory() {
    echo "=== FLOW memory (learned cross-task memory) ==="
    require_ollama || { skip memory.persists "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" '"memory": {"enabled": true, "memory_bytes": 8192}' || { fail memory.agentd "no daemon"; return; }

    # Task 1: a memory entry must be written to the home.
    local mem; mem="$(agent_home "$dir")/memory.md"
    local id; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    await_http "$id" >/dev/null || { fail memory.await1 "no outcome"; return; }
    sleep 2
    [ -s "$mem" ] && ok memory.entry_written || fail memory.entry_written "memory.md empty"

    # Restart the daemon (same home) → memory persists and loads into the next task.
    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" '"memory": {"enabled": true, "memory_bytes": 8192}' || { fail memory.restart "no daemon"; return; }
    local id2; id2=$(submit_http "What is 2 plus 2?" 100)
    await_http "$id2" >/dev/null || { fail memory.await2 "no outcome"; return; }

    # Task 2's transcript must carry the injected memory in its system prompt.
    if grep -q '"kind":"system"' "$ADATA/$id2.jsonl" 2>/dev/null && grep -q '# Memory' "$ADATA/$id2.jsonl" 2>/dev/null; then
        ok memory.loaded_next_task
    else
        fail memory.loaded_next_task "task 2 transcript lacks injected memory"
    fi
}

# Mechanism flows (no LLM) run first and fast; LLM flows (real Ollama) follow.
ALL=(flow_bootstrap flow_init flow_recovery flow_drain flow_step flow_named
     flow_task flow_persona flow_rating flow_role_law flow_memory flow_maker)

if [ "$#" -gt 0 ]; then
    RUN=("$@")
else
    RUN=("${ALL[@]}")
fi

for f in "${RUN[@]}"; do
    "$f"
done

echo
echo "==========================================="
echo "  PASS: $PASS   FAIL: $FAIL   SKIP: $SKIP"
echo "==========================================="
[ "$FAIL" -eq 0 ]
