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
# starting the daemon as a login and handing it tasks. Each account
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
# create_agent — create the agent's account and, as its operator, approve sys:
# the message helper below runs as sys, and the kernel withholds every task from a
# requester the addressee has not approved (kernel U52).
create_agent() {
    create_user agent agent-pass
    approve agent "sys@$(kname "$DB")"
}

# approve handle requester — handle's operator approves requester (kernel U52).
approve() { jj "$DB" "$KDIR/$1" user approve "$2" >/dev/null; }

# ---------------------------------------------------------------------------
# agentd
# ---------------------------------------------------------------------------
# agent_home [handle] — the daemon's home for handle's login (default agent).
agent_home() { local h="${1:-agent}"; echo "$(client_home "$h")/agents/$(login "$h")"; }

# start_agentd dir [cfg-fragment] — write the agent's optional config.json, launch
# the daemon as agent's login, and set AGENTD_PID and PERSONAS.
start_agentd() {
    local dir="$1" extra="${2:-}" home; home=$(agent_home)
    PERSONAS="$home/personas"
    mkdir -p "$home"
    cat > "$home/config.json" <<CFG
{ "default_budget": 100, "max_turns": 12, "concurrency": 2, "poll_seconds": 1, "rate_tasks": true
  ${extra:+, $extra} }
CFG
    JUICE_HOME="$(client_home agent)" "$AGENTD" serve "$(login agent)" >"$dir/agentd.log" 2>&1 &
    AGENTD_PID=$!; track_pid "$AGENTD_PID"
    wait_line "$dir/agentd.log" '"event":"\(serving\)"' >/dev/null || { echo "agentd did not start" >&2; cat "$dir/agentd.log" >&2; return 1; }
}

# message to text [from] — park a message task for to (a handle) as from (default
# sys); echo its id.
message() {
    local run=(kj); [ -n "${3:-}" ] && run=(jj "$DB" "$KDIR/$3")
    "${run[@]}" run "sys@$(kname "$DB")/message" "{\"to\":\"$(login "$1")\",\"message\":$(json_str "$2")}" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['task_id'])" 2>/dev/null
}

# reply_of id [from] — the answer the agent completed task id with, read from the
# completion transaction as its sender (default sys), a party to it.
reply_of() {
    local run=(kj); [ -n "${2:-}" ] && run=(jj "$DB" "$KDIR/$2")
    local tx; tx=$("${run[@]}" task show "$1" | jfield tx_id)
    "${run[@]}" tx show "$tx" | python3 -c "import sys,json; print(json.load(sys.stdin).get('args',{}).get('answer',''))" 2>/dev/null
}

# ---------------------------------------------------------------------------
# applications: an HTTP action whose backend creates a task for the agent
# ---------------------------------------------------------------------------
# start_app handle name — publish handle's action <name>; each call creates a
# task for the agent through the call's capability (kernel API §9), with the
# call's args as partial_args. The task acts for handle (its requester), is paid
# by the caller (its owner) and is created by handle's action (created_by).
start_app() {
    local port; port=$(backend_port)
    python3 - "$port" "$(login agent)" <<'PYEOF' &
import sys, json, http.server, urllib.request
port, to = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        args = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))) or b'{}')
        cap, out = self.headers.get('X-Juice-Capability'), b'{}'
        if cap:  # absent only on the harness's readiness probe
            req = urllib.request.Request(self.headers['X-Juice-Callback'] + '/v1/tasks', method='POST',
                data=json.dumps({"action": "sys@" + to.split('@')[1] + "/sink", "required_caller": to,
                                 "partial_args": args}).encode(),
                headers={'Content-Type': 'application/json', 'X-Juice-Capability': cap})
            out = json.dumps({"task_id": json.load(urllib.request.urlopen(req))["id"]}).encode()
        self.send_response(200); self.send_header('Content-Type', 'application/json'); self.end_headers()
        self.wfile.write(out)
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    track_pid $!
    _await_http "$port" POST
    publish "$DB" "$KDIR/$1" "$2" --kind http --source "http://127.0.0.1:$port/" --price "$(units 0)" \
        --description "relays a post to the agent" >/dev/null
}

# chat_action handle name source price — publish, as handle, an HTTP action at
# source meeting the reason contract, at price base units; echo its address.
chat_action() {
    local in='{"type":"object","properties":{"messages":{"type":"array","description":"The conversation","items":{"type":"object","properties":{"role":{"type":"string","description":"Who speaks"},"content":{"type":"string","description":"What is said"}},"required":["role","content"]}}},"required":["messages"]}'
    local out='{"type":"object","properties":{"choices":{"type":"array","description":"The reply","items":{"type":"object","properties":{"message":{"type":"object","description":"The reply","properties":{"content":{"type":"string","description":"The reply text"}}}}}}}}'
    publish "$DB" "$KDIR/$1" "$2" --kind http --source "$3" --price "$(units "$4")" \
        --description "a chat endpoint" --input-schema "$in" --output-schema "$out" >/dev/null \
        && echo "$(login "$1")/$2"
}

# dead_reason — a free reasoning action of the agent's whose endpoint is closed;
# echo its address. Every call fails at once, so every job fails at once and its
# task is completed with status failed: a flow's conversations and completions
# are then decided by the task fields alone, whether or not a model is present.
dead_reason() { chat_action agent dead-chat "http://127.0.0.1:1/" 0; }

# replying_reason handle price — handle's reasoning action at price base units
# whose every reply selects the reply tool with the answer "done"; sets REASON to
# its address. A job reasoning through it takes exactly one paid turn. It sets a
# variable rather than echoing, so the backend starts in the flow's own shell,
# where the harness records it and stops it with the flow.
replying_reason() {
    local port; port=$(backend_port)
    start_backend "$port" 200 '{"choices":[{"message":{"content":"{\"action\":\"reply\",\"args\":{\"answer\":\"done\"}}"}}]}' >/dev/null
    REASON=$(chat_action "$1" chat "http://127.0.0.1:$port/" "$2")
}

# completion_of id field — a field of the args task id was completed with, read
# as its sender, sys.
completion_of() {
    local tx; tx=$(kj task show "$1" | jfield tx_id)
    kj tx show "$tx" | python3 -c "import sys,json; print(json.load(sys.stdin).get('args',{}).get(sys.argv[1],''))" "$2" 2>/dev/null
}

# post from action json — call action as from; echo the task it created.
post() {
    jj "$DB" "$KDIR/$1" run "$2" "$3" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['task_id'])" 2>/dev/null
}

# persona_of id log — the persona the daemon opened for task id.
persona_of() {
    grep '"event":"persona_opened"' "$2" | python3 -c "
import sys,json
for l in sys.stdin:
    e=json.loads(l)
    if e['job']=='task-$1': print(e['persona'])"
}

# loaded_of id log — how many earlier records the job for task id carried in.
loaded_of() {
    grep '"event":"persona_opened"' "$2" | python3 -c "
import sys,json
for l in sys.stdin:
    e=json.loads(l)
    if e['job']=='task-$1': print(e['loaded'])"
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

assert_le()      { [ "$2" -le "$3" ] && ok "$1" || fail "$1" "want $2 <= $3"; }
assert_match()   { echo "$3" | grep -qE "$2" && ok "$1" || fail "$1" "want /$2/ in [$3]"; }
