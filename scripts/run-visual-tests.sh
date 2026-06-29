#!/usr/bin/env bash
# OllamaLegion — Playwright visual-regression runner (bash).
#
# Usage:
#   ./scripts/run-visual-tests.sh              # compare against baseline
#   ./scripts/run-visual-tests.sh --update     # regenerate baseline
#   ./scripts/run-visual-tests.sh --install    # first-time setup
#   ./scripts/run-visual-tests.sh --install-browsers
#   BASE_URL=http://localhost:18081 ./scripts/run-visual-tests.sh
#
# Required tools: node (>= 18), npm.
# First-time use:
#   ./scripts/run-visual-tests.sh --install-browsers
# (downloads ~280 MB of Chromium for Playwright)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$REPO_ROOT"

cmd="${1:-run}"

case "$cmd" in
    --install)
        echo "[run-visual-tests] installing npm dependencies…"
        npm install --no-audit --no-fund
        echo "[run-visual-tests] installing Playwright browsers (chromium)…"
        npx playwright install chromium
        echo "[run-visual-tests] done."
        ;;
    --install-browsers)
        echo "[run-visual-tests] installing Playwright browsers (chromium)…"
        npx playwright install chromium
        echo "[run-visual-tests] done."
        ;;
    --update)
        echo "[run-visual-tests] regenerating baseline screenshots…"
        npx playwright test --config=playwright.config.js --update-snapshots
        ;;
    --help|-h)
        sed -n '3,20p' "$0"
        ;;
    run|"")
        echo "[run-visual-tests] running visual-regression suite…"
        npx playwright test --config=playwright.config.js
        ;;
    *)
        echo "Unknown command: $cmd" >&2
        echo "Run '$0 --help' for usage." >&2
        exit 64
        ;;
esac