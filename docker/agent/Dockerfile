# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Установка зависимостей
RUN apk add --no-cache git

# Копирование go.mod и go.sum
COPY go.mod go.sum ./
RUN go mod download

# Копирование исходного кода
COPY . .

# Build argument для включения NVML
ARG ENABLE_NVML=false

# Сборка агента
# Если ENABLE_NVML=true, то используется CGO и build tag "nvml"
# Если ENABLE_NVML=false, то используется CGO_ENABLED=0 без NVML
RUN if [ "$ENABLE_NVML" = "true" ]; then \
        GOOS=linux go build -tags nvml -o agent ./cmd/agent; \
    else \
        CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o agent ./cmd/agent; \
    fi

# Runtime stage
FROM alpine:3.19

WORKDIR /app

# Установка зависимостей
# nvidia-smi требуется для сбора GPU метрик
# Для работы с GPU также нужны драйверы NVIDIA (предоставляются хостом)
RUN apk add --no-cache tzdata ca-certificates curl wget

# Копирование бинарника из builder
COPY --from=builder /build/agent .

# Создание пользователя без root прав
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER appuser

# Экспозиция порта для локальных метрик
EXPOSE 9090

# Метки для образа
LABEL org.opencontainers.image.title="Ollama Load Balancer Agent"
LABEL org.opencontainers.image.description="Agent for collecting GPU metrics from remote servers"
LABEL org.opencontainers.image.vendor="Ollama Load Balancer"
LABEL org.opencontainers.image.version="1.0.0"

# Health check для проверки состояния Ollama
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:11434/api/tags || exit 1

# Запуск агента
ENTRYPOINT ["./agent"]
CMD []
