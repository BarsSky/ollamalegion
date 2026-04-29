# =============================================================================
# Ollama Agent — Dockerfile
# =============================================================================
# Multi-stage build:
#   1. deps       — загрузка Go модулей (кэшируется отдельно)
#   2. test       — запуск тестов с coverage
#   3. builder    — сборка production бинарника
#   4. production — минимальный runtime образ
# =============================================================================
# Build args:
#   BASE_IMAGE    — базовый образ runtime (по умолчанию ubuntu:22.04)
#   ENABLE_NVML   — включить NVML/GPU поддержку (true/false)
# =============================================================================

ARG BASE_IMAGE=ubuntu:22.04

# -----------------------------------------------------------------------------
# Stage 1: deps — загрузка зависимостей
# -----------------------------------------------------------------------------
FROM golang:1.21 AS deps

WORKDIR /build

# Минимальные зависимости для CGO/NVML
RUN apt-get update && apt-get install -y --no-install-recommends git gcc libc6-dev && \
    rm -rf /var/lib/apt/lists/*

# Копируем только go.mod/go.sum для кэширования слоя зависимостей
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 2: test — запуск тестов
# -----------------------------------------------------------------------------
FROM deps AS test

# Копируем исходный код
COPY . .

# Запускаем тесты (NVML stub всегда компилируется, race detector отключен для CGO)
RUN CGO_ENABLED=1 go test -coverprofile=coverage.out -covermode=atomic ./... || \
    (echo "=== TEST FAILURES ===" && cat coverage.out 2>/dev/null; exit 1)

# -----------------------------------------------------------------------------
# Stage 3: builder — компиляция production бинарника
# -----------------------------------------------------------------------------
FROM deps AS builder

# Копируем исходный код (повторно используем кэш deps)
COPY . .

ARG ENABLE_NVML=true

# Сборка агента с условным включением NVML
# -trimpath — воспроизводимая сборка
# -ldflags "-w -s" — минимизация размера
RUN if [ "$ENABLE_NVML" = "true" ]; then \
        CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags "-w -s" \
        -tags nvml -o agent ./cmd/agent; \
    else \
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags "-w -s -extldflags '-static'" \
        -a -installsuffix cgo -o agent ./cmd/agent; \
    fi

# -----------------------------------------------------------------------------
# Stage 4: production — минимальный runtime
# -----------------------------------------------------------------------------
FROM ${BASE_IMAGE} AS production

WORKDIR /app

# Минимальные зависимости runtime
RUN apt-get update && \
    apt-get install -y --no-install-recommends tzdata ca-certificates curl wget && \
    rm -rf /var/lib/apt/lists/*

# Копирование бинарника из builder
COPY --from=builder /build/agent .

# Создание непривилегированного пользователя
RUN groupadd -g 1000 appgroup && \
    useradd -u 1000 -g appgroup -m -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER appuser

# Экспозиция порта агента
EXPOSE 18032

# Метки образа
LABEL org.opencontainers.image.title="Ollama Load Balancer Agent"
LABEL org.opencontainers.image.description="Agent for collecting GPU/CPU metrics from Ollama servers"
LABEL org.opencontainers.image.vendor="OllamaLegion"
LABEL org.opencontainers.image.version="1.1.0"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"

# Health check — проверка /health endpoint агента
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:18032/health || exit 1

# Запуск агента
ENTRYPOINT ["./agent"]
CMD []