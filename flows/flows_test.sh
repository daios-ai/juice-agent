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
# choices that would exercise them on demand. The budget flow stages the budget
# with a reasoning action that always replies; the model flows assert the
# invariant that survives regardless (spend never exceeds budget).

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
    assert_not_contains bootstrap.no_fake_subtask "$(login agent)/subtask" "$acts"

    kill -9 "$AGENTD_PID" 2>/dev/null; wait "$AGENTD_PID" 2>/dev/null
    start_agentd "$dir" || { fail bootstrap.restart "no daemon"; return; }
    ok bootstrap.restart
}

flow_recovery() {
    echo "=== FLOW recovery ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local pdir; pdir="$(agent_home)/personas"
    mkdir -p "$pdir"
    printf '%s\n%s\n' \
        '{"role":"user","content":"half-done job"}' \
        '{"role":"assistant","action":"sys@k/time"}' > "$pdir/orphan.jsonl"

    start_agentd "$dir" || { fail recovery.agentd "no daemon"; return; }
    if grep -q '"event":"recovery"' "$dir/agentd.log" && grep -q 'orphan' "$dir/agentd.log"; then
        ok recovery.marks_interrupted
    else
        fail recovery.marks_interrupted "no recovery log for orphan"
    fi
    assert_contains recovery.marker '"kind":"interrupted"' "$(tail -1 "$pdir/orphan.jsonl")"
}

flow_init() {
    echo "=== FLOW init (home under agents/<login>; blank agent.md; personas/; config.json and its schema written) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home)

    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd.log" 2>&1 &
    local pid=$!; track_pid "$pid"
    wait_line "$dir/agentd.log" '"event":"\(serving\)"' >/dev/null || { fail init.serving "no daemon"; cat "$dir/agentd.log" >&2; return; }

    [ -f "$home/agent.md" ] && [ ! -s "$home/agent.md" ] && ok init.blank_agent_md || fail init.blank_agent_md "agent.md missing or not blank"
    [ -d "$home/personas" ] && ok init.personas_dir || fail init.personas_dir "no personas/"
    local retired=""
    for f in persona.md knowledge.md memory.md data; do [ -e "$home/$f" ] && retired="$retired $f"; done
    [ -z "$retired" ] && ok init.no_retired_files || fail init.no_retired_files "the daemon created:$retired"
    # First start writes every key, $schema first, the affordances the login's kernel's natives.
    local keys; keys=$(python3 -c "import sys,json; print(' '.join(json.load(open(sys.argv[1]))))" "$home/config.json" 2>/dev/null)
    assert_eq init.config_written '$schema reason action_lookup default_budget max_turns concurrency poll_seconds compact_bytes rate_tasks' "$keys"
    assert_eq init.config_reason "sys@$(kname "$DB")/llm/chat" "$(jfield reason < "$home/config.json")"
    [ -f "$(client_home agent)/schemas/juice-agent.schema.json" ] && ok init.schema_written || fail init.schema_written "no schema"
    # With the file first start wrote, the daemon takes tasks.
    local tid; tid=$(message agent "no config")
    assert_eq init.serves_with_written_config done "$(await_task "$tid" 120)"

    # An operator edit must survive a restart (init never truncates). A partial config.json, as a
    # hand or an older release writes it, gets exactly the keys first start creates written in.
    printf 'INIT-MARKER-9Z\n' > "$home/agent.md"
    echo '{ "max_turns": 7 }' > "$home/config.json"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd2.log" 2>&1 &
    track_pid $!
    wait_line "$dir/agentd2.log" '"event":"\(serving\)"' >/dev/null || { fail init.restart "no daemon"; return; }
    grep -q 'INIT-MARKER-9Z' "$home/agent.md" \
        && ok init.non_truncating \
        || fail init.non_truncating "agent.md truncated on restart"
    keys=$(python3 -c "import sys,json; print(' '.join(json.load(open(sys.argv[1]))))" "$home/config.json" 2>/dev/null)
    assert_eq init.partial_config_completed '$schema action_lookup max_turns reason' "$keys"
    assert_eq init.operator_value_kept 7 "$(jfield max_turns < "$home/config.json")"
}

flow_config() {
    echo "=== FLOW config (retired keys and wrong-shaped affordances are refused at startup) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home); mkdir -p "$home"
    local key out rc
    for key in channels memory_bytes maker; do
        echo "{ \"$key\": {} }" > "$home/config.json"
        out=$(JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" 2>&1); rc=$?
        assert_ne "config.refuses_$key" 0 "$rc"
    done
    assert_contains config.names_file "config.json" "$out"

    # Each affordance is checked against its contract: an action of another
    # shape stops the daemon, naming the key, the address and the field.
    local t="sys@$(kname "$DB")/time"
    echo "{ \"reason\": \"$t\" }" > "$home/config.json"
    out=$(JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" 2>&1); rc=$?
    assert_ne config.refuses_wrong_reason 0 "$rc"
    assert_contains config.names_reason "reason = $t: input.properties.messages: missing" "$out"
    echo "{ \"action_lookup\": \"sys@$(kname "$DB")/llm/chat\" }" > "$home/config.json"
    out=$(JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" 2>&1); rc=$?
    assert_ne config.refuses_wrong_action_lookup 0 "$rc"
    assert_contains config.names_action_lookup "action_lookup = sys@$(kname "$DB")/llm/chat: input: requires messages, which the agent does not send" "$out"
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

flow_lock() {
    echo "=== FLOW lock (one daemon per agent: serve.lock holds its pid under its flock) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail lock.agentd "no daemon"; return; }
    local lock; lock="$(agent_home)/serve.lock"
    assert_eq lock.holds_pid "$AGENTD_PID" "$(cat "$lock" 2>/dev/null)"

    local out rc
    # Bounded, so a second daemon that is wrongly admitted fails the flow instead of hanging it.
    out=$(JUICE_HOME="$(client_home agent)" timeout 10 "$AGENTD" serve "$(login agent)" 2>&1); rc=$?
    assert_ne lock.second_refused 0 "$rc"
    assert_contains lock.names_login "already serving $(login agent)" "$out"

    # The refused daemon left the pid alone, so the pid in the lock is the serving daemon's.
    [ "$(cat "$lock" 2>/dev/null)" = "$AGENTD_PID" ] && ok lock.pid_kept \
        || { fail lock.pid_kept "the lock no longer holds $AGENTD_PID"; return; }
    kill -TERM "$(cat "$lock")" 2>/dev/null
    wait "$AGENTD_PID"; rc=$?
    assert_eq lock.term_drains 0 "$rc"

    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd2.log" 2>&1 &
    track_pid $!
    wait_line "$dir/agentd2.log" '"event":"\(serving\)"' >/dev/null \
        && ok lock.free_after_exit || fail lock.free_after_exit "$(cat "$dir/agentd2.log")"
}

flow_tasks() {
    echo "=== FLOW tasks (intake + completion mechanics) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir" || { fail tasks.kernel "no kernel"; return; }
    create_agent
    create_user other other-pass
    approve other "sys@$(kname "$DB")"
    # No model: every job fails at once, so the completion is deterministic.
    start_agentd "$dir" "\"reason\": \"$(dead_reason)\"" || { fail tasks.agentd "no daemon"; return; }

    # A task addressed to the agent is picked up and COMPLETED by the daemon, even
    # when its job fails. The reply rides in the completion transaction's args,
    # which the sender can read: the completion runs as the sender (kernel D6).
    # The kernel merges the task's partial_args into those args, so the check reads
    # the status, which only the agent's completion supplies.
    local tid; tid=$(message agent "deterministic ping")
    assert_nonempty tasks.created "$tid"
    assert_eq tasks.completed_by_daemon done "$(await_task "$tid" 120)"
    local tx; tx=$(kj task show "$tid" | jfield tx_id)
    assert_eq tasks.reply_in_tx_args failed "$(kj tx show "$tx" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("args",{}).get("status",""))' 2>/dev/null)"
    assert_eq tasks.runs_as_sender "sys@$(kname "$DB")" "$(kj tx show "$tx" | jfield caller)"

    # A task addressed to other is not the agent's work: it stays waiting.
    local oid; oid=$(message other "not for the agent")
    sleep 3 # several daemon polls
    assert_eq tasks.foreign_untouched waiting "$(kj task show "$oid" | jfield status)"
}

flow_budget() {
    echo "=== FLOW budget (reasoning is paid from the job's budget, at the terms of each job) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent; create_user alice alice-pass
    # alice's reasoning action answers every turn with the reply, at 7 base units.
    replying_reason alice 7; local reason="$REASON"
    assert_nonempty budget.reason "$reason"
    start_agentd "$dir" "\"reason\": \"$reason\"" || { fail budget.agentd "no daemon"; return; }
    local before after tid spent
    before=$(jj "$DB" "$KDIR/agent" user me | jfield available)

    # One turn: answered, its 7 counted in spent, and the wallet fell by that.
    tid=$(message agent "say done")
    assert_eq budget.done done "$(await_task "$tid" 60)"
    assert_eq budget.answered done "$(reply_of "$tid")"
    after=$(jj "$DB" "$KDIR/agent" user me | jfield available)
    spent=$(grep '"event":"task_end"' "$dir/agentd.log" | tail -1 | jfield spent)
    assert_eq budget.turn_counted 7 "$spent"
    assert_eq budget.spent_is_charged "$spent" "$(( before - after ))"

    # alice raises the price above the job budget (100) while the daemon runs. The
    # next job reads the new terms, so the turn is refused for the budget, not for
    # moved terms, and nothing is paid.
    jj "$DB" "$KDIR/alice" action update "$reason" --price "$(units 150)" >/dev/null
    tid=$(message agent "say done again")
    assert_eq budget.over_done done "$(await_task "$tid" 60)"
    assert_eq budget.over_failed failed "$(completion_of "$tid" status)"
    assert_eq budget.over_reason budget "$(completion_of "$tid" reason)"
    assert_eq budget.over_unpaid "$after" "$(jj "$DB" "$KDIR/agent" user me | jfield available)"
}

flow_approval() {
    echo "=== FLOW approval (the kernel withholds an unapproved sender's task) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent; create_user alice alice-pass
    start_agentd "$dir" || { fail approval.agentd "no daemon"; return; }

    local tid; tid=$(message agent "let me in" alice)
    assert_nonempty approval.created "$tid"
    sleep 3 # several daemon polls
    assert_eq approval.withheld_until_approved waiting "$(jj "$DB" "$KDIR/alice" task show "$tid" | jfield status)"
    assert_contains approval.listed_pending '"pending"' "$(jj "$DB" "$KDIR/agent" user requesters)"

    approve agent "$(login alice)"
    assert_eq approval.served_once_approved done "$(await_task_as alice "$tid" 120)"
}

# await_task_as handle id [timeout_s] — wait until handle sees task id done.
await_task_as() {
    local h="$1" id="$2" deadline=$(( $(date +%s) + ${3:-300} )) st=""
    while [ "$(date +%s)" -lt "$deadline" ]; do
        st=$(jj "$DB" "$KDIR/$h" task show "$id" | jfield status); [ "$st" = "done" ] && break; sleep 0.5
    done
    echo "$st"
}

flow_personas() {
    echo "=== FLOW personas (one file per sender; a sender's next job loads the last) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent; create_user alice alice-pass
    approve agent "$(login alice)"
    start_agentd "$dir" || { fail personas.agentd "no daemon"; return; }

    local t1 t2 t3
    t1=$(message agent "first from sys");  await_task "$t1" 300 >/dev/null
    t2=$(message agent "second from sys"); await_task "$t2" 300 >/dev/null
    t3=$(message agent "hello from alice" alice); await_task_as alice "$t3" 300 >/dev/null

    local n; n=$(ls "$PERSONAS"/*.jsonl 2>/dev/null | wc -l)
    assert_eq personas.one_file_per_sender 2 "$n"
    local sysfile; sysfile=$(grep -l 'first from sys' "$PERSONAS"/*.jsonl)
    assert_contains personas.second_in_same_file "second from sys" "$(cat "$sysfile")"
    assert_not_contains personas.alice_kept_apart "hello from alice" "$(cat "$sysfile")"
    assert_contains personas.attributed "sys@$(kname "$DB"): first from sys" "$(cat "$sysfile")"

    # The log says what each job loaded: nothing for a persona's first job, the
    # earlier exchange for its next.
    local loads; loads=$(grep '"event":"persona_opened"' "$dir/agentd.log" | python3 -c '
import sys,json
for l in sys.stdin:
    e=json.loads(l); print(e["job"], e["loaded"])')
    local l1 l2 l3
    l1=$(echo "$loads" | awk -v j="task-$t1" '$1==j{print $2}')
    l2=$(echo "$loads" | awk -v j="task-$t2" '$1==j{print $2}')
    l3=$(echo "$loads" | awk -v j="task-$t3" '$1==j{print $2}')
    assert_eq personas.first_job_loads_nothing 0 "$l1"
    [ "${l2:-0}" -gt 0 ] && ok personas.next_job_loads_history || fail personas.next_job_loads_history "loaded=[$l2]"
    assert_eq personas.other_sender_loads_nothing 0 "$l3"
}

flow_threads() {
    echo "=== FLOW threads (applications' tasks: which tasks share a conversation) ==="
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local u; for u in alice carol bob bot2 rooms cron; do create_user "$u" "$u-pass"; done
    start_app rooms post; start_app rooms notice; start_app cron fire; start_app bob post
    local R="$(login rooms)/post" N="$(login rooms)/notice" C="$(login cron)/fire" B="$(login bob)/post"

    # Access: a relayed task acts for the application, so approving the person
    # who posted releases nothing; the operator approves the application.
    local a1; a1=$(post alice "$R" '{"thread_id":"team","message":"A1 lunch at noon"}')
    assert_nonempty threads.app_created_task "$a1"
    approve agent "$(login alice)"
    assert_eq threads.withheld_until_app_approved "[]" "$(jj "$DB" "$KDIR/agent" task list | tr -d ' \n')"
    assert_contains threads.app_listed_pending "\"$(login rooms)\"" "$(jj "$DB" "$KDIR/agent" user requesters)"
    local t; for t in rooms cron bob bot2; do approve agent "$(login "$t")"; done

    # Every task waits before the daemon starts, so all arrive in one poll.
    local c1 c2 ev s1 n1 b1 d1 r1 f1 f2
    c1=$(post carol "$R" '{"thread_id":"team","message":"C1 I will bring cake"}')
    c2=$(post carol "$R" '{"thread_id":"team","message":"C2 and coffee"}')
    ev=$(post rooms "$R" '{"thread_id":"team","event":"carol joined"}')
    s1=$(post alice "$R" '{"thread_id":"secret","message":"S1 surprise party"}')
    n1=$(post alice "$N" '{"thread_id":"team","message":"N1 another action"}')
    b1=$(post alice "$B" '{"thread_id":"secret","message":"B1 via bob"}')
    d1=$(message agent "D1 direct from alice" alice)
    r1=$(message agent "R1 from another agent" bot2)
    f1=$(post agent "$C" '{"note":"F1 reminder one"}')
    f2=$(post agent "$C" '{"note":"F2 reminder two"}')

    # No model: every job fails at once and still completes its task, so the
    # conversations are decided by the task fields alone.
    start_agentd "$dir" "\"reason\": \"$(dead_reason)\"" || { fail threads.agentd "no daemon"; return; }
    local id all="$a1 $c1 $c2 $ev $s1 $n1 $b1 $d1 $r1 $f1 $f2" undone=""
    for id in $all; do [ "$(await_task "$id" 120)" = done ] || undone="$undone $id"; done
    assert_eq threads.all_answered "" "$undone"
    local args; args=$(kj tx show "$(kj task show "$s1" | jfield tx_id)" | python3 -c 'import sys,json; print(json.load(sys.stdin)["args"].get("status",""))')
    assert_eq threads.reply_in_completion failed "$args"

    local log="$dir/agentd.log" team
    team=$(persona_of "$a1" "$log")
    # One room is one conversation, whoever speaks, and an application's event joins it.
    for id in $c1 $c2 $ev; do assert_eq threads.room_shared "$team" "$(persona_of "$id" "$log")"; done
    # Its jobs run one at a time, each carrying in all earlier ones: four distinct loads.
    local loads; loads=$(for id in $a1 $c1 $c2 $ev; do loaded_of "$id" "$log"; done | sort -un | wc -l)
    assert_eq threads.room_serialised 4 "$loads"
    # Everything else is its own conversation: another room, the same room name
    # from another action of the application or from another application, a
    # direct message, another agent, and each reminder.
    local distinct; distinct=$(for id in $a1 $s1 $n1 $b1 $d1 $r1 $f1 $f2; do persona_of "$id" "$log"; done | sort -u | wc -l)
    assert_eq threads.others_apart 8 "$distinct"
    assert_eq threads.reminder_fresh "0 0" "$(loaded_of "$f1" "$log") $(loaded_of "$f2" "$log")"

    # What each conversation holds, attributed to who said it.
    local tf sf bf df
    tf=$(cat "$PERSONAS/$team.jsonl")
    sf=$(cat "$PERSONAS/$(persona_of "$s1" "$log").jsonl")
    bf=$(cat "$PERSONAS/$(persona_of "$b1" "$log").jsonl")
    df=$(cat "$PERSONAS/$(persona_of "$d1" "$log").jsonl")
    assert_contains threads.room_alice "$(login alice) via $(login rooms): A1" "$tf"
    assert_contains threads.room_carol "$(login carol) via $(login rooms): C2" "$tf"
    assert_contains threads.room_event "carol joined" "$tf"
    assert_not_contains threads.room_no_secret "S1" "$tf"
    assert_contains threads.secret_holds_own "S1 surprise party" "$sf"
    assert_not_contains threads.secret_no_room "A1" "$sf"
    assert_contains threads.other_app_attributed "$(login alice) via $(login bob): B1" "$bf"
    assert_not_contains threads.other_app_no_secret "S1" "$bf"
    assert_contains threads.direct_attributed "$(login alice): D1" "$df"
    assert_not_contains threads.direct_no_room "A1" "$df"
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

# widest file — the longest line on stdin, in columns.
widest() { tr -d '\r' | awk '{ if (length > n) n = length } END { print n + 0 }'; }

flow_usage() {
    echo "=== FLOW usage (wraps to the terminal's width; no kernel) ==="
    local out; out=$("$AGENTD" 2>&1)
    assert_contains usage.printed "usage:" "$out"
    assert_contains usage.names_home_files "agent.md" "$out"
    assert_le usage.fits_80_without_terminal "$(echo "$out" | widest)" 80
    command -v script >/dev/null || { skip usage.fits_terminal "script unavailable"; return; }
    assert_le usage.fits_50_column_terminal "$(script -qec "stty cols 50; '$AGENTD'" /dev/null | widest)" 50
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
    # still seen and completed (bob's session would never list it).
    local tid; tid=$(message agent "identity check")
    assert_eq session_identity.still_the_agent done "$(await_task "$tid" 120)"
}

# ---- LLM-in-the-loop flows (real model; robust structural assertions) ------

flow_answer() {
    echo "=== FLOW answer (real LLM: answer + ledger) ==="
    require_ollama || { skip answer.answered "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    start_agentd "$dir" || { fail answer.agentd "no daemon"; return; }

    local before after tid
    before=$(jj "$DB" "$KDIR/agent" user me | jfield available)
    tid=$(message agent "What is the current time? Reply with an ISO-8601 timestamp.")
    assert_eq answer.done done "$(await_task "$tid" 300)"
    after=$(jj "$DB" "$KDIR/agent" user me | jfield available)
    assert_match answer.iso_answer '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}' "$(reply_of "$tid")"
    local spent; spent=$(grep '"event":"task_end"' "$dir/agentd.log" | tail -1 | jfield spent)
    # Budget invariant: whatever the model chose, spend never exceeds the budget.
    assert_le answer.within_budget "$spent" 100
    # spent is what the kernel charged: the agent's balance fell by exactly that.
    assert_eq answer.spent_is_charged "$spent" "$(( before - after ))"
}

flow_agent_md() {
    echo "=== FLOW agent_md (agent.md reaches the system prompt) ==="
    require_ollama || { skip agent_md.injected "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local home; home=$(agent_home); mkdir -p "$home"
    printf 'Agent note AGENT-MARKER-7Q: be terse.\n' > "$home/agent.md"
    start_agentd "$dir" || { fail agent_md.agentd "no daemon"; return; }

    local tid; tid=$(message agent "In one short sentence, what is the capital of France?")
    assert_eq agent_md.done done "$(await_task "$tid" 300)"
    if grep -q 'AGENT-MARKER-7Q' "$PERSONAS"/*.jsonl 2>/dev/null && grep -q '# Agent' "$PERSONAS"/*.jsonl 2>/dev/null; then
        ok agent_md.injected
    else
        fail agent_md.injected "agent.md not present in the job's system prompt"
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
    local tid; tid=$(message agent "What is the current time? Reply with an ISO-8601 timestamp.")
    assert_eq rating.done done "$(await_task "$tid" 300)"
    sleep 3 # the rating pass runs just after the completion
    local rated; rated=$(jj "$DB" "$KDIR/agent" tx list | python3 -c "
import sys,json
for t in json.load(sys.stdin):
    if t.get('action','').endswith('/time') and t.get('rating') is not None:
        print(t['rating'].get('value')); break
" 2>/dev/null)
    assert_nonempty rating.tx_rated "$rated"
    # A second job in the same persona rates only its own calls: no rating error.
    local t2; t2=$(message agent "Thanks. Reply with the word OK.")
    await_task "$t2" 300 >/dev/null; sleep 3
    assert_not_contains rating.never_rerates "rate_error" "$(cat "$dir/agentd.log")"
}

flow_conversation() {
    echo "=== FLOW conversation (real LLM: a persona remembers; another does not) ==="
    require_ollama || { skip conversation.remembers "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent; create_user bob bob-pass
    approve agent "$(login bob)"
    start_agentd "$dir" || { fail conversation.agentd "no daemon"; return; }

    local t1 t2 t3
    t1=$(message agent "Remember this codeword for later: ZEPHYR-4417. Reply only with OK.")
    await_task "$t1" 300 >/dev/null
    t2=$(message agent "What codeword did I ask you to remember? Reply with the codeword only.")
    await_task "$t2" 300 >/dev/null
    assert_contains conversation.remembers "ZEPHYR-4417" "$(reply_of "$t2")"

    t3=$(message agent "What codeword were you asked to remember? Reply with the codeword only, or NONE." bob)
    await_task_as bob "$t3" 300 >/dev/null
    assert_not_contains conversation.isolated "ZEPHYR-4417" "$(reply_of "$t3" bob)"
}

flow_thread_conversation() {
    echo "=== FLOW thread_conversation (real LLM: a room remembers for all in it, and only them) ==="
    require_ollama || { skip thread_conversation.room_remembers "Ollama/$CHAT_MODEL unavailable"; return; }
    local dir; dir=$(new_dir)
    start_kernel "$dir"; create_agent
    local u; for u in alice carol bob rooms; do create_user "$u" "$u-pass"; done
    start_app rooms post; start_app bob post
    local t; for t in rooms bob; do approve agent "$(login "$t")"; done
    start_agentd "$dir" || { fail thread_conversation.agentd "no daemon"; return; }
    local R="$(login rooms)/post" B="$(login bob)/post" ask="What codeword did alice give? Reply with the codeword only, or NONE."

    local t1 t2 t3 t4
    t1=$(post alice "$R" '{"thread_id":"team","message":"Remember this codeword for later: ORCHID-2291. Reply only with OK."}')
    await_task "$t1" 300 >/dev/null
    # Carol was not there when alice spoke, but the room was.
    t2=$(post carol "$R" "{\"thread_id\":\"team\",\"message\":$(json_str "$ask")}")
    await_task "$t2" 300 >/dev/null
    assert_contains thread_conversation.room_remembers "ORCHID-2291" "$(reply_of "$t2")"
    # Alice herself, in another room, and bob's application naming the same room, hear nothing.
    t3=$(post alice "$R" "{\"thread_id\":\"secret\",\"message\":$(json_str "$ask")}")
    t4=$(post bob "$B" "{\"thread_id\":\"team\",\"message\":$(json_str "$ask")}")
    await_task "$t3" 300 >/dev/null; await_task "$t4" 300 >/dev/null
    assert_not_contains thread_conversation.other_room_apart "ORCHID-2291" "$(reply_of "$t3")"
    assert_not_contains thread_conversation.other_app_apart "ORCHID-2291" "$(reply_of "$t4")"
}

# Mechanism flows (deterministic assertions) run first; LLM flows (real Ollama) follow.
ALL=(flow_bootstrap flow_init flow_config flow_no_session flow_recovery flow_drain flow_lock flow_tasks flow_budget flow_approval
     flow_personas flow_threads flow_named flow_session_identity flow_usage
     flow_answer flow_agent_md flow_rating flow_conversation flow_thread_conversation)

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
