#!/usr/bin/env bash
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

    local acts; acts=$(kj action list --all)
    assert_contains bootstrap.reply_registered   "@agent/reply"   "$acts"
    assert_contains bootstrap.publish_registered "@agent/publish" "$acts"

    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" || { fail bootstrap.restart "no daemon"; return; }
    ok bootstrap.idempotent_restart
}

flow_recovery() {
    echo "=== FLOW recovery ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    mkdir -p "$dir/agent-data"
    printf '%s\n%s\n' \
        '{"role":"user","content":"half-done task"}' \
        '{"role":"assistant","action":"@sys/time"}' > "$dir/agent-data/orphan.jsonl"

    start_agentd "$dir" || { fail recovery.agentd "no daemon"; return; }
    if grep -q '"event":"recovery"' "$dir/agentd.log" && grep -q 'orphan' "$dir/agentd.log"; then
        ok recovery.marks_interrupted
    else
        fail recovery.marks_interrupted "no recovery log for orphan"
    fi
    assert_contains recovery.marker '"kind":"interrupted"' "$(tail -1 "$dir/agent-data/orphan.jsonl")"
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

# ---- 0.2: persona, named agents, memory -----------------------------------

flow_persona() {
    echo "=== FLOW persona (persona.md reaches the system prompt) ==="
    require_ollama || { skip persona.injected "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    # persona.md lives in the home (AGENT_HOME=$dir), carrying a distinctive marker.
    printf 'Persona note PERSONA-MARKER-7Q: be terse.\n' > "$dir/persona.md"
    start_agentd "$dir" || { fail persona.agentd "no daemon"; return; }

    local id out; id=$(submit_http "In one short sentence, what is the capital of France?" 100)
    out=$(await_http "$id") || { fail persona.await "no outcome"; return; }
    assert_eq persona.answered "answered" "$(echo "$out" | jfield status)"
    # Structural: the persona must appear in the task's injected system prompt.
    # (Whether the model obeys a style rule is the model's business, not the daemon's.)
    if grep -q 'PERSONA-MARKER-7Q' "$dir/agent-data/$id.jsonl" 2>/dev/null && grep -q '# Persona' "$dir/agent-data/$id.jsonl" 2>/dev/null; then
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
    mkdir -p "$dir/alice" "$dir/bob"
    echo "{ \"server_url\": \"$KURL\", \"handle\": \"@alice\", \"memory\": {\"enabled\": false} }" > "$dir/alice/agent.json"
    echo "{ \"server_url\": \"$KURL\", \"handle\": \"@bob\",   \"memory\": {\"enabled\": false} }" > "$dir/bob/agent.json"

    AGENT_HOME="$dir/alice" AGENT_PASSWORD=alice-pass "$AGENTD" @alice serve >"$dir/alice.log" 2>&1 &
    track $!
    AGENT_HOME="$dir/bob" AGENT_PASSWORD=bob-pass "$AGENTD" @bob serve >"$dir/bob.log" 2>&1 &
    track $!
    wait_line "$dir/alice.log" '"event":"serving"' >/dev/null
    wait_line "$dir/bob.log"   '"event":"serving"' >/dev/null

    # Each named agent registered its own terminal action under its own handle.
    local acts; acts=$(kj action list --all)
    assert_contains named.alice_reply "@alice/reply" "$acts"
    assert_contains named.bob_reply   "@bob/reply"   "$acts"
    assert_contains named.identity    '"handle":"@alice"' "$(cat "$dir/alice.log")"
}

flow_memory() {
    echo "=== FLOW memory (learned cross-task memory) ==="
    require_ollama || { skip memory.persists "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" '"memory": {"enabled": true, "memory_bytes": 8192}' || { fail memory.agentd "no daemon"; return; }

    # Task 1: a memory entry must be written to the home.
    local id; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    await_http "$id" >/dev/null || { fail memory.await1 "no outcome"; return; }
    sleep 2
    [ -s "$dir/memory.md" ] && ok memory.entry_written || fail memory.entry_written "memory.md empty"

    # Restart the daemon (same home) → memory persists and loads into the next task.
    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" '"memory": {"enabled": true, "memory_bytes": 8192}' || { fail memory.restart "no daemon"; return; }
    local id2; id2=$(submit_http "What is 2 plus 2?" 100)
    await_http "$id2" >/dev/null || { fail memory.await2 "no outcome"; return; }

    # Task 2's transcript must carry the injected memory in its system prompt.
    if grep -q '"kind":"system"' "$dir/agent-data/$id2.jsonl" 2>/dev/null && grep -q '# Memory' "$dir/agent-data/$id2.jsonl" 2>/dev/null; then
        ok memory.loaded_next_task
    else
        fail memory.loaded_next_task "task 2 transcript lacks injected memory"
    fi
}
