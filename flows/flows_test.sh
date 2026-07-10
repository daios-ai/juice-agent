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
source "$here/flows_agent.sh"

# Mechanism flows (no LLM) run first and fast; LLM flows (real Ollama) follow.
ALL=(flow_bootstrap flow_recovery flow_drain
     flow_task flow_role_law flow_rating flow_maker)

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
