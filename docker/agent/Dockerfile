# Глобальный build arg для базового образа runtime stage
# Доступен для FROM ниже, т.к. объявлен ДО первого FROM
ARG BASE_IMAGE=ubuntu:22.04

# Build stage
# Используем golang:1.21 (Debian-based) для совместимости с NVML/glibc
FROM golang:1.21 AS builder

WORKDIR /build

# Установка зависимостей
# Для NVML требуется gcc и зависимости для CGO
RUN apt-get update && apt-get install -y --no-install-recommends git gcc libc6-dev

# Копирование go.mod и go.sum
COPY go.mod go.sum ./
RUN go mod download

# Копирование исходного кода
COPY . .

# Build argument для включения NVML
ARG ENABLE_NVML=true

# Сборка агента
# Если ENABLE_NVML=true, то используется CGO и build tag "nvml"
# Если ENABLE_NVML=false, то используется CGO_ENABLED=0 без NVML
RUN if [ "$ENABLE_NVML" = "true" ]; then \
        CGO_ENABLED=1 GOOS=linux go build -tags nvml -o agent ./cmd/agent; \
    else \
        CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o agent ./cmd/agent; \
    fi

# Runtime stage
# Базовый образ берется из глобального ARG BASE_IMAGE, объявленного выше.
# По умолчанию ubuntu:22.04 (CPU-режим).
# Для GPU: docker build --build-arg BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04
FROM ${BASE_IMAGE}

# Объявляем ARG повторно внутри stage, т.к. каждый FROM сбрасывает scope build args.
# Значение уже задано глобально, здесь достаточно имени без значения.
ARG BASE_IMAGE

WORKDIR /app

# Установка минимальных зависимостей
RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates curl wget && rm -rf /var/lib/apt/lists/*

# Копирование бинарника из builder
COPY --from=builder /build/agent .

# Создание пользователя без root прав
RUN groupadd -g 1000 appgroup && \
    useradd -u 1000 -g appgroup -m -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER appuser

# Экспозиция порта для локальных метрик
EXPOSE 18032

# Метки для образа
LABEL org.opencontainers.image.title="Ollama Load Balancer Agent"
LABEL org.opencontainers.image.description="Agent for collecting GPU metrics from remote servers"
LABEL org.opencontainers.image.vendor="Ollama Load Balancer"
LABEL org.opencontainers.image.version="1.0.0"

# Health check для проверки состояния Ollama
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:18032/metrics || exit 1

# Запуск агента
ENTRYPOINT ["./agent"]
CMD []
