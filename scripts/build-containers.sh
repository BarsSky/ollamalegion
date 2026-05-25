#!/bin/bash
# =============================================================================
# Ollama Legion - sequential Docker build (Linux)
# =============================================================================
# Usage:
#   ./build-containers.sh                   - build all 3 containers
#   ./build-containers.sh --no-cache        - build without cache
#   ./build-containers.sh loadbalancer      - build a single service
#   ./build-containers.sh agent             - build CPU agent
#   ./build-containers.sh agent-gpu         - build GPU agent
#   ./build-containers.sh all-agent         - build both agents
# =============================================================================
set -e

NO_CACHE=""

# Parse --no-cache flag
if [[ "${1:-}" == "--no-cache" ]]; then
    NO_CACHE="--no-cache"
    shift
fi

TARGET="${1:-all}"

# Select compose file based on target
case "$TARGET" in
    cppworker)
        COMPOSE_FILE="deployments/docker-compose.cppworker.yml"
        ;;
    agent|agent-gpu|all-agent)
        COMPOSE_FILE="deployments/docker-compose.agent.yml"
        ;;
    *)  COMPOSE_FILE="deployments/docker-compose.yml"
        ;;
esac

# Select services
case "$TARGET" in
    all)          SERVICES=(loadbalancer webui) ;;
    loadbalancer) SERVICES=(loadbalancer) ;;
    webui)        SERVICES=(webui) ;;
    cppworker)    SERVICES=(cppworker) ;;
    agent)        SERVICES=(agent) ;;
    agent-gpu)    SERVICES=(agent-gpu) ;;
    all-agent)    SERVICES=(agent agent-gpu) ;;
    *)
        echo "ERROR: unknown service '$TARGET'. Use: loadbalancer, webui, cppworker, agent, agent-gpu, all, all-agent."
        exit 1
        ;;
esac

echo ""
echo "======================================================================"
echo "        Ollama Legion - Sequential Container Build"
echo "======================================================================"
echo ""
echo "Services: ${SERVICES[*]}"
echo "Cache:    ${NO_CACHE:-enabled}"
echo "Compose:  ${COMPOSE_FILE}"
echo ""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_ROOT"

START_TIME=$(date +%H:%M:%S)
FAILED=0

for SVC in "${SERVICES[@]}"; do
    echo ""
    echo "======================================================================"
    NOW=$(date +%H:%M:%S)
    echo "[$SVC] Build started at $NOW"
    echo "======================================================================"

    DOCKER_BUILDKIT=0 docker compose -f "$COMPOSE_FILE" build $NO_CACHE "$SVC" || {
        NOW=$(date +%H:%M:%S)
        echo ""
        echo "[$SVC] BUILD FAILED at $NOW - stopping."
        FAILED=1
        break
    }

    NOW=$(date +%H:%M:%S)
    echo ""
    echo "[$SVC] Build OK at $NOW"
done

END_TIME=$(date +%H:%M:%S)

echo ""
echo "======================================================================"
if [[ "$FAILED" -eq 1 ]]; then
    echo " BUILD STOPPED due to error. Started: $START_TIME, ended: $END_TIME"
else
    echo " ALL CONTAINERS BUILT. Started: $START_TIME, ended: $END_TIME"
fi
echo "======================================================================"

echo ""
echo "Built images:"
docker images --filter "reference=ollama-legion/*" --format "  {{.Repository}}:{{.Tag}}  {{.Size}}" 2>/dev/null

echo ""
if [[ "$FAILED" -eq 1 ]]; then
    echo "Retry the failed service and remaining ones:"
    echo "  ./build-containers.sh $NO_CACHE $TARGET"
    echo ""
    echo 'If you see "runc run failed: container process is already dead":'
    echo "  Increase Docker memory limit (Docker Desktop) or system swap."
    exit 1
fi

echo "All containers built. To start:"
echo "  docker compose -f $COMPOSE_FILE up -d"
echo ""