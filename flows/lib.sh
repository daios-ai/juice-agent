#!/usr/bin/env bash
#
# flows/lib.sh — integration harness for juice-agent flows.
#
# Drives ONLY compiled binaries: a real juice kernel wired to a REAL Ollama
# (the LLM the kernel ships with, behind @sys/llm/*), and the juice-agent daemon.
# Bash cannot reach either in process, so a flow can never bypass the binary.
#
# Because a real model sits in the loop, LLM-dependent flows assert robust
# structural properties (a task terminates, the ledger charges within budget, a
# step settles under the role law) rather than exact model outputs. The economic
# and turn-cap rails are code invariants proven exhaustively by `go test`; the
# flows confirm the real integration, not the model's cleverness.
#
# Requires: bash>=4, python3, curl, a running Ollama with the chat+embed models.
# JUICE_BIN points at a compiled kernel.

set -uo pipefail

JUICE_BIN="${JUICE_BIN:-../juice/juice}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENTD="$ROOT/juice-agent"

OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
CHAT_MODEL="${OLLAMA_CHAT_MODEL:-gemma4:26b}"
EMBED_MODEL="${OLLAMA_EMBED_MODEL:-nomic-embed-text:latest}"

for bin in "$JUICE_BIN" "$AGENTD"; do
    [ -x "$bin" ] || { echo "missing binary: $bin (run 'make build')" >&2; exit 1; }
done

PASS=0; FAIL=0; SKIP=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $1 — $2"; FAIL=$((FAIL+1)); }
skip() { echo "  SKIP: $1 — $2"; SKIP=$((SKIP+1)); }

# require_ollama — true iff Ollama is up and the chat model is present. LLM flows
# guard on this and skip (not fail) when the model isn't installed.
require_ollama() {
    local tags; tags=$(curl -s "$OLLAMA_URL/api/tags" 2>/dev/null) || return 1
    echo "$tags" | python3 -c "import sys,json; ms=[m['name'] for m in json.load(sys.stdin).get('models',[])]; sys.exit(0 if '$CHAT_MODEL' in ms else 1)" 2>/dev/null
}

declare -a _PIDS=()
RUNROOT="$(mktemp -d)"
track() { _PIDS+=("$1"); }
cleanup() {
    for p in "${_PIDS[@]:-}"; do [ -n "$p" ] && kill -9 "$p" 2>/dev/null; done
    rm -rf "$RUNROOT" 2>/dev/null
}
trap cleanup EXIT INT TERM

new_dir() { mktemp -d -p "$RUNROOT"; }

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
# kernel (wired to real Ollama)
# ---------------------------------------------------------------------------
# start_kernel dir — boot a juice kernel using the real LLM, login @sys, fund it.
# Sets KDB, KCFG, KHOME, KURL.
start_kernel() {
    local dir="$1"
    KDB="$dir/juice.db"; KCFG="$dir/juice.json"; KHOME="$dir/khome"
    mkdir -p "$KHOME"
    cat > "$KCFG" <<EOF
{ "fee_bps": 0, "log_format": "json", "log_level": "info", "allow_local_sources": true,
  "kernel_handle": "@agent-test-kernel", "bootstrap_peers": [],
  "native": { "llm": { "url": "$OLLAMA_URL", "chat_model": "$CHAT_MODEL", "embed_model": "$EMBED_MODEL" } } }
EOF
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$KHOME" \
        "$JUICE_BIN" --config "$KCFG" --db "$KDB" serve --addr 127.0.0.1:0 >"$dir/kernel.log" 2>&1 &
    track $!
    local addr; addr=$(wait_line "$dir/kernel.log" '"msg":"server.ready".*"addr":"\([^"]*\)"')
    [ -n "$addr" ] || { echo "kernel did not start" >&2; cat "$dir/kernel.log" >&2; return 1; }
    KURL="http://$addr"
    k auth login @sys --password sys-pass >/dev/null 2>&1
    k admin deposit @sys 100000 --reason flows >/dev/null 2>&1
}

k()  { HOME="$KHOME" JUICE_SERVER="$KURL" "$JUICE_BIN" --config "$KCFG" --db "$KDB" "$@" 2>/dev/null; }
kj() { HOME="$KHOME" JUICE_SERVER="$KURL" "$JUICE_BIN" --config "$KCFG" --db "$KDB" --json "$@" 2>/dev/null; }

# create_agent — register @agent and fund it.
create_agent() {
    k user create @agent agent@example.com --password agent-pass >/dev/null 2>&1
    k admin deposit @agent 100000 --reason flows >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# agentd
# ---------------------------------------------------------------------------
# start_agentd dir [cfg-fragment] — write agent.json, launch the daemon, set
# ADATA and AHTTP (its http-channel address).
start_agentd() {
    local dir="$1" extra="${2:-}"
    ADATA="$dir/agent-data"
    local acfg="$dir/agent.json"
    cat > "$acfg" <<EOF
{ "server_url": "$KURL", "handle": "@agent", "data_dir": "$ADATA",
  "default_budget": 100, "max_turns": 12, "concurrency": 2,
  "lookup_limit": 8, "candidate_limit": 12, "compact_bytes": 262144, "rate_tasks": true,
  "channels": { "http": { "addr": "127.0.0.1:0" }, "step": { "poll_seconds": 1 } }
  ${extra:+, $extra} }
EOF
    AGENT_CONFIG="$acfg" AGENT_PASSWORD=agent-pass "$AGENTD" serve >"$dir/agentd.log" 2>&1 &
    AGENTD_PID=$!; track "$AGENTD_PID"
    local addr; addr=$(wait_line "$dir/agentd.log" '"event":"serving".*"http_addr":"\([^"]*\)"')
    [ -n "$addr" ] || { echo "agentd did not start" >&2; cat "$dir/agentd.log" >&2; return 1; }
    AHTTP="http://$addr"
}

# submit_http prompt [budget] — POST a task to agentd's http channel, echo task_id.
submit_http() {
    local prompt="$1" budget="${2:-0}"
    curl -sf -X POST "$AHTTP/task" -H 'Content-Type: application/json' \
        -d "{\"prompt\":$(json_str "$prompt"),\"budget\":$budget}" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['task_id'])" 2>/dev/null
}

# await_http task_id [timeout_s] — poll until the outcome is ready; echo its JSON.
# The timeout is generous: a real model takes tens of seconds per turn.
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

json_str() { python3 -c "import sys,json; print(json.dumps(sys.argv[1]))" "$1"; }
jfield()   { python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get(sys.argv[1],''))" "$1"; }
afield()   { python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('answer',{}).get(sys.argv[1],''))" "$1"; }

assert_eq()      { [ "$2" = "$3" ] && ok "$1" || fail "$1" "want [$2] got [$3]"; }
assert_le()      { [ "$2" -le "$3" ] && ok "$1" || fail "$1" "want $2 <= $3"; }
assert_contains(){ case "$3" in *"$2"*) ok "$1";; *) fail "$1" "want substring [$2] in [$3]";; esac; }
assert_nonempty(){ [ -n "$2" ] && ok "$1" || fail "$1" "expected non-empty"; }
assert_match()   { echo "$3" | grep -qE "$2" && ok "$1" || fail "$1" "want /$2/ in [$3]"; }
