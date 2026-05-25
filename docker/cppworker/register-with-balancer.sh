#!/bin/sh
# =============================================================================
# CppWorker Auto-Registration Script
# =============================================================================
# Регистрирует cppworker в балансере после его запуска.
# Вызывается из entrypoint.sh после старта cppworker.
#
# Переменные окружения:
#   BALANCER_URL          — URL API балансера (напр. http://loadbalancer:18081)
#   CPPWORKER_HOST        — хост, по которому балансер может достучаться до cppworker
#   CPPWORKER_PORT        — порт cppworker (по умолчанию 18091)
#   CPPWORKER_NAME        — имя бэкенда (по умолчанию "cppworker")
#   CPPWORKER_BACKEND_ID  — ID бэкенда (по умолчанию "cppworker-gpu")
#   CPPWORKER_MAX_CONCURRENT — макс. одновременных запросов (по умолчанию 4)
#   CPPWORKER_MAX_MODELS  — макс. моделей (по умолчанию 3)
#   CPPWORKER_GPU_MODE    — режим GPU: auto/cpu/cuda (по умолчанию "auto")
#   CPPWORKER_LABELS      — метки через запятую (по умолчанию "linux,amd64,llamacpp")
#   BALANCER_API_TOKEN    — токен API балансера (если требуется)
# =============================================================================

set -e

BALANCER_URL="${BALANCER_URL:-}"
CPPWORKER_HOST="${CPPWORKER_HOST:-cppworker}"
CPPWORKER_PORT="${CPPWORKER_PORT:-18091}"
CPPWORKER_NAME="${CPPWORKER_NAME:-CppWorker}"
CPPWORKER_BACKEND_ID="${CPPWORKER_BACKEND_ID:-cppworker-gpu}"
CPPWORKER_MAX_CONCURRENT="${CPPWORKER_MAX_CONCURRENT:-4}"
CPPWORKER_MAX_MODELS="${CPPWORKER_MAX_MODELS:-3}"
CPPWORKER_GPU_MODE="${CPPWORKER_GPU_MODE:-auto}"
CPPWORKER_LABELS="${CPPWORKER_LABELS:-linux,amd64,llamacpp}"
BALANCER_API_TOKEN="${BALANCER_API_TOKEN:-}"
MAX_RETRIES=30
RETRY_DELAY=2

CPPWORKER_ADVERTISED_HOST="${CPPWORKER_ADVERTISED_HOST:-${CPPWORKER_HOST:-cppworker}}"

if [ -z "${BALANCER_URL}" ]; then
    echo "[register] BALANCER_URL not set, skipping auto-registration"
    exit 0
fi

echo "[register] Waiting for cppworker to be ready..."
for i in $(seq 1 ${MAX_RETRIES}); do
    if curl -s -f "http://127.0.0.1:${CPPWORKER_PORT}/health" > /dev/null 2>&1; then
        echo "[register] CppWorker is ready (port ${CPPWORKER_PORT})"
        break
    fi
    if [ $i -eq ${MAX_RETRIES} ]; then
        echo "[register] ERROR: CppWorker did not become ready within $((MAX_RETRIES * RETRY_DELAY))s"
        exit 1
    fi
    echo "[register]   attempt ${i}/${MAX_RETRIES} — waiting ${RETRY_DELAY}s..."
    sleep ${RETRY_DELAY}
done

echo "[register] Waiting for balancer API to be ready..."
for i in $(seq 1 ${MAX_RETRIES}); do
    if curl -s -f "${BALANCER_URL}/api/v1/health" > /dev/null 2>&1; then
        echo "[register] Balancer API is ready (${BALANCER_URL})"
        break
    fi
    if [ $i -eq ${MAX_RETRIES} ]; then
        echo "[register] WARNING: Balancer API not reachable after $((MAX_RETRIES * RETRY_DELAY))s, will retry registration"
    fi
    echo "[register]   attempt ${i}/${MAX_RETRIES} — waiting ${RETRY_DELAY}s..."
    sleep ${RETRY_DELAY}
done

# Конвертируем метки из строки с запятыми в JSON-массив
LABELS_JSON="["
FIRST=true
OLD_IFS="${IFS}"
IFS=","
for label in ${CPPWORKER_LABELS}; do
    # trim whitespace
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

# Формируем JSON для регистрации
REGISTER_JSON=$(cat <<EOF
{
  "id": "${CPPWORKER_BACKEND_ID}",
  "name": "${CPPWORKER_NAME}",
  "host": "${CPPWORKER_HOST}",
  "ollamaPort": 0,
  "agentPort": 0,
  "cppWorkerPort": ${CPPWORKER_PORT},
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

echo "[register] Registering cppworker in balancer..."
echo "[register]   URL:  ${BALANCER_URL}/api/v1/backends"
echo "[register]   JSON: ${REGISTER_JSON}"

AUTH_HEADER=""
if [ -n "${BALANCER_API_TOKEN}" ]; then
    AUTH_HEADER="-H 'X-API-Token: ${BALANCER_API_TOKEN}'"
fi

# Отправляем запрос на регистрацию
HTTP_CODE=$(curl -s -o /tmp/register_response.txt -w "%{http_code}" \
    -X POST \
    -H "Content-Type: application/json" \
    ${AUTH_HEADER:+-H "X-API-Token: ${BALANCER_API_TOKEN}"} \
    -d "${REGISTER_JSON}" \
    "${BALANCER_URL}/api/v1/backends" 2>&1)

RESPONSE=$(cat /tmp/register_response.txt)
echo "[register] Response: HTTP ${HTTP_CODE}"
echo "[register] Body: ${RESPONSE}"

if [ "${HTTP_CODE}" -ge 200 ] && [ "${HTTP_CODE}" -lt 300 ]; then
    echo "[register] ✅ CppWorker registered successfully in balancer"
else
    # Проверяем, может бэкенд уже существует (409 Conflict / 200 с ошибкой)
    if echo "${RESPONSE}" | grep -qi "already exists"; then
        echo "[register] ⚠️  Backend already exists, this is OK"
    else
        echo "[register] ❌ Registration failed with HTTP ${HTTP_CODE}"
        echo "[register]    Response: ${RESPONSE}"
    fi
fi

rm -f /tmp/register_response.txt
echo "[register] Auto-registration complete"