#!/usr/bin/env bash
# =============================================================================
# start-bundled-full.sh — Linux equivalent of start-bundled-full.ps1
# =============================================================================
# Usage:
#   ./scripts/start-bundled-full.sh
#   ./scripts/start-bundled-full.sh --rebuild
#   ./scripts/start-bundled-full.sh --foreground
# =============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY_DIR="$REPO_ROOT/deployments"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.bundled-full.yml"
ENV_FILE="$DEPLOY_DIR/.env.bundled-full"
PROJECT="ol-bundled-full"

REBUILD=false
DETACHED=true
FOREGROUND=false
NO_BUILD=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --rebuild) REBUILD=true; shift ;;
        --detached) DETACHED=true; shift ;;
        --foreground) FOREGROUND=true; DETACHED=false; shift ;;
        --no-build) NO_BUILD=true; shift ;;
        *) echo "Unknown arg: $1"; exit 1 ;;
    esac
done

echo "=================================================="
echo "OllamaLegion bundled-full stack (Linux)"
echo "=================================================="
echo "Repo:    $REPO_ROOT"
echo "Compose: $COMPOSE_FILE"
echo "Env:     $ENV_FILE"
echo

# 1. Pre-flight
echo "[1/4] Pre-flight checks..."
if ! docker info >/dev/null 2>&1; then
    echo "  ✗ Docker daemon NOT accessible"
    exit 1
fi
echo "  ✓ Docker daemon running"

if [[ ! -f "$COMPOSE_FILE" ]]; then
    echo "  ✗ Compose file not found: $COMPOSE_FILE"
    exit 1
fi
echo "  ✓ Compose file exists"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "  ⚠ Env file not found: $ENV_FILE (using inline defaults from compose)"
fi
echo "  ✓ Env ready"

# 2. Stop existing
echo
echo "[2/4] Stopping existing $PROJECT (if any)..."
docker compose -p "$PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" down --remove-orphans 2>&1 || true

# 3. Start
echo
echo "[3/4] Starting $PROJECT..."
COMPOSE_ARGS=(-p "$PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up)
if $DETACHED && ! $FOREGROUND; then
    COMPOSE_ARGS+=(-d)
fi
if $REBUILD || (! $NO_BUILD); then
    COMPOSE_ARGS+=(--build)
fi

echo "  docker compose ${COMPOSE_ARGS[*]}"
docker compose "${COMPOSE_ARGS[@]}"

# 4. Wait for health
echo
echo "[4/4] Waiting for services to be healthy..."
HEALTHY=0
for i in $(seq 1 90); do
    sleep 5
    ALL_HEALTHY=true
    for c in ol-bundled-full-balancer ol-bundled-full-cppworker-gpu ol-bundled-full-cppworker-gpu-agent ol-bundled-full-webui; do
        STATUS=$(docker inspect --format '{{.State.Health.Status}}' "$c" 2>/dev/null || echo "missing")
        RUNNING=$(docker inspect --format '{{.State.Running}}' "$c" 2>/dev/null || echo "false")
        if [[ "$RUNNING" != "true" || "$STATUS" != "healthy" ]]; then
            ALL_HEALTHY=false
            break
        fi
    done
    if $ALL_HEALTHY; then
        HEALTHY=4
        break
    fi
done

if [[ $HEALTHY -eq 4 ]]; then
    echo "  ✓ All 4 services healthy"
else
    echo "  ⚠ Some services not yet healthy"
fi

# 5. Status
echo
echo "=================================================="
echo "✓ Stack started"
echo "=================================================="
echo "  WebUI:      http://localhost:18083"
echo "  Ollama API: http://localhost:18080"
echo "  Admin:      http://localhost:18081"
echo "  cppworker:  http://localhost:18092"
echo
echo "Running containers:"
docker ps --filter "name=$PROJECT" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
echo
echo "Smoke test:"
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' "$ENV_FILE" | cut -d= -f2-)
if curl -sS --max-time 10 -H "Authorization: Bearer $TOKEN" http://localhost:18080/v1/models >/dev/null 2>&1; then
    echo "  ✓ /v1/models responding"
else
    echo "  ⚠ /v1/models not yet responding"
fi
echo
echo "To stream logs: docker compose -p $PROJECT -f $COMPOSE_FILE logs -f"
echo "To stop:        ./scripts/stop-bundled-full.sh"
