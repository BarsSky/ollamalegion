# =============================================================================
# ImageWorker (sdworker) — Go-воркер генерации изображений поверх sd-server
# (stable-diffusion.cpp). R-Image / Phase 3.
# =============================================================================
#
# ЧТО СОБИРАЕТСЯ:
#   - Go-бинарник sdworker (без cgo: весь движок живёт в отдельном процессе
#     sd-server, поэтому CGO_ENABLED=0 и никаких ggml/CUDA-линковок в образе);
#   - бинарь sd-server из ГОТОВОГО релиза stable-diffusion.cpp.
#
# ПОЧЕМУ ГОТОВЫЙ РЕЛИЗ, А НЕ СБОРКА ИЗ ИСХОДНИКОВ:
# релизы sd.cpp выходят по нескольку раз в день, а сборка из исходников — это
# полный ggml-подмодуль и десятки минут CI. Для основного пути берём
# официальный Linux-Vulkan-ассет (~35 MB) — он работает на AMD/Intel/NVIDIA
# через драйвер Vulkan, то есть один образ покрывает почти всё железо.
#
# ┌─ VULKAN И NVIDIA: ЧТО ИМЕННО НУЖНО ────────────────────────────────────────┐
# │ mesa-vulkan-drivers (в образе) даёт ICD для AMD/Intel (RADV/ANV) и          │
# │ software-lavapipe, но НЕ для NVIDIA: проприетарный Vulkan-драйвер NVIDIA    │
# │ (nvidia_icd.json + libnvidia-glcore) попадает в контейнер ТОЛЬКО через     │
# │ NVIDIA Container Toolkit, то есть `runtime: nvidia` + capability           │
# │ `graphics` (compute,utility дают лишь CUDA/NVML). Проброс /dev/dri для      │
# │ NVIDIA бесполезен, он нужен AMD/Intel — см. devices: в                    │
# │ deployments/docker-compose.imageworker.yml.                                 │
# └────────────────────────────────────────────────────────────────────────────┘
#
# ⚠️ CUDA: LINUX-CUDA-РЕЛИЗА У stable-diffusion.cpp НЕТ.
# В релизе master-929 присутствуют: Linux-Ubuntu-24.04-x86_64 (CPU),
# -vulkan, -rocm-7.14.0, а CUDA-сборки есть только под Windows.
# Поэтому CUDA-вариант возможен ТОЛЬКО сборкой из исходников
# (`cmake -B build -DSD_CUDA=ON`, нужен CUDA Toolkit) — либо подсунуть готовый
# бинарь через build-arg SD_SERVER_BIN (ИМЯ ФАЙЛА внутри
# docker/imageworker/vendor/; каталог существует в репозитории, поэтому COPY
# не падает, когда бинарь не задан):
#
#   cp build/bin/sd-server docker/imageworker/vendor/sd-server-cuda
#   docker build -f docker/imageworker/Dockerfile \
#     --build-arg SD_SERVER_BIN=sd-server-cuda \
#     --build-arg RUNTIME_BASE=nvidia/cuda:12.6.0-runtime-ubuntu24.04 \
#     -t ollama-legion/imageworker:cuda .
#
#   ⚠️ Два ограничения контекста сборки:
#     1) .dockerignore в корне репо исключает *.so — если вашему бинарю НУЖНЫ
#        .so рядом с ним, в образ они не попадут (соберите статически либо
#        доработайте .dockerignore, он вне зоны этого Dockerfile);
#     2) CUDA-сборка sd.cpp линкуется с libcudart/libcublas: nvidia-runtime
#        монтирует только ДРАЙВЕР, а эти библиотеки даёт базовый образ, поэтому
#        для CUDA нужен RUNTIME_BASE=nvidia/cuda:<ver>-runtime-ubuntu24.04.
#
# ЮБИЛЕЙНЫЙ ВАЖНЫЙ МОМЕНТ ПРО ВЕРСИЮ: имена флагов sd-server менялись
# (--host/--port → --listen-ip/--listen-port после master-600). Контракт
# sdworker написан под types.PinnedSDServerRevision; пин ниже фиксирует ту же
# ревизию, чтобы сборка и контракт не разъезжались молча.
# =============================================================================

# Глобальный build-arg: только объявленный ДО первого FROM ARG можно
# подставлять в строку FROM — это требование Dockerfile.
# RUNTIME_BASE — база стадии runtime. Дефолт ubuntu:24.04 (Vulkan/CPU). Для CUDA
# подставляйте образ с CUDA-рантаймом (libcudart/libcublas), например
# nvidia/cuda:12.6.0-runtime-ubuntu24.04: nvidia-container-runtime монтирует
# только драйвер, и без этих библиотек бинарь не запустится.
ARG RUNTIME_BASE=ubuntu:24.04

FROM golang:1.24-alpine AS builder

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} GOFLAGS=-mod=mod

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: sdworker общается с sd-server по HTTP, C-линковок нет —
# образ остаётся маленьким, а сборка не зависит от тулчейна/ABI движка.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -tags llama_stub -ldflags "-w -s" \
    -o /out/sdworker ./cmd/sdworker

# -----------------------------------------------------------------------------
# Stage 2: fetch-sd-server — забрать официальный релизный sd-server
# -----------------------------------------------------------------------------
FROM alpine:3.20 AS fetch-sd-server

ARG SD_SERVER_REVISION=master-929-3f8527a
# Дефолтный ассет — ровно то имя, что лежит в релизе master-929-3f8527a
# (Linux, Ubuntu 24.04, x86_64, Vulkan). CPU-вариант того же релиза:
#   sd-master-3f8527a-bin-Linux-Ubuntu-24.04-x86_64.zip
ARG SD_SERVER_ASSET=sd-master-3f8527a-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip
ARG SD_SERVER_URL=https://github.com/leejet/stable-diffusion.cpp/releases/download/${SD_SERVER_REVISION}/${SD_SERVER_ASSET}
# SD_SERVER_BIN — «подсунуть свой бинарь» (CUDA / своя сборка из исходников).
# Значение — имя файла внутри docker/imageworker/vendor/ (context = корень репо).
ARG SD_SERVER_BIN=

# vendor/ существует в контексте ВСЕГДА (в нём лежит .gitkeep), поэтому этот
# COPY не падает при пустом SD_SERVER_BIN — в отличие от `COPY ${SD_SERVER_BIN}`.
COPY docker/imageworker/vendor/ /provided/

RUN apk add --no-cache curl unzip && mkdir -p /sdserver /out
# В обоих режимах каталог движка переносится ЦЕЛИКОМ (`cp -a <dir>/. /out/`):
# рядом с sd-server лежат его .so, поодиночке бинарь не запустится.
# Комментарии намеренно вне RUN: строки с `#` внутри продолжения инструкции
# парсер Dockerfile понимает не во всех версиях.
RUN set -eux; \
    if [ -n "${SD_SERVER_BIN}" ]; then \
        name="${SD_SERVER_BIN#./}"; \
        src="/provided/${name}"; \
        if [ ! -f "${src}" ]; then \
            echo "ERROR: SD_SERVER_BIN='${SD_SERVER_BIN}' не найден (искал ${src})" >&2; \
            echo "       положите бинарь в docker/imageworker/vendor/ и передайте имя файла:" >&2; \
            echo "       docker build --build-arg SD_SERVER_BIN=sd-server-cuda ..." >&2; \
            exit 1; \
        fi; \
        echo ">>> provided binary mode: ${src}"; \
        cp -a /provided/. /out/; \
        if [ "${name}" != "sd-server" ]; then cp -a "${src}" /out/sd-server; fi; \
        chmod +x /out/sd-server; \
    else \
        echo ">>> downloading ${SD_SERVER_URL}"; \
        curl -fsSL -o /tmp/sd.zip "${SD_SERVER_URL}"; \
        unzip -q -o /tmp/sd.zip -d /sdserver; \
        rm -f /tmp/sd.zip; \
        found="$(find /sdserver -type f -name 'sd-server' | head -n1)"; \
        if [ -z "${found}" ]; then \
            echo "ERROR: sd-server не найден в релизном архиве ${SD_SERVER_URL}" >&2; \
            echo "       (для CUDA под Linux готового релиза НЕТ — задайте --build-arg SD_SERVER_BIN=sd-server-cuda)" >&2; \
            exit 1; \
        fi; \
        dir="$(dirname "${found}")"; \
        cp -a "${dir}"/. /out/; \
        chmod +x /out/sd-server; \
    fi; \
    ls -la /out

# -----------------------------------------------------------------------------
# Stage 3: runtime
# -----------------------------------------------------------------------------
# База этой стадии задаётся глобальным ARG RUNTIME_BASE (см. начало файла):
# ubuntu:24.04 по умолчанию, nvidia/cuda:<ver>-runtime-ubuntu24.04 для CUDA.
FROM ${RUNTIME_BASE} AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Зависимости рантайма:
#   libvulkan1 — Vulkan LOADER, обязателен (sd.cpp-Vulkan линкуется с ним);
#   mesa-vulkan-drivers — ICD-драйверы AMD/Intel (RADV/ANV) + software-lavapipe
#     (аварийный путь «Vulkan без GPU»); ICD NVIDIA приезжает из nvidia-runtime;
#   libgomp1 — OpenMP (движок использует его для CPU-части);
#   libstdc++6 — C++ рантайм (в базовом образе есть, фиксируем явно);
#   ca-certificates/tzdata — TLS и корректное время в логах;
#   curl — healthcheck и отладка из контейнера, python3 — HF-скрипты оператора.
RUN apt-get update && apt-get install -y --no-install-recommends \
        libvulkan1 \
        mesa-vulkan-drivers \
        libgomp1 \
        libstdc++6 \
        ca-certificates \
        tzdata \
        curl \
        python3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/sdworker /app/sdworker
COPY --from=fetch-sd-server /out/ /app/sd-server/
COPY docker/imageworker/entrypoint.sh /entrypoint.sh

# UID/GID воркера — 1000; entrypoint сбрасывает привилегии через setpriv по
# ЧИСЛАМ, поэтому имена пользователя/группы не обязательны. В ubuntu:24.04
# UID/GID 1000 уже занят пользователем `ubuntu`, поэтому создаём «только если
# свободно»: раньше здесь стоял alpine-синтаксис `addgroup -gid` + безусловный
# groupadd — на ubuntu это падало и ломало сборку целиком.
RUN chmod +x /entrypoint.sh /app/sd-server/sd-server && \
    mkdir -p /app/models/image /app/data/images /app/lora /app/upscalers && \
    (getent group 1000 >/dev/null || groupadd -g 1000 appgroup) && \
    (getent passwd 1000 >/dev/null || useradd -u 1000 -g 1000 -M -s /usr/sbin/nologin appuser) && \
    chown -R 1000:1000 /app

# Пути по умолчанию: sd-server берём из образа, модели — из volume.
ENV SDWORKER_PORT=18093 \
    SDWORKER_SD_SERVER_BIN=/app/sd-server/sd-server \
    SDWORKER_IMAGE_MODELS_DIR=/app/models/image \
    SDWORKER_IMAGES_DIR=/app/data/images \
    SDWORKER_SD_SERVER_PORT=18094 \
    SDWORKER_LORA_DIR=/app/lora \
    SDWORKER_HIRES_UPSCALERS_DIR=/app/upscalers \
    HF_HOME=/app/.cache/huggingface \
    HF_TOKEN="" \
    HF_MIRROR=""

# Движок слушает ТОЛЬКО loopback (--listen-ip 127.0.0.1): наружу торчит API
# воркера, иначе клиенты обойдут нормализацию seed/размеров.
EXPOSE 18093

# HEALTHCHECK через сам бинарник (как у cppworker): `sdworker -healthcheck`
# есть всегда, а curl в минимальном образе может отсутствовать.
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
    CMD ["/app/sdworker", "-healthcheck"]

ENTRYPOINT ["/entrypoint.sh"]
CMD ["--port", "18093"]

LABEL org.opencontainers.image.title="Ollama Legion Image Worker (sd.cpp)"
LABEL org.opencontainers.image.description="Image generation worker: supervises sd-server (stable-diffusion.cpp) and exposes OpenAI/A1111/native HTTP API"
LABEL org.opencontainers.image.vendor="OllamaLegion"
LABEL org.opencontainers.image.version="0.1.0"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"
