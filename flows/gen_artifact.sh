#!/usr/bin/env bash
# Regenerate cmd/juice-agent/reply.wasm.b64: the one-line echo Handle compiled through
# a throwaway kernel's @sys/tinygo/compile. Run via `make artifact`.
set -euo pipefail

JUICE_BIN="${JUICE_BIN:-../juice/juice}"
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'kill $(cat "$work/pid" 2>/dev/null) 2>/dev/null || true; rm -rf "$work"' EXIT

cat > "$work/juice.json" <<'EOF'
{ "fee_bps": 0, "log_format": "json", "log_level": "info", "allow_local_sources": true,
  "kernel_handle": "@gen", "bootstrap_peers": [] }
EOF

JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$work" \
    "$JUICE_BIN" --config "$work/juice.json" --db "$work/juice.db" serve --addr 127.0.0.1:0 >"$work/log" 2>&1 &
echo $! > "$work/pid"

for _ in $(seq 1 100); do
    addr=$(sed -n 's/.*"msg":"server.ready".*"addr":"\([^"]*\)".*/\1/p' "$work/log" 2>/dev/null | head -1)
    [ -n "$addr" ] && break
    sleep 0.1
done
[ -n "${addr:-}" ] || { echo "kernel did not start"; cat "$work/log"; exit 1; }

export JUICE_SERVER="http://$addr"
J() { HOME="$work" "$JUICE_BIN" --config "$work/juice.json" --db "$work/juice.db" "$@"; }
J auth login @sys --password sys-pass >/dev/null 2>&1
J admin deposit @sys 1000 --reason gen >/dev/null 2>&1

printf '{"source":"func Handle(in map[string]any) (map[string]any, error) {\\n\\treturn in, nil\\n}\\n"}' > "$work/args.json"
J --json run @sys/tinygo/compile "@$work/args.json" > "$work/out.json" 2>"$work/err"
python3 -c "import json,sys; d=json.load(open('$work/out.json'))['result']; \
assert d['status']=='success', d.get('diagnostics'); \
open('$here/cmd/juice-agent/reply.wasm.b64','w').write(d['artifact']); \
print('wrote cmd/juice-agent/reply.wasm.b64', len(d['artifact']), 'bytes, hash', d['artifact_hash'][:12])"
