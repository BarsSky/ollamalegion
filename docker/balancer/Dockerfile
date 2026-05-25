# =============================================================================
# Ollama Load Balancer — Dockerfile (оптимизированный, локальные образы)
# =============================================================================
# Использует локально доступные базовые образы для обхода блокировки Docker Hub.
#
# Multi-stage build:
#   1. deps       — загрузка Go модулей (кэшируется отдельно)
#   2. test       — запуск тестов с coverage и race detector
#   3. builder    — сборка production бинарника
#   4. production — минимальный alpine образ
# =============================================================================
# Сборка:
#   docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: deps — загрузка зависимостей (golang:1.24-alpine, локальный)
# -----------------------------------------------------------------------------
FROM golang:1.24-alpine AS deps

WORKDIR /build

# Установка git для приватных репозиториев и build-base (gcc/musl) для CGO/race detector
RUN apk add --no-cache git build-base

# Копируем только go.mod/go.sum для кэширования слоя зависимостей
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 2: builder — компиляция production бинарника
# -----------------------------------------------------------------------------
FROM deps AS builder

# Копируем исходный код (повторно используем кэш deps)
COPY . .

# Статически линкованный бинарник
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-w -s -extldflags '-static'" \
    -a -installsuffix cgo -o balancer ./cmd/balancer

# -----------------------------------------------------------------------------
# Stage 4: production — минимальный alpine (локальный alpine:latest)
# -----------------------------------------------------------------------------
FROM alpine:latest AS production

WORKDIR /app

# Минимальные зависимости: tzdata, ca-certificates, wget (для HEALTHCHECK)
RUN apk add --no-cache tzdata ca-certificates wget curl

# Копирование бинарника из builder
COPY --from=builder /build/balancer .

# Копирование конфигурации по умолчанию (readonly шаблон)
COPY --from=builder /build/config/config.example.json ./config.json

# Копирование entrypoint-скрипта
COPY docker/balancer/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Копирование webui статики (monitor.html, css, js, img, favicon)
COPY --from=builder /build/webui/monitor.html ./webui/monitor.html
COPY --from=builder /build/webui/css ./webui/css
COPY --from=builder /build/webui/js ./webui/js
COPY --from=builder /build/webui/img ./webui/img
COPY --from=builder /build/webui/favicon.ico ./webui/favicon.ico
COPY --from=builder /build/webui/favicon-16x16.png ./webui/favicon-16x16.png
COPY --from=builder /build/webui/favicon-32x32.png ./webui/favicon-32x32.png
COPY --from=builder /build/webui/logo.svg ./webui/logo.svg

# Создание непривилегированного пользователя
RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -D -s /bin/sh appuser && \
    mkdir -p /app/data && \
    chown -R appuser:appgroup /app

USER appuser

# Порты: 18080 (Ollama API proxy), 18081 (Management API + WebSocket)
EXPOSE 18080 18081

# Healthcheck через wget (alpine имеет shell + wget)
HEALTHCHECK --interval=15s --timeout=10s --start-period=30s --retries=5 \
    CMD curl -f -s http://localhost:18081/api/v1/health || exit 1

# Запуск через entrypoint (копирует config.json в data/ при первом старте)
ENTRYPOINT ["/entrypoint.sh"]
CMD []
