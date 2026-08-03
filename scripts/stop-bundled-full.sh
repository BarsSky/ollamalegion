#!/usr/bin/env bash
# =============================================================================
# stop-bundled-full.sh — stop the bundled-full stack (Linux)
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY_DIR="$REPO_ROOT/deployments"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.bundled-full.yml"
ENV_FILE="$DEPLOY_DIR/.env.bundled-full"
PROJECT="ol-bundled-full"

CLEAN=false
CLEAN_IMAGES=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --clean) CLEAN=true; shift ;;
        --clean-images) CLEAN_IMAGES=true; shift ;;
        *) echo "Unknown arg: $1"; exit 1 ;;
    esac
done

echo "Stopping $PROJECT..."
ARGS=(-p "$PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" down --remove-orphans)
if $CLEAN; then
    ARGS+=(-v)
fi
docker compose "${ARGS[@]}"

if $CLEAN_IMAGES; then
    echo "Removing images..."
    docker rmi -f \
        ollama-legion/balancer:cppworker-bundled-full \
        ollama-legion/cppworker:gpu-arch_all \
        ollama-legion/agent:gpu-llamacpp \
        ollama-legion/webui:cppworker-bundled-full 2>/dev/null || true
fi

echo "✓ $PROJECT stopped"
