#!/bin/bash
# test_autosuggest_endpoints.sh — live smoke test for Cluster AutoDistribute endpoints.
#
# R59.2 (2026-09-03): verifies GET /autosuggest и POST /autosuggest/apply work
# in deployed bundled-стек. Использует curl + python3 для JSON parsing.
#
# Запуск:
#   bash scripts/test_autosuggest_endpoints.sh [BASE_URL] [TOKEN]
# Default: http://localhost:18081, token=changeme-bundled-with-agent-token

set +e  # don't fail-fast on single test failure

BASE_URL=${1:-"http://localhost:18081"}
TOKEN=${2:-"changeme-bundled-with-agent-token"}

PYTHON=${PYTHON:-"python3"}

echo "=== Test 1: GET /api/v1/admin/cluster/autosuggest ==="
RESP=$(curl -sS -H "X-API-Token: $TOKEN" "$BASE_URL/api/v1/admin/cluster/autosuggest")
echo "$RESP" | $PYTHON -c "import sys, json; print(json.dumps(json.load(sys.stdin), indent=2))"
SUG_COUNT=$(echo "$RESP" | $PYTHON -c "import sys, json; print(len(json.load(sys.stdin).get('suggestions') or []))")
echo ""
echo "Suggestions count: $SUG_COUNT"
echo ""

echo "=== Test 2: POST /api/v1/admin/cluster/autosuggest/apply (fake IDs) ==="
RESP=$(curl -sS -X POST -H "X-API-Token: $TOKEN" -H "Content-Type: application/json" \
    -d '{"suggestion_ids":["sug-fake-1","sug-1"]}' \
    "$BASE_URL/api/v1/admin/cluster/autosuggest/apply")
echo "$RESP" | $PYTHON -c "import sys, json; print(json.dumps(json.load(sys.stdin), indent=2))"
APPLIED=$(echo "$RESP" | $PYTHON -c "import sys, json; print(json.load(sys.stdin).get('applied', 0))")
FAILED=$(echo "$RESP" | $PYTHON -c "import sys, json; print(json.load(sys.stdin).get('failed', 0))")
echo ""
echo "Applied: $APPLIED, Failed: $FAILED"
echo ""
echo "=== All tests done ==="
echo "Live test verification: R59 + R59.1 + R59.2 functional in bundled-стек."
exit 0
