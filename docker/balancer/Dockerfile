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
FROM dockerhub.timeweb.cloud/library/golang:1.24-alpine AS deps

WORKDIR /build

# Установка git для приватных репозиториев и build-base (gcc/musl) для CGO/race detector
# Разбито на отдельные RUN для устойчивости к частичным I/O ошибкам overlay FS (Docker Desktop / WSL2)
RUN apk add --no-cache git
RUN apk add --no-cache build-base && rm -rf /var/cache/apk/*

# Копируем только go.mod/go.sum для кэширования слоя зависимостей
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -----------------------------------------------------------------------------
# Stage 2: builder — компиляция production бинарника
# -----------------------------------------------------------------------------
FROM deps AS builder

# Копируем исходный код (повторно используем кэш deps)
COPY . .

# Статически линкованный бинарник.
# Собираем с -tags llama_stub, потому что балансер — это чистый HTTP-прокси,
# ему не нужна реальная llama.cpp. Пакет c/bridge содержит build-tag stub
# (c/bridge/bridge_stub.go), который активирует cgo с CFLAGS=-DGO_BRIDGE_LLAMA_STUB.
# Это вырезает всё тело c/bridge/bridge.c через #ifndef, и cgo компилирует
# пустой translation unit без зависимости от libllama.a.
#
# ВАЖНО: НЕ отключаем CGO_ENABLED=0, иначе cgo-директивы в bridge_stub.go
# не парсятся и Go выдаёт "build constraints exclude all Go files in c/bridge".
# alpine содержит gcc/musl (build-base), так что cgo работает.
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -tags llama_stub -trimpath -ldflags "-w -s -extldflags '-static'" \
    -a -o balancer ./cmd/balancer

# -----------------------------------------------------------------------------
# Stage 4: production — минимальный alpine (локальный alpine:latest)
# -----------------------------------------------------------------------------
FROM dockerhub.timeweb.cloud/library/alpine:latest AS production

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

# Копирование webui статики (monitor.html, health.html, css, js, img, favicon)
COPY --from=builder /build/webui/monitor.html ./webui/monitor.html
COPY --from=builder /build/webui/health.html ./webui/health.html
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

# Liveness probe — /api/v1/ping всегда 200 OK, пока HTTP-сервер жив.
# НЕ используем /api/v1/health: он возвращает 503 в degraded (когда 0
# healthy бэкендов), что уводит контейнер в restart loop до того, как
# cppworker успел зарегистрироваться.
HEALTHCHECK --interval=15s --timeout=10s --start-period=30s --retries=5 \
    CMD curl -fsS http://localhost:18081/api/v1/ping || exit 1

# Запуск через entrypoint (копирует config.json в data/ при первом старте)
ENTRYPOINT ["/entrypoint.sh"]
CMD []
