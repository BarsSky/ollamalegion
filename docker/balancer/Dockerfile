# =============================================================================
# Ollama Load Balancer — Dockerfile
# =============================================================================
# Multi-stage build:
#   1. deps       — загрузка Go модулей (кэшируется отдельно)
#   2. test       — запуск тестов с coverage и race detector
#   3. builder    — сборка production бинарника
#   4. production — минимальный runtime образ
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: deps — загрузка зависимостей
# -----------------------------------------------------------------------------
FROM golang:1.21-alpine AS deps

WORKDIR /build

# Установка git для приватных репозиториев
# build-base (gcc, libc) нужен для CGO/race detector в stage test
RUN apk add --no-cache git build-base

# Копируем только go.mod/go.sum для кэширования слоя зависимостей
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 2: test — запуск тестов с race detector
# -----------------------------------------------------------------------------
FROM deps AS test

# Копируем исходный код
COPY . .

# Запускаем тесты с race detector и coverage
# CGO_ENABLED=1 требуется для race detector
RUN CGO_ENABLED=1 go test -race -coverprofile=coverage.out -covermode=atomic ./... || \
    (echo "=== TEST FAILURES ===" && cat coverage.out 2>/dev/null; exit 1)

# Генерируем HTML отчёт о покрытии (опционально, сохраняется как артефакт)
RUN go tool cover -html=coverage.out -o coverage.html 2>/dev/null || true

# -----------------------------------------------------------------------------
# Stage 3: builder — компиляция production бинарника
# -----------------------------------------------------------------------------
FROM deps AS builder

# Копируем исходный код (повторно используем кэш deps)
COPY . .

# Сборка статически линкованного бинарника
# -ldflags "-w -s" — удаление debug info и symbol table для уменьшения размера
# -trimpath — удаление абсолютных путей для воспроизводимой сборки
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-w -s -extldflags '-static'" \
    -a -installsuffix cgo -o balancer ./cmd/balancer

# -----------------------------------------------------------------------------
# Stage 4: production — минимальный runtime
# -----------------------------------------------------------------------------
FROM alpine:3.19 AS production

WORKDIR /app

# Установка минимальных зависимостей: tzdata (часовые пояса), ca-certificates (TLS),
# wget (для HEALTHCHECK). Используем --no-cache для минимизации слоя.
RUN apk add --no-cache tzdata ca-certificates wget

# Копирование бинарника из builder
COPY --from=builder /build/balancer .

# Копирование конфигурации по умолчанию
COPY --from=builder /build/config/config.example.json ./config.json

# Копирование monitor.html (для cmd/monitor)
COPY --from=builder /build/cmd/monitor/monitor.html ./monitor.html

# Создание директории для state и непривилегированного пользователя
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D -s /bin/sh appuser && \
    mkdir -p /app/data && \
    chown -R appuser:appgroup /app

USER appuser

# Порты:
#   18080 — Ollama API proxy
#   18081 — Management API + WebSocket
EXPOSE 18080 18081

# Health check — глубокая проверка через Management API.
# Использует wget с проверкой HTTP статуса (200 = healthy, 503 = degraded/unhealthy).
# --start-period=30s: даём балансеру время на полную инициализацию.
# --retries=5: больше попыток для стабильности при временных сбоях.
# --interval=15s: чаще, чтобы быстрее обнаруживать проблемы.
HEALTHCHECK --interval=15s --timeout=10s --start-period=30s --retries=5 \
    CMD wget --no-verbose --tries=1 --server-response --spider http://localhost:18081/api/v1/health 2>&1 | grep -q "200 OK" || exit 1

# Запуск
ENTRYPOINT ["./balancer"]
CMD ["-config", "./config.json"]