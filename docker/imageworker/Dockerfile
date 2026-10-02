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
# официальный Linux-Vulkan-ассет (~37 MB) — он работает на AMD/Intel/NVIDIA
# через драйвер Vulkan, то есть один образ покрывает почти всё железо.
#
# ⚠️ CUDA: LINUX-CUDA-РЕЛИЗА У stable-diffusion.cpp НЕТ.
# В релизе master-929 присутствуют: Linux-Ubuntu-24.04-x86_64 (CPU),
# -vulkan, -rocm-7.14.0, а CUDA-сборки есть только под Windows.
# Поэтому CUDA-вариант возможен ТОЛЬКО сборкой из исходников
# (`cmake -B build -DSD_CUDA=ON`, нужен CUDA Toolkit) — либо подсунуть готовый
# бинарь через build-arg SD_SERVER_BIN=/path/inside/build/context:
#
#   docker build -f docker/imageworker/Dockerfile \
#     --build-arg SD_SERVER_URL= \
#     --build-arg SD_SERVER_BIN=./vendor/sd-server-cuda \
#     -t ollama-legion/imageworker:cuda .
#
# ЮБИЛЕЙНЫЙ ВАЖНЫЙ МОМЕНТ ПРО ВЕРСИЮ: имена флагов sd-server менялись
# (--host/--port → --listen-ip/--listen-port после master-600). Контракт
# sdworker написан под types.PinnedSDServerRevision; пин ниже фиксирует ту же
# ревизию, чтобы сборка и контракт не разъезжались молча.
# =============================================================================

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
ARG SD_SERVER_ASSET=sd-master-3f8527a-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip
ARG SD_SERVER_URL=https://github.com/leejet/stable-diffusion.cpp/releases/download/${SD_SERVER_REVISION}/${SD_SERVER_ASSET}
# SD_SERVER_BIN — «подсунуть свой бинарь» (сборка из исходников, CUDA-вариант).
# Путь трактуется относительно build context (например ./vendor/sd-server).
ARG SD_SERVER_BIN=

RUN apk add --no-cache curl unzip && mkdir -p /sdserver
RUN set -eux; \
    if [ -n "${SD_SERVER_BIN}" ]; then \
        # Свой бинарь передаётся как часть build context (путь внутри него)
        # и монтируется в отдельный stage ниже; здесь только создаём каталог,
        # чтобы COPY --from не падал при пустом build-arg.
        echo ">>> provided binary mode: ${SD_SERVER_BIN}"; \
        mkdir -p /out; \
    else \
        echo ">>> downloading ${SD_SERVER_URL}"; \
        curl -fsSL -o /tmp/sd.zip "${SD_SERVER_URL}"; \
        unzip -q -o /tmp/sd.zip -d /sdserver; \
        rm -f /tmp/sd.zip; \
        found="$(find /sdserver -type f -name 'sd-server' | head -n1)"; \
        if [ -z "${found}" ]; then \
            echo "ERROR: sd-server не найден в релизном архиве ${SD_SERVER_URL}" >&2; \
            echo "       (для CUDA под Linux готового релиза НЕТ — задайте --build-arg SD_SERVER_BIN=./vendor/sd-server-cuda)" >&2; \
            exit 1; \
        fi; \
        dir="$(dirname "${found}")"; \
        mkdir -p /out; \
        # Переносим каталог целиком: рядом с sd-server лежат его .so.
        cp -a "${dir}"/. /out/; \
        chmod +x /out/sd-server; \
    fi; \
    ls -la /out

# -----------------------------------------------------------------------------
# Stage 3: runtime
# -----------------------------------------------------------------------------
FROM ubuntu:24.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Зависимости рантайма:
#   libvulkan1 + mesa-vulkan-drivers — Vulkan-сборка (основной путь);
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
RUN chmod +x /entrypoint.sh /app/sd-server/sd-server && \
    mkdir -p /app/models/image /app/data/images /app/lora /app/upscalers && \
    addgroup -gid 1000 appgroup 2>/dev/null || groupadd -g 1000 appgroup && \
    useradd -u 1000 -g appgroup -M -s /usr/sbin/nologin appuser && \
    chown -R appuser:appgroup /app

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

# HEALTHCHECK через сам бинарник: curl может отсутствовать в минимальном
# образе, а `sdworker -healthcheck` есть всегда.
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
    CMD /app/sdworker -healthcheck || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["--port", "18093"]

LABEL org.opencontainers.image.title="Ollama Legion Image Worker (sd.cpp)"
LABEL org.opencontainers.image.description="Image generation worker: supervises sd-server (stable-diffusion.cpp) and exposes OpenAI/A1111/native HTTP API"
LABEL org.opencontainers.image.vendor="OllamaLegion"
LABEL org.opencontainers.image.version="0.1.0"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"
