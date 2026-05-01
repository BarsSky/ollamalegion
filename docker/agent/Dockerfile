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
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 2: test — запуск тестов
# -----------------------------------------------------------------------------
FROM deps AS test

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go test -coverprofile=coverage.out -covermode=atomic ./... || \
    (echo "=== TEST FAILURES ===" && cat coverage.out 2>/dev/null; exit 1)

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
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-w -s -extldflags '-static'" \
    -a -installsuffix cgo -o agent ./cmd/agent

# -----------------------------------------------------------------------------
# Stage 3b: builder-gpu — сборка с NVML (требует glibc, не alpine)
# Использует локальный nvidia/cuda образ с установленным golang
# -----------------------------------------------------------------------------
FROM nvidia/cuda:12.2.0-base-ubuntu22.04 AS builder-gpu

# Устанавливаем Go 1.22 напрямую (обход Docker Hub)
# Используем go1.22.10 — stable, не требует Docker Hub
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

# Сборка с NVML под glibc (nvidia/cuda base)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-w -s" \
    -tags nvml -o agent ./cmd/agent

# -----------------------------------------------------------------------------
# Stage 4: agent-cpu — distroless (статический бинарник, без shell)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS agent-cpu

WORKDIR /app
COPY --from=builder /build/agent .

EXPOSE 18032

LABEL org.opencontainers.image.title="Ollama Load Balancer Agent (CPU)"
LABEL org.opencontainers.image.description="Agent for collecting CPU metrics from Ollama servers"
LABEL org.opencontainers.image.vendor="OllamaLegion"
LABEL org.opencontainers.image.version="1.2.0"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"

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
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && \
    apt-get install -y --no-install-recommends tzdata ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

RUN groupadd -g 65532 appgroup && \
    useradd -u 65532 -g appgroup -m -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER 65532

EXPOSE 18032

LABEL org.opencontainers.image.title="Ollama Load Balancer Agent (GPU)"
LABEL org.opencontainers.image.description="Agent for collecting GPU/CPU metrics from Ollama servers with NVML"
LABEL org.opencontainers.image.vendor="OllamaLegion"
LABEL org.opencontainers.image.version="1.2.0"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"

HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/app/agent", "-healthcheck"]

ENTRYPOINT ["/app/agent"]
CMD []