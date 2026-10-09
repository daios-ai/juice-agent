#!/usr/bin/env bash
#
# flows/lib.sh — integration harness for juice-agent flows.
#
# Drives ONLY compiled binaries: a real juice kernel wired to a REAL Ollama (the
# LLM the kernel ships with, behind its llm/* natives), and the juice-agent
# daemon. Bash cannot reach either in process, so a flow can never bypass the
# binary.
#
# Booting a kernel, creating accounts, funding them and logging them in is the
# kernel's own business, so this harness sources the kernel repository's flow
# library ($JUICE_SRC/flows/lib.sh) for it and adds only what is the agent's:
# starting the daemon as a login and talking to its HTTP channel. Each account
# gets its own client home (its juice CLI records), which is the JUICE_HOME the
# daemon then runs under — exactly as an operator's installation would be.
#
# Because a real model sits in the loop, LLM-dependent flows assert robust
# structural properties rather than exact model outputs; they skip (never fail)
# when the chat model is absent.
#
# Requires: bash>=4, python3, curl. JUICE_SRC is the kernel checkout (default
# ../juice); JUICE_BIN its compiled binary (default $JUICE_SRC/juice).

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENTD="$ROOT/juice-agent"
JUICE_SRC="${JUICE_SRC:-$ROOT/../juice}"
JUICE="${JUICE_BIN:-$JUICE_SRC/juice}"
[ -x "$AGENTD" ] || { echo "missing binary: $AGENTD (run 'make build')" >&2; exit 1; }
# shellcheck source=/dev/null
source "$JUICE_SRC/flows/lib.sh"

OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
CHAT_MODEL="${OLLAMA_CHAT_MODEL:-gemma4:26b}" # the model the kernel's shipped ollama endpoint binds

SKIP=0
skip() { echo "  SKIP: $1 — $2"; SKIP=$((SKIP+1)); }

# require_ollama — true iff Ollama is up and the chat model is present.
require_ollama() {
    local tags; tags=$(curl -s "$OLLAMA_URL/api/tags" 2>/dev/null) || return 1
    echo "$tags" | python3 -c "import sys,json; ms=[m['name'] for m in json.load(sys.stdin).get('models',[])]; sys.exit(0 if '$CHAT_MODEL' in ms else 1)" 2>/dev/null
}

# wait_line file regex — wait up to 30s for regex in file; print capture group 1.
wait_line() {
    local file="$1" re="$2" deadline=$(( $(date +%s) + 30 )) out
    while :; do
        out=$(sed -n "s/.*$re.*/\1/p" "$file" 2>/dev/null | head -1)
        [ -n "$out" ] && { echo "$out"; return 0; }
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.05
    done
}

# ---------------------------------------------------------------------------
# kernel and accounts
# ---------------------------------------------------------------------------
# start_kernel dir — boot a kernel serving the play world under dir and log sys
# in. Sets KDIR and DB; k/kj then run the juice CLI as sys.
start_kernel() {
    KDIR="$1"; DB=$(kdb "$KDIR")
    make_admin "$DB" "$KDIR/sys" fee_bps=0
}
k()  { j  "$DB" "$KDIR/sys" "$@"; }
kj() { jj "$DB" "$KDIR/sys" "$@"; }

# login handle — the account's login on the flow's kernel, handle@kernel.
login() { echo "$1@$(kname "$DB")"; }

# client_home handle — the juice-family root holding handle's CLI records.
client_home() { echo "$KDIR/$1/.juice"; }

# create_user handle password — create, fund and log in an account; its records
# land in client_home handle.
create_user() {
    make_user "$DB" "$KDIR/sys" "$KDIR/$1" "$1" "$2"
    deposit "$DB" "$KDIR/sys" "$1" 100000000
}
create_agent() { create_user agent agent-pass; }

# ---------------------------------------------------------------------------
# agentd
# ---------------------------------------------------------------------------
# agent_home [handle] — the daemon's home for handle's login (default agent).
agent_home() { local h="${1:-agent}"; echo "$(client_home "$h")/agents/$(login "$h")"; }

# start_agentd dir [cfg-fragment] — write the agent's optional config.json, launch
# the daemon as agent's login, and set AGENTD_PID, ADATA and AHTTP. Memory is off
# by default (flows that test it override memory_bytes in their fragment).
start_agentd() {
    local dir="$1" extra="${2:-}" home; home=$(agent_home)
    ADATA="$home/data"
    mkdir -p "$home"
    cat > "$home/config.json" <<CFG
{ "default_budget": 100, "max_turns": 12, "concurrency": 2,
  "lookup_limit": 8, "candidate_limit": 12, "rate_tasks": true, "memory_bytes": 0,
  "channels": { "http": { "addr": "127.0.0.1:0" }, "task": { "poll_seconds": 1 } }
  ${extra:+, $extra} }
CFG
    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd.log" 2>&1 &
    AGENTD_PID=$!; track_pid "$AGENTD_PID"
    local addr; addr=$(wait_line "$dir/agentd.log" '"event":"serving".*"http_addr":"\([^"]*\)"')
    [ -n "$addr" ] || { echo "agentd did not start" >&2; cat "$dir/agentd.log" >&2; return 1; }
    AHTTP="http://$addr"
}

# submit_http prompt [budget] — POST a job to agentd's http channel, echo its id.
submit_http() {
    local prompt="$1" budget="${2:-0}"
    curl -sf -X POST "$AHTTP/task" -H 'Content-Type: application/json' \
        -d "{\"prompt\":$(json_str "$prompt"),\"budget\":$budget}" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['task_id'])" 2>/dev/null
}

# await_http id [timeout_s] — poll until the outcome is ready; echo its JSON.
await_http() {
    local id="$1" secs="${2:-300}" code
    local deadline=$(( $(date +%s) + secs ))
    while :; do
        code=$(curl -s -o /dev/null -w '%{http_code}' "$AHTTP/task/$id" 2>/dev/null)
        [ "$code" = "200" ] && { curl -s "$AHTTP/task/$id" 2>/dev/null; return 0; }
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.3
    done
}

# message to text — park a message task for to (a handle) as sys; echo its id.
message() {
    kj run "sys@$(kname "$DB")/message" "{\"to\":\"$(login "$1")\",\"message\":$(json_str "$2")}" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['task_id'])" 2>/dev/null
}

# await_task id [timeout_s] — wait until a task is done; echo its status.
await_task() {
    local id="$1" deadline=$(( $(date +%s) + ${2:-300} )) st=""
    while [ "$(date +%s)" -lt "$deadline" ]; do
        st=$(kj task show "$id" | jfield status); [ "$st" = "done" ] && break; sleep 0.5
    done
    echo "$st"
}

json_str() { python3 -c "import sys,json; print(json.dumps(sys.argv[1]))" "$1"; }
jfield()   { python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get(sys.argv[1],''))" "$1"; }
afield()   { python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('answer',{}).get(sys.argv[1],''))" "$1"; }

assert_le()      { [ "$2" -le "$3" ] && ok "$1" || fail "$1" "want $2 <= $3"; }
assert_match()   { echo "$3" | grep -qE "$2" && ok "$1" || fail "$1" "want /$2/ in [$3]"; }
