# =============================================================================
# Ollama Agent — Dockerfile (оптимизированный, локальные образы)
# =============================================================================
# Использует локально доступные базовые образы для обхода блокировки Docker Hub.
#
# Multi-stage build:
#   1. deps       — загрузка Go модулей (кэшируется отдельно)
#   2. test       — запуск тестов с coverage
#   3. builder    — сборка production бинарника
#   4. agent-cpu  — минимальный distroless образ (~8 MB)
#   5. agent-gpu  — образ с NVIDIA CUDA (~350 MB)
# =============================================================================
# Build args:
#   ENABLE_NVML — включить NVML/GPU поддержку (true/false, по умолчанию false)
# =============================================================================
# Сборка CPU (по умолчанию):
#   docker build -t ollama-legion/agent:latest --target agent-cpu -f docker/agent/Dockerfile .
#
# Сборка GPU:
#   docker build --build-arg ENABLE_NVML=true \
#     -t ollama-legion/agent:latest-gpu --target agent-gpu -f docker/agent/Dockerfile .
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: deps — загрузка зависимостей (golang:1.24-alpine, локальный)
# -----------------------------------------------------------------------------
FROM golang:1.24-alpine AS deps

WORKDIR /build

# Alpine: gcc + libc-dev для CGO/NVML (только для test и gpu-builder)
RUN apk add --no-cache git gcc musl-dev

# Копируем только go.mod/go.sum для кэширования слоя зависимостей
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 3: builder — компиляция production бинарника
# -----------------------------------------------------------------------------
FROM deps AS builder

COPY . .

ARG ENABLE_NVML=false

# -trimpath — воспроизводимая сборка
# -ldflags "-w -s" — минимизация размера (strip debug info)
# CGO_ENABLED=0 для CPU (статический бинарник, alpine musl не нужен)
# CGO_ENABLED=1 для GPU — но на alpine musl NVML может не собраться,
#   поэтому GPU-вариант требует glibc-совместимого билдера.
#   Для GPU используйте отдельный builder (см. stage builder-gpu ниже).
#
# R486 (2026-11-10): идентичность ЗАПЕКАЕТСЯ в бинарник. Дефолтов `=unknown` у
# ARG нет: непереданный arg валит сборку стражей ниже (живой дефект: образ с
# коммитом 850923d7 при HEAD 8b246ffe «честно» сообщал чужой штамп).
ARG SERVICE=agent
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR

# R486: СТРАЖ перед go build: без штампа образ не собирается.
RUN test -n "$GIT_COMMIT" && test "$GIT_COMMIT" != "unknown" || \
        (echo "FATAL: GIT_COMMIT not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)
RUN test -n "$GIT_COMMIT_FULL" && test "$GIT_COMMIT_FULL" != "unknown" || \
        (echo "FATAL: GIT_COMMIT_FULL not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)
RUN test -n "$BUILD_DATE" && test "$BUILD_DATE" != "unknown" || \
        (echo "FATAL: BUILD_DATE not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -tags llama_stub -trimpath \
    -ldflags "-w -s -extldflags '-static' \
      -X ollama-loadbalancer/pkg/version.Service=${SERVICE} \
      -X ollama-loadbalancer/pkg/version.Version=${VERSION} \
      -X ollama-loadbalancer/pkg/version.ImageTag=${IMAGE_TAG} \
      -X ollama-loadbalancer/pkg/version.Commit=${GIT_COMMIT} \
      -X ollama-loadbalancer/pkg/version.CommitFull=${GIT_COMMIT_FULL} \
      -X ollama-loadbalancer/pkg/version.CommitDate=${GIT_COMMIT_DATE} \
      -X ollama-loadbalancer/pkg/version.BuildDate=${BUILD_DATE}" \
    -a -installsuffix cgo -o agent ./cmd/agent

# R486: /build/identity.json — манифест идентичности ДЛЯ ФИНАЛЬНОЙ СТАДИИ.
#
# ПОЧЕМУ ЗДЕСЬ, А НЕ В agent-cpu. Финальная стадия CPU — distroless
# (static-debian12:nonroot): в ней НЕТ shell, поэтому `RUN printf` там
# невозможен принципиально. Пишем файл в стадии со shell и переносим его COPY'ем
# (см. ниже); содержимое то же, что уходит в -ldflags.
RUN printf '{"service":"%s","version":"%s","image_tag":"%s","commit":"%s","commit_full":"%s","commit_date":"%s","build_date":"%s","author":"%s","llama_commit":"%s","llama_date":"%s","rpc_proto":"%s"}\n' "$SERVICE" "$VERSION" "$IMAGE_TAG" "$GIT_COMMIT" "$GIT_COMMIT_FULL" "$GIT_COMMIT_DATE" "$BUILD_DATE" "$BUILD_AUTHOR" "$LLAMA_BUILD" "$LLAMA_BUILD_DATE" "$RPC_PROTO" > /build/identity.json && chmod 0644 /build/identity.json

# -----------------------------------------------------------------------------
# Stage 3b: builder-gpu — сборка с NVML (требует glibc, не alpine)
# Использует локальный nvidia/cuda образ с установленным golang.
#
# 2026-06-30: добавляем -tags llama_stub чтобы не подтягивать c/bridge/bridge.c
# (требует llama.h из upstream llama.cpp, который не копируется на этой стадии).
# bridge_stub.go полностью покрывает все симптомы CGo, которые нужны агенту
# — agent не вызывает llama.cpp напрямую (он работает с CppWorker через HTTP).
# -----------------------------------------------------------------------------
FROM nvidia/cuda:12.2.0-base-ubuntu22.04 AS builder-gpu

# Устанавливаем Go напрямую (обход Docker Hub).
# Используем go1.22.10 — stable, не требует Docker Hub.
ARG GO_VERSION=1.22.10
RUN apt-get update && apt-get install -y --no-install-recommends \
    wget ca-certificates gcc libc6-dev && \
    wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz && \
    rm -rf /var/lib/apt/lists/*

ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
ENV GOMODCACHE="/go/pkg/mod"

WORKDIR /build

# Копируем go.mod/go.sum и скачиваем зависимости
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Копируем исходный код
COPY . .

ARG ENABLE_NVML=true

# Сборка с NVML под glibc (nvidia/cuda base).
# -tags "nvml llama_stub" включает NVML (Go-nvml) и stub-режим bridge —
# agent не вызывает bridge symbols на runtime, поэтому stub покрывает потребности.
#
# R486: те же ARG и тот же страж, что в CPU-стадии (build-arg виден только в той
# стадии, где объявлен), плюс полный набор -X для pkg/version.
ARG SERVICE=agent
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR

# R486: СТРАЖ перед go build: без штампа образ не собирается.
RUN test -n "$GIT_COMMIT" && test "$GIT_COMMIT" != "unknown" || \
        (echo "FATAL: GIT_COMMIT not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)
RUN test -n "$GIT_COMMIT_FULL" && test "$GIT_COMMIT_FULL" != "unknown" || \
        (echo "FATAL: GIT_COMMIT_FULL not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)
RUN test -n "$BUILD_DATE" && test "$BUILD_DATE" != "unknown" || \
        (echo "FATAL: BUILD_DATE not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags "-w -s -extldflags '-static' \
      -X ollama-loadbalancer/pkg/version.Service=${SERVICE} \
      -X ollama-loadbalancer/pkg/version.Version=${VERSION} \
      -X ollama-loadbalancer/pkg/version.ImageTag=${IMAGE_TAG} \
      -X ollama-loadbalancer/pkg/version.Commit=${GIT_COMMIT} \
      -X ollama-loadbalancer/pkg/version.CommitFull=${GIT_COMMIT_FULL} \
      -X ollama-loadbalancer/pkg/version.CommitDate=${GIT_COMMIT_DATE} \
      -X ollama-loadbalancer/pkg/version.BuildDate=${BUILD_DATE}" \
    -tags "nvml llama_stub" -o agent ./cmd/agent

# -----------------------------------------------------------------------------
# Stage 4: agent-cpu — distroless (статический бинарник, без shell)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS agent-cpu

WORKDIR /app
COPY --from=builder /build/agent .
# R486: идентичность переносим готовым файлом — в distroless НЕТ shell, `RUN
# printf` здесь невозможен. --chown=65532 = nonroot-пользователь образа
# (distroless:nonroot), поэтому владелец совпадает с тем, от кого работает агент.
COPY --chown=65532:65532 --from=builder /build/identity.json /app/identity.json

# R486: build-arg'и объявлены ПОВТОРНО — ARG виден только в той стадии, где
# объявлен. Здесь нужны для OCI-меток.
ARG SERVICE=agent
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR

EXPOSE 18032

LABEL org.opencontainers.image.title="Ollama Load Balancer Agent (CPU)"
LABEL org.opencontainers.image.description="Agent for collecting CPU metrics from Ollama servers"
LABEL org.opencontainers.image.vendor="OllamaLegion"
# R486: версия — из build-arg, а не литерал «1.2.0» (литерал не имел отношения к
# собранному коду). .revision — ПОЛНЫЙ коммит, короткий лежит в ru.*-метке.
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_COMMIT_FULL}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.authors="${BUILD_AUTHOR}"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"
LABEL ru.ollamalegion.git.commit="${GIT_COMMIT}"
LABEL ru.ollamalegion.image.tag="${IMAGE_TAG}"

HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/app/agent", "-healthcheck"]

ENTRYPOINT ["/app/agent"]
CMD []

# -----------------------------------------------------------------------------
# Stage 5: agent-gpu — NVIDIA CUDA base (локальный образ)
# -----------------------------------------------------------------------------
FROM nvidia/cuda:12.2.0-base-ubuntu22.04 AS agent-gpu

WORKDIR /app
COPY --from=builder-gpu /build/agent .

# Минимальные runtime-зависимости
RUN apt-get update && \
    apt-get install -y --no-install-recommends tzdata ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

# R486: build-arg'и объявлены ПОВТОРНО (ARG виден только в той стадии, где он
# объявлен) и СРАЗУ перед использованием: объявление выше сбрасывало бы кэш слоя
# apt на каждой смене коммита.
ARG SERVICE=agent
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR

# R486: /app/identity.json — ЗАПЕЧЁННЫЙ манифест сборки, читает pkg/version
# (порядок: -ldflags > identity.json > env) и сверяет с бинарём на старте
# (VerifyR486). Пишем ДО chown ниже — тогда владельцем сразу становится appuser.
RUN printf '{"service":"%s","version":"%s","image_tag":"%s","commit":"%s","commit_full":"%s","commit_date":"%s","build_date":"%s","author":"%s","llama_commit":"%s","llama_date":"%s","rpc_proto":"%s"}\n' "$SERVICE" "$VERSION" "$IMAGE_TAG" "$GIT_COMMIT" "$GIT_COMMIT_FULL" "$GIT_COMMIT_DATE" "$BUILD_DATE" "$BUILD_AUTHOR" "$LLAMA_BUILD" "$LLAMA_BUILD_DATE" "$RPC_PROTO" > /app/identity.json && chmod 0644 /app/identity.json

RUN groupadd -g 65532 appgroup && \
    useradd -u 65532 -g appgroup -m -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER 65532

EXPOSE 18032

LABEL org.opencontainers.image.title="Ollama Load Balancer Agent (GPU)"
LABEL org.opencontainers.image.description="Agent for collecting GPU/CPU metrics from Ollama servers with NVML"
LABEL org.opencontainers.image.vendor="OllamaLegion"
# R486: версия — из build-arg, а не литерал «1.2.0» (литерал не имел отношения к
# собранному коду). .revision — ПОЛНЫЙ коммит, короткий лежит в ru.*-метке.
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_COMMIT_FULL}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.authors="${BUILD_AUTHOR}"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"
LABEL ru.ollamalegion.git.commit="${GIT_COMMIT}"
LABEL ru.ollamalegion.image.tag="${IMAGE_TAG}"

HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/app/agent", "-healthcheck"]

ENTRYPOINT ["/app/agent"]
CMD []
