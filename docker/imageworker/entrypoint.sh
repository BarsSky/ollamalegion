#!/bin/sh
# =============================================================================
# ImageWorker (sdworker) Entrypoint
# =============================================================================
# Воркер сам управляет субпроцессом sd-server (spawn/kill), поэтому entrypoint
# намеренно ТОНКИЙ: привести пути, пробросить токены/URL балансера и запустить
# sdworker на переднем плане.
#
# ЧТО ЗДЕСЬ ЕСТЬ ПО СРАВНЕНИЮ С cppworker/entrypoint.sh:
#   - НЕТ авто-скачивания модели при старте. Bundle image-модели — это НАБОР
#     файлов (diffusion + VAE + text encoders, 2–12 GB), его раскладывает
#     Phase 2 (HF-слой) в <models/image>/<имя>/. Скачивать «один .gguf» здесь
#     означало бы создать bundle, который движок не сможет загрузить;
#   - НЕТ curl-ожидания health: sdworker сам регистрируется в балансере
#     Go-кодом (cmd/sdworker/balancer_register.go) и ретраит сам.
#
# Переменные окружения (см. internal/sdbackend/config.go):
#   SDWORKER_PORT                  — порт API воркера (18093)
#   SDWORKER_IMAGE_MODELS_DIR      — каталог bundle'ов (<dir>/<model>/profile.json)
#   SDWORKER_SD_SERVER_BIN         — путь к sd-server
#   SDWORKER_SD_SERVER_PORT        — внутренний порт sd-server (loopback)
#   SDWORKER_IDLE_UNLOAD_MINUTES   — выгрузка простаивающей модели (0 = выкл)
#   SDWORKER_MAX_CONCURRENT        — размер очереди генераций (по умолчанию 64)
#   SDWORKER_PRELOAD_MODEL         — загрузить модель при старте
#   SDWORKER_BASE_URL              — внешний URL воркера (для response_format:url)
#   SDWORKER_BALANCER_URL          — авторегистрация в балансере
#   SDWORKER_BACKEND_ID            — id бэкенда в балансере
#   SDWORKER_ADVERTISE_HOST        — host, под которым воркер виден балансеру
#   SDWORKER_BALANCER_TOKEN        — токен балансера (X-API-Token)
#   SDWORKER_REGISTER_DISABLE      — true = выключить Go-side регистрацию
#   HF_TOKEN / HF_MIRROR           — для HF-слоя (Phase 2) и ручных скриптов
#
# Пример docker run (Vulkan, AMD):
#   docker run --device /dev/dri --group-add video --group-add render \
#     -e SDWORKER_BASE_URL=http://192.168.1.10:18093 \
#     -v ./models/image:/app/models/image \
#     -p 18093:18093 ollama-legion/imageworker:vulkan
# =============================================================================

set -e

echo "=== ImageWorker (sdworker) Entrypoint ==="
echo "port                 : ${SDWORKER_PORT:-18093}"
echo "models dir           : ${SDWORKER_IMAGE_MODELS_DIR:-/app/models/image}"
echo "sd-server bin        : ${SDWORKER_SD_SERVER_BIN:-/app/sd-server/sd-server}"
echo "sd-server port       : ${SDWORKER_SD_SERVER_PORT:-18094}"
echo "idle unload (min)    : ${SDWORKER_IDLE_UNLOAD_MINUTES:-30}"
echo "max concurrent       : ${SDWORKER_MAX_CONCURRENT:-64}"
echo "preload model        : ${SDWORKER_PRELOAD_MODEL:-<none>}"
echo "base url             : ${SDWORKER_BASE_URL:-<not set>}"
echo "balancer url         : ${SDWORKER_BALANCER_URL:-${BALANCER_URL:-<not set>}}"

# ---- Проверка наличия движка: без него воркер поднимется, но генерация даст
# ---- понятную ошибку на первом запросе. Лучше сказать об этом сразу в логе.
if [ ! -x "${SDWORKER_SD_SERVER_BIN:-/app/sd-server/sd-server}" ]; then
    echo "[entrypoint] WARNING: sd-server не найден или не исполняем: ${SDWORKER_SD_SERVER_BIN:-/app/sd-server/sd-server}"
    echo "[entrypoint]          воркер поднимется (load/generate вернут понятную ошибку)."
fi

# ---- Локальные модели: только предупреждаем, если каталог пуст ----
MODELS_DIR="${SDWORKER_IMAGE_MODELS_DIR:-/app/models/image}"
if [ -d "${MODELS_DIR}" ]; then
    COUNT=$(find "${MODELS_DIR}" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
    echo "[entrypoint] image bundles found: ${COUNT}"
    if [ "${COUNT}" = "0" ]; then
        echo "[entrypoint] NOTE: каталог моделей пуст — генерация вернёт model_not_found."
        echo "[entrypoint]       Bundle кладётся как <models_dir>/<имя>/{файлы,profile.json}."
    fi
else
    mkdir -p "${MODELS_DIR}"
    echo "[entrypoint] created models dir ${MODELS_DIR}"
fi

# ---- Унификация имён переменных балансера ----
# Go-сторона читает SDWORKER_BALANCER_URL/TOKEN, compose-файлы исторически
# используют BALANCER_URL/BALANCER_API_TOKEN. Принимаем оба имени.
export SDWORKER_BALANCER_URL="${SDWORKER_BALANCER_URL:-${BALANCER_URL:-}}"
export SDWORKER_BALANCER_TOKEN="${SDWORKER_BALANCER_TOKEN:-${BALANCER_API_TOKEN:-}}"

# ---- Права на каталоги volume (docker создаёт их root'ом) ----
for d in "${MODELS_DIR}" "${SDWORKER_IMAGES_DIR:-/app/data/images}" \
         "${SDWORKER_LORA_DIR:-/app/lora}" "${SDWORKER_HIRES_UPSCALERS_DIR:-/app/upscalers}"; do
    [ -d "$d" ] || mkdir -p "$d"
    # chown может не сработать (read-only volume) — это не повод падать.
    chown -R appuser:appgroup "$d" 2>/dev/null || true
done

echo "[entrypoint] launching: /app/sdworker $*"
# Привилегии сбрасываем через setpriv (есть в базовом ubuntu:24.04 —
# util-linux), а не через su-exec/gosu: лишняя зависимость в образе не нужна.
# Выполняем именно exec, чтобы sdworker стал PID 1 и получал SIGTERM от docker
# stop (иначе субпроцесс sd-server останется сиротой с занятой VRAM).
exec setpriv --reuid=1000 --regid=1000 --init-groups /app/sdworker "$@"
