#!/bin/sh
# =============================================================================
# CppWorker Entrypoint — HuggingFace Hub Integration
# =============================================================================
# Поддерживает авто-загрузку GGUF моделей при старте контейнера.
#
# Переменные окружения:
#   HF_TOKEN              — токен HuggingFace (для gated моделей)
#   HF_AUTO_DOWNLOAD_REPO — репозиторий для авто-загрузки (напр. "bartowski/gemma-4-4b-it-GGUF")
#   HF_AUTO_DOWNLOAD_FILE — конкретный файл (напр. "gemma-4-4b-it-Q4_K_M.gguf")
#   HF_AUTO_DOWNLOAD_QUANT — фильтр квантизации при авто-выборе (по умолчанию "Q4_K_M")
#   HF_MIRROR             — зеркало HuggingFace (напр. "https://hf-mirror.com")
#   HF_HOME               — кэш HuggingFace (по умолчанию /app/.cache/huggingface)
#
# Пример docker run (GPU):
#   docker run --gpus all \
#     -e HF_TOKEN=hf_xxx \
#     -e HF_AUTO_DOWNLOAD_REPO=bartowski/gemma-4-4b-it-GGUF \
#     -e HF_AUTO_DOWNLOAD_FILE=gemma-4-4b-it-Q4_K_M.gguf \
#     -v ./models:/app/models \
#     ollama-legion/cppworker:gpu
#
# Пример docker run (CPU):
#   docker run \
#     -e HF_TOKEN=hf_xxx \
#     -e HF_AUTO_DOWNLOAD_REPO=bartowski/gemma-4-4b-it-GGUF \
#     -e HF_AUTO_DOWNLOAD_FILE=gemma-4-4b-it-Q4_K_M.gguf \
#     -v ./models:/app/models \
#     ollama-legion/cppworker:cpu
# =============================================================================

set -e

echo "=== CppWorker Entrypoint ==="
echo "HF_AUTO_DOWNLOAD_REPO=${HF_AUTO_DOWNLOAD_REPO:-<not set>}"
echo "HF_AUTO_DOWNLOAD_FILE=${HF_AUTO_DOWNLOAD_FILE:-<not set>}"
echo "HF_AUTO_DOWNLOAD_QUANT=${HF_AUTO_DOWNLOAD_QUANT:-Q4_K_M}"
echo "HF_MIRROR=${HF_MIRROR:-<not set>}"
echo "Models dir: ./models"

# ---- Auto-download model from HuggingFace Hub ----
if [ -n "${HF_AUTO_DOWNLOAD_REPO}" ]; then
    echo ""
    echo ">>> HuggingFace auto-download enabled <<<"

    # Login if token is provided
    if [ -n "${HF_TOKEN}" ]; then
        echo "Logging in to HuggingFace Hub..."
        python3 -c "from huggingface_hub import login; login(token='${HF_TOKEN}')" || true
    fi

    # Build download command
    DOWNLOAD_CMD="from huggingface_hub import hf_hub_download; import os"

    # Set mirror if configured
    if [ -n "${HF_MIRROR}" ]; then
        DOWNLOAD_CMD="${DOWNLOAD_CMD}; os.environ['HF_ENDPOINT']='${HF_MIRROR}'"
    fi

    if [ -n "${HF_AUTO_DOWNLOAD_FILE}" ]; then
        # Download specific file
        TARGET_FILE="${HF_AUTO_DOWNLOAD_FILE}"
        echo "Downloading specific file: ${TARGET_FILE} from ${HF_AUTO_DOWNLOAD_REPO}"
        DOWNLOAD_CMD="${DOWNLOAD_CMD}; path = hf_hub_download(repo_id='${HF_AUTO_DOWNLOAD_REPO}', filename='${TARGET_FILE}', local_dir='/app/models', local_dir_use_symlinks=False); print(f'Downloaded: {path}')"
    else
        # Auto-select: list files, filter by quant, pick smallest
        echo "Auto-selecting file with quantization ${HF_AUTO_DOWNLOAD_QUANT}..."
        QUANT="${HF_AUTO_DOWNLOAD_QUANT}"
        DOWNLOAD_CMD="${DOWNLOAD_CMD}
from huggingface_hub import list_repo_files
files = list_repo_files('${HF_AUTO_DOWNLOAD_REPO}')
gguf_files = [f for f in files if f.endswith('.gguf')]
quant_files = [f for f in gguf_files if '${QUANT}'.upper() in f.upper() or '${QUANT}'.lower() in f.lower()]
target = quant_files[0] if quant_files else (gguf_files[0] if gguf_files else None)
if target is None:
    raise RuntimeError('No .gguf files found in ${HF_AUTO_DOWNLOAD_REPO}')
print(f'Auto-selected: {target}')
path = hf_hub_download(repo_id='${HF_AUTO_DOWNLOAD_REPO}', filename=target, local_dir='/app/models', local_dir_use_symlinks=False)
print(f'Downloaded: {path}')"
    fi

    python3 -c "${DOWNLOAD_CMD}"

    echo ">>> Download complete. Listing /app/models/:"
    ls -lh /app/models/
    echo ""
fi

# ---- Cross-sync env vars so Go auto-registration picks up BALANCER_URL ----
# Go-side (cmd/cppworker/balancer_register.go) reads CPPWORKER_BALANCER_URL;
# the legacy shell-script reads BALANCER_URL. Accept both names and unify.
export CPPWORKER_BALANCER_URL="${CPPWORKER_BALANCER_URL:-${BALANCER_URL:-}}"
export CPPWORKER_BALANCER_TOKEN="${CPPWORKER_BALANCER_TOKEN:-${BALANCER_API_TOKEN:-}}"

# ---- Launch CppWorker in background for auto-registration ----
echo "Starting CppWorker with args: $@"
./cppworker "$@" &
CPPWORKER_PID=$!

# ---- Auto-register with balancer (if BALANCER_URL is set) ----
if [ -n "${BALANCER_URL}" ] || [ -n "${CPPWORKER_BALANCER_URL}" ]; then
    echo ""
    echo ">>> Auto-registration with balancer enabled <<<"
    echo "BALANCER_URL=${BALANCER_URL:-<not set>}"
    echo "CPPWORKER_BALANCER_URL=${CPPWORKER_BALANCER_URL:-<not set>}"
    echo "CPPWORKER_PORT=${CPPWORKER_PORT:-18092}"

    # Wait for CppWorker to become ready BEFORE running registration
    echo "[entrypoint] Waiting for CppWorker to be ready (up to 900s)..."
    ATTEMPT=0
    MAX_ATTEMPTS=900
    while [ ${ATTEMPT} -lt ${MAX_ATTEMPTS} ]; do
        if curl -sf "http://127.0.0.1:${CPPWORKER_PORT:-18092}/health" >/dev/null 2>&1; then
            echo "[entrypoint] CppWorker is ready after ${ATTEMPT}s"
            break
        fi
        sleep 1
        ATTEMPT=$((ATTEMPT + 1))
        if [ $((ATTEMPT % 30)) -eq 0 ]; then
            echo "[entrypoint]   still waiting (${ATTEMPT}s/${MAX_ATTEMPTS}s)..."
        fi
    done

    if [ ${ATTEMPT} -ge ${MAX_ATTEMPTS} ]; then
        echo "[entrypoint] WARNING: CppWorker did not become ready within ${MAX_ATTEMPTS}s, attempting registration anyway..."
    fi

    # Run registration in background with retry loop (up to 5 attempts)
    # If registration fails, retry after 15 seconds
    (
        MAX_REG_RETRIES=5
        REG_RETRY_DELAY=15
        for reg_attempt in $(seq 1 ${MAX_REG_RETRIES}); do
            echo "[entrypoint] Registration attempt ${reg_attempt}/${MAX_REG_RETRIES}..."
            if /register-with-balancer.sh; then
                echo "[entrypoint] Registration succeeded on attempt ${reg_attempt}"
                break
            fi
            if [ ${reg_attempt} -lt ${MAX_REG_RETRIES} ]; then
                echo "[entrypoint] Registration failed, retrying in ${REG_RETRY_DELAY}s..."
                sleep ${REG_RETRY_DELAY}
            else
                echo "[entrypoint] WARNING: All ${MAX_REG_RETRIES} registration attempts failed"
            fi
        done
    ) &
fi

# ---- Wait for cppworker to finish ----
wait ${CPPWORKER_PID}
