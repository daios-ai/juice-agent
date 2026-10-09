#!/usr/bin/env bash
# flows_test.sh — run the juice-agent flow suite end to end.
#
#   make flows            (builds binaries, then runs this)
#   JUICE_SRC=../juice JUICE_BIN=... ./flows/flows_test.sh [flow_name ...]
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
    assert_not_contains bootstrap.no_fake_reply   "$(login agent)/reply"   "$acts"
    assert_not_contains bootstrap.no_fake_publish "$(login agent)/publish" "$acts"

    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" || { fail bootstrap.restart "no daemon"; return; }
    ok bootstrap.restart
}

flow_recovery() {
    echo "=== FLOW recovery ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local adata; adata="$(agent_home)/data"
    mkdir -p "$adata"
    printf '%s\n%s\n' \
        '{"role":"user","content":"half-done job"}' \
        '{"role":"assistant","action":"sys@k/time"}' > "$adata/orphan.jsonl"

    start_agentd "$dir" || { fail recovery.agentd "no daemon"; return; }
    if grep -q '"event":"recovery"' "$dir/agentd.log" && grep -q 'orphan' "$dir/agentd.log"; then
        ok recovery.marks_interrupted
    else
        fail recovery.marks_interrupted "no recovery log for orphan"
    fi
    assert_contains recovery.marker '"kind":"interrupted"' "$(tail -1 "$adata/orphan.jsonl")"
}

flow_init() {
    echo "=== FLOW init (home under agents/<login>; blank files; no config written) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home)

    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd.log" 2>&1 &
    local pid=$!; track_pid "$pid"
    wait_line "$dir/agentd.log" '"event":"\(serving\)"' >/dev/null || { fail init.serving "no daemon"; cat "$dir/agentd.log" >&2; return; }

    local missing=""
    for f in persona.md knowledge.md memory.md; do
        [ -f "$home/$f" ] || missing="$missing $f"
        [ -s "$home/$f" ] && missing="$missing $f(nonempty)"
    done
    [ -d "$home/data" ] || missing="$missing data/"
    [ -z "$missing" ] && ok init.blank_files_created || fail init.blank_files_created "missing/nonblank:$missing"
    [ -e "$home/config.json" ] && fail init.no_config_written "the daemon wrote config.json" || ok init.no_config_written
    grep -q '"channels":\[[^]]*"task"' "$dir/agentd.log" && ok init.task_channel_always_on \
        || fail init.task_channel_always_on "no task channel with no config"

    # An operator edit must survive a restart (init never truncates).
    printf 'INIT-MARKER-9Z\n' > "$home/persona.md"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd2.log" 2>&1 &
    track_pid $!
    wait_line "$dir/agentd2.log" '"event":"\(serving\)"' >/dev/null || { fail init.restart "no daemon"; return; }
    grep -q 'INIT-MARKER-9Z' "$home/persona.md" \
        && ok init.non_truncating \
        || fail init.non_truncating "persona.md truncated on restart"
}

flow_no_session() {
    echo "=== FLOW no_session (a login without a session, or an unknown kernel, is refused) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"
    # The kernel is recorded for nobody's installation, but nobody never logged in.
    know "$DB" "$KDIR/nobody"
    local out rc
    out=$(JUICE_HOME="$(client_home nobody)" "$AGENTD" serve "$(login nobody)" 2>&1); rc=$?
    assert_ne no_session.exit_nonzero 0 "$rc"
    assert_contains no_session.names_login_command "juice auth login $(login nobody)" "$out"
    out=$(JUICE_HOME="$(client_home nobody)" "$AGENTD" serve nobody@elsewhere 2>&1)
    assert_contains no_session.names_kernel_add "juice kernel add" "$out"
    out=$(JUICE_HOME="$(client_home nobody)" "$AGENTD" serve nobody 2>&1)
    assert_contains no_session.bare_handle_refused "handle@kernel" "$out"
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

flow_task_channel() {
    echo "=== FLOW task_channel (intake + egress mechanics; no LLM required) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir" || { fail task_channel.kernel "no kernel"; return; }
    create_agent
    create_user other other-pass
    start_agentd "$dir" || { fail task_channel.agentd "no daemon"; return; }

    # A task addressed to the agent is picked up and COMPLETED by the daemon even
    # with no LLM: a failed loop still acks the task (waiting -> done). The reply
    # rides in the completion transaction's args, readable by the sender.
    local tid; tid=$(message agent "deterministic ping")
    assert_nonempty task_channel.created "$tid"
    assert_eq task_channel.completed_by_daemon done "$(await_task "$tid" 60)"
    local tx args; tx=$(kj task show "$tid" | jfield tx_id)
    args=$(kj tx show "$tx" | python3 -c 'import sys,json; print(json.dumps(json.load(sys.stdin).get("args",{})))' 2>/dev/null)
    assert_contains task_channel.reply_in_tx_args "deterministic ping" "$args"

    # A task addressed to other is not the agent's work: the daemon leaves it
    # waiting (only tasks whose required caller is the agent are its to do).
    local oid; oid=$(message other "not for the agent")
    sleep 3 # several daemon polls
    assert_eq task_channel.foreign_untouched waiting "$(kj task show "$oid" | jfield status)"
}

flow_named() {
    echo "=== FLOW named (multiple agents, one per login) ==="  # mechanism only, no LLM
    local dir; dir=$(new_dir)
    start_kernel "$dir"
    create_user alice alice-pass
    create_user bob bob-pass
    local h
    for h in alice bob; do
        JUICE_HOME="$(client_home "$h")" "$AGENTD" serve "$(login "$h")" >"$dir/$h.log" 2>&1 &
        track_pid $!
    done
    wait_line "$dir/alice.log" '"event":"\(serving\)"' >/dev/null
    wait_line "$dir/bob.log"   '"event":"\(serving\)"' >/dev/null

    # Each agent serves as its own login, in its own home.
    assert_contains named.alice_identity "\"login\":\"$(login alice)\"" "$(cat "$dir/alice.log")"
    assert_contains named.bob_identity   "\"login\":\"$(login bob)\""   "$(cat "$dir/bob.log")"
    [ -d "$(agent_home alice)" ] && [ -d "$(agent_home bob)" ] && ok named.own_homes || fail named.own_homes "missing agent homes"
}

flow_session_identity() {
    echo "=== FLOW session_identity (a login relabelled to another account is not adopted) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent; create_user bob bob-pass
    start_agentd "$dir" || { fail session_identity.agentd "no daemon"; return; }

    # A later login under the agent's label stores bob's session in its file.
    cp "$(client_home bob)/client/credentials/$(login bob).json" \
       "$(client_home agent)/client/credentials/$(login agent).json"
    # While the agent is suspended the kernel refuses its held token, so the
    # daemon re-reads the file on its next poll and finds bob's session there.
    k admin user suspend "$(login agent)" >/dev/null 2>&1 || { fail session_identity.suspend "suspend failed"; return; }
    sleep 3 # several daemon polls
    k admin user unsuspend "$(login agent)" >/dev/null 2>&1

    # The daemon kept the agent's own session: a task addressed to the agent is
    # still seen and completed by the agent (bob's session would never list it).
    local tid tx; tid=$(message agent "identity check")
    assert_eq session_identity.still_the_agent done "$(await_task "$tid" 120)"
    tx=$(kj task show "$tid" | jfield tx_id)
    assert_eq session_identity.caller_is_agent "$(login agent)" "$(kj tx show "$tx" | jfield caller)"
}

# ---- LLM-in-the-loop flows (real model; robust structural assertions) ------

flow_task() {
    echo "=== FLOW task (real LLM: answer + ledger) ==="
    require_ollama || { skip task.answered "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail task.agentd "no daemon"; return; }

    local before after id out
    before=$(jj "$DB" "$KDIR/agent" user me | jfield available)
    id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    assert_nonempty task.task_id "$id"
    out=$(await_http "$id") || { fail task.await "no outcome (model too slow?)"; return; }
    after=$(jj "$DB" "$KDIR/agent" user me | jfield available)
    assert_eq    task.answered "answered" "$(echo "$out" | jfield status)"
    assert_match task.iso_answer '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}' "$(echo "$out" | afield answer)"
    # Budget invariant: whatever the model chose, spend never exceeds the budget.
    assert_le    task.within_budget "$(echo "$out" | jfield spent)" 100
    # spent is what the kernel charged: the agent's balance fell by exactly that.
    assert_eq    task.spent_is_charged "$(echo "$out" | jfield spent)" "$(( before - after ))"
}

flow_role_law() {
    echo "=== FLOW role_law (job via a kernel task, real LLM) ==="
    require_ollama || { skip role_law.task_done "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail role_law.agentd "no daemon"; return; }

    local tid; tid=$(message agent "Please reply that you acknowledge this.")
    assert_nonempty role_law.task_created "$tid"
    assert_eq role_law.task_done "done" "$(await_task "$tid")"
    local txid; txid=$(kj task show "$tid" | jfield tx_id)
    if [ -n "$txid" ]; then
        assert_eq role_law.caller_is_agent "$(login agent)" "$(kj tx show "$txid" | jfield caller)"
    else
        fail role_law.tx "task had no tx_id"
    fi
}

flow_rating() {
    echo "=== FLOW rating (real LLM: agent rates its tool use) ==="
    require_ollama || { skip rating.tx_rated "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail rating.agentd "no daemon"; return; }

    # A time question makes the model dispatch the time native, giving the
    # post-job rating pass a real call to grade.
    local id out; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    out=$(await_http "$id") || { fail rating.await "no outcome"; return; }
    assert_eq rating.answered "answered" "$(echo "$out" | jfield status)"

    sleep 2 # the rating pass runs just after the outcome is delivered
    local rated; rated=$(jj "$DB" "$KDIR/agent" tx list | python3 -c "
import sys,json
for t in json.load(sys.stdin):
    if t.get('action','').endswith('/time') and t.get('rating') is not None:
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
        || fail maker.trigger_fires "no maker_trigger after two similar jobs"
}

flow_persona() {
    echo "=== FLOW persona (persona.md reaches the system prompt) ==="
    require_ollama || { skip persona.injected "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home); mkdir -p "$home"
    printf 'Persona note PERSONA-MARKER-7Q: be terse.\n' > "$home/persona.md"
    start_agentd "$dir" || { fail persona.agentd "no daemon"; return; }

    local id out; id=$(submit_http "In one short sentence, what is the capital of France?" 100)
    out=$(await_http "$id") || { fail persona.await "no outcome"; return; }
    assert_eq persona.answered "answered" "$(echo "$out" | jfield status)"
    # Structural: the persona must appear in the job's injected system prompt.
    if grep -q 'PERSONA-MARKER-7Q' "$ADATA/$id.jsonl" 2>/dev/null && grep -q '# Persona' "$ADATA/$id.jsonl" 2>/dev/null; then
        ok persona.injected
    else
        fail persona.injected "persona.md not present in the job's system prompt"
    fi
}

flow_memory() {
    echo "=== FLOW memory (learned cross-job memory, tagged with its identity) ==="
    require_ollama || { skip memory.persists "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" '"memory_bytes": 8192' || { fail memory.agentd "no daemon"; return; }

    local mem; mem="$(agent_home)/memory.md"
    local id; id=$(submit_http "What is the current time? Reply with an ISO-8601 timestamp." 100)
    await_http "$id" >/dev/null || { fail memory.await1 "no outcome"; return; }
    sleep 2
    [ -s "$mem" ] && ok memory.entry_written || fail memory.entry_written "memory.md empty"
    assert_contains memory.entry_tagged "principal=" "$(cat "$mem" 2>/dev/null)"

    # Restart the daemon (same home) → memory persists and loads into the next job.
    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" '"memory_bytes": 8192' || { fail memory.restart "no daemon"; return; }
    local id2; id2=$(submit_http "What is 2 plus 2?" 100)
    await_http "$id2" >/dev/null || { fail memory.await2 "no outcome"; return; }
    if grep -q '"kind":"system"' "$ADATA/$id2.jsonl" 2>/dev/null && grep -q '# Memory' "$ADATA/$id2.jsonl" 2>/dev/null; then
        ok memory.loaded_next_job
    else
        fail memory.loaded_next_job "job 2 transcript lacks injected memory"
    fi
}

# Mechanism flows (no LLM) run first and fast; LLM flows (real Ollama) follow.
ALL=(flow_bootstrap flow_init flow_no_session flow_recovery flow_drain flow_task_channel flow_named flow_session_identity
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
