#!/bin/sh
# =============================================================================
# CppWorker Auto-Registration Script
# =============================================================================
# Регистрирует cppworker в балансере после его запуска.
# Вызывается из entrypoint.sh после старта cppworker.
# При already exists — обновляет бэкенд через PUT.

set -e

BALANCER_URL="${BALANCER_URL:-}"
CPPWORKER_HOST="${CPPWORKER_HOST:-cppworker}"
CPPWORKER_PORT="${CPPWORKER_PORT:-18091}"
CPPWORKER_ADVERTISED_PORT="${CPPWORKER_ADVERTISED_PORT:-${CPPWORKER_PORT}}"
CPPWORKER_NAME="${CPPWORKER_NAME:-CppWorker}"
CPPWORKER_BACKEND_ID="${CPPWORKER_BACKEND_ID:-cppworker-gpu}"
CPPWORKER_MAX_CONCURRENT="${CPPWORKER_MAX_CONCURRENT:-4}"
CPPWORKER_MAX_MODELS="${CPPWORKER_MAX_MODELS:-3}"
CPPWORKER_GPU_MODE="${CPPWORKER_GPU_MODE:-auto}"
CPPWORKER_LABELS="${CPPWORKER_LABELS:-linux,amd64,llamacpp}"
BALANCER_API_TOKEN="${BALANCER_API_TOKEN:-}"
MAX_RETRIES=150
RETRY_DELAY=2

CPPWORKER_ADVERTISED_HOST="${CPPWORKER_ADVERTISED_HOST:-${CPPWORKER_HOST:-cppworker}}"

if [ -z "${BALANCER_URL}" ]; then
    echo "[register] BALANCER_URL not set, skipping auto-registration"
    exit 0
fi

echo "[register] Waiting for cppworker to be ready..."
for i in $(seq 1 ${MAX_RETRIES}); do
    if curl -sf "http://127.0.0.1:${CPPWORKER_PORT}/health" > /dev/null 2>&1; then
        echo "[register] CppWorker is ready (port ${CPPWORKER_PORT})"
        break
    fi
    if [ $i -eq ${MAX_RETRIES} ]; then
        echo "[register] ERROR: CppWorker did not become ready within $((MAX_RETRIES * RETRY_DELAY))s"
        exit 1
    fi
    echo "[register]   attempt ${i}/${MAX_RETRIES} - waiting ${RETRY_DELAY}s..."
    sleep ${RETRY_DELAY}
done

echo "[register] Waiting for balancer API to be ready..."
for i in $(seq 1 ${MAX_RETRIES}); do
    if curl -sf "${BALANCER_URL}/api/v1/health" > /dev/null 2>&1; then
        echo "[register] Balancer API is ready (${BALANCER_URL})"
        break
    fi
    if [ $i -eq ${MAX_RETRIES} ]; then
        echo "[register] WARNING: Balancer API not reachable after $((MAX_RETRIES * RETRY_DELAY))s, will retry registration"
    fi
    echo "[register]   attempt ${i}/${MAX_RETRIES} - waiting ${RETRY_DELAY}s..."
    sleep ${RETRY_DELAY}
done

# Convert labels string to JSON array
LABELS_JSON="["
FIRST=true
OLD_IFS="${IFS}"
IFS=","
for label in ${CPPWORKER_LABELS}; do
    label=$(echo "$label" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    if [ -n "$label" ]; then
        if [ "$FIRST" = true ]; then
            FIRST=false
        else
            LABELS_JSON="${LABELS_JSON}, "
        fi
        LABELS_JSON="${LABELS_JSON}\"${label}\""
    fi
done
IFS="${OLD_IFS}"
LABELS_JSON="${LABELS_JSON}]"

# Use advertised port for registration (external port visible to balancer)
REGISTER_CPP_PORT="${CPPWORKER_ADVERTISED_PORT}"
echo "[register] Using cppWorkerPort=${REGISTER_CPP_PORT} (advertised=${CPPWORKER_ADVERTISED_PORT}, internal=${CPPWORKER_PORT})"

REGISTER_JSON=$(cat <<EOF
{
  "id": "${CPPWORKER_BACKEND_ID}",
  "name": "${CPPWORKER_NAME}",
  "host": "${CPPWORKER_HOST}",
  "ollamaPort": 0,
  "agentPort": 0,
  "cppWorkerPort": ${REGISTER_CPP_PORT},
  "weight": 10,
  "maxConcurrentRequests": ${CPPWORKER_MAX_CONCURRENT},
  "maxModels": ${CPPWORKER_MAX_MODELS},
  "labels": ${LABELS_JSON},
  "backendType": "llama_cpp",
  "backendEngine": "llama_cpp",
  "gpuMode": "${CPPWORKER_GPU_MODE}"
}
EOF
)

# Helper: build auth header
build_auth_header() {
    if [ -n "${BALANCER_API_TOKEN}" ]; then
        printf '%s' "X-API-Token: ${BALANCER_API_TOKEN}"
    fi
}

# Helper: perform registration POST
do_register() {
    AUTH_HDR=$(build_auth_header)
    if [ -n "${AUTH_HDR}" ]; then
        curl -s -w "%{http_code}" \
            -H "Content-Type: application/json" \
            -H "${AUTH_HDR}" \
            -d "${REGISTER_JSON}" \
            -o /tmp/register_response.txt \
            "${BALANCER_URL}/api/v1/backends"
    else
        curl -s -w "%{http_code}" \
            -H "Content-Type: application/json" \
            -d "${REGISTER_JSON}" \
            -o /tmp/register_response.txt \
            "${BALANCER_URL}/api/v1/backends"
    fi
}

# Helper: perform update PUT
do_update() {
    AUTH_HDR=$(build_auth_header)
    if [ -n "${AUTH_HDR}" ]; then
        curl -s -w "%{http_code}" \
            -X PUT \
            -H "Content-Type: application/json" \
            -H "${AUTH_HDR}" \
            -d "${REGISTER_JSON}" \
            -o /tmp/update_response.txt \
            "${BALANCER_URL}/api/v1/backends/${CPPWORKER_BACKEND_ID}"
    else
        curl -s -w "%{http_code}" \
            -X PUT \
            -H "Content-Type: application/json" \
            -d "${REGISTER_JSON}" \
            -o /tmp/update_response.txt \
            "${BALANCER_URL}/api/v1/backends/${CPPWORKER_BACKEND_ID}"
    fi
}

echo "[register] Registering cppworker in balancer..."
echo "[register]   URL:  ${BALANCER_URL}/api/v1/backends"
echo "[register]   JSON: ${REGISTER_JSON}"

HTTP_CODE=$(do_register)
RESPONSE=$(cat /tmp/register_response.txt 2>/dev/null || echo "")
echo "[register] Response: HTTP ${HTTP_CODE}"
echo "[register] Body: ${RESPONSE}"

if [ "${HTTP_CODE}" -ge 200 ] && [ "${HTTP_CODE}" -lt 300 ]; then
    echo "[register] OK CppWorker registered successfully in balancer"
elif [ "${HTTP_CODE}" -eq 409 ] || echo "${RESPONSE}" | grep -qi "already exists"; then
    echo "[register] Backend already exists, updating via PUT..."
    HTTP_CODE2=$(do_update)
    RESPONSE2=$(cat /tmp/update_response.txt 2>/dev/null || echo "")
    echo "[register] Update response: HTTP ${HTTP_CODE2}"
    echo "[register] Update body: ${RESPONSE2}"
    if [ "${HTTP_CODE2}" -ge 200 ] && [ "${HTTP_CODE2}" -lt 300 ]; then
        echo "[register] OK CppWorker updated successfully in balancer"
    else
        echo "[register] FAILED Update failed with HTTP ${HTTP_CODE2}"
        echo "[register]    Response: ${RESPONSE2}"
        rm -f /tmp/register_response.txt /tmp/update_response.txt
        exit 1
    fi
    rm -f /tmp/update_response.txt
else
    echo "[register] FAILED Registration failed with HTTP ${HTTP_CODE}"
    echo "[register]    Response: ${RESPONSE}"
    rm -f /tmp/register_response.txt
    exit 1
fi

rm -f /tmp/register_response.txt
echo "[register] Auto-registration complete"