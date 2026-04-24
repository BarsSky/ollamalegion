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

# Сборка балансировщика
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o balancer ./cmd/balancer

# Runtime stage
FROM alpine:3.19

WORKDIR /app

# Установка tzdata для часовых поясов
RUN apk add --no-cache tzdata ca-certificates

# Копирование бинарника из builder
COPY --from=builder /build/balancer .
COPY --from=builder /build/config/config.example.json ./config.json

# Создание пользователя без root прав
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D -s /bin/sh appuser && \
    chown -R appuser:appgroup /app

USER appuser

# Экспозиция портов
# 18080 - прокси для Ollama API
# 18081 - Management API
EXPOSE 18080 18081

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:18081/api/v1/health || exit 1

# Запуск
ENTRYPOINT ["./balancer"]
CMD ["-config", "./config.json"]
