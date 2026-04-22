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

# Сборка агента
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o agent ./cmd/agent

# Runtime stage
FROM alpine:3.19

WORKDIR /app

# Установка зависимостей
# nvidia-smi требуется для сбора GPU метрик
# Для работы с GPU также нужны драйверы NVIDIA (предоставляются хостом)
RUN apk add --no-cache tzdata ca-certificates curl

# Копирование бинарника из builder
COPY --from=builder /build/agent .

# Создание пользователя без root прав
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER appuser

# Экспозиция порта для локальных метрик
EXPOSE 9090

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:11434/api/tags || exit 1

# Запуск
ENTRYPOINT ["./agent"]
CMD []
