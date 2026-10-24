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
# Сборка (Round 31 #1 - v31r5):
# (было:  #7 v31r4-final 2):
# (было:  #7 - v31r4-final):
# (было:  #7 token usage - v31r4):
# (было:  FINAL v31r4-final):
# (было:  final2 - v31r4):
# (было:  final - v31r4g):
# (было:  fix config - v31r4f):
# (было:  debug2):
# (было:  - v31r4d):
# (было:  - v31r4 async preflight):
# (было:  #4 — v31r3 streaming strip):
#   docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: deps — загрузка зависимостей (golang:1.24-alpine, локальный)
# -----------------------------------------------------------------------------
# R128 (2026-10-16): база запиннена по патч-версии — ради воспроизводимости.
#
# ЧЕСТНО ПРО ПРИЧИНУ. Изначально я объяснил этим пином «нестабильный линкер»:
# сборка падала, печатая usage линкера, то есть выглядела как несовместимость
# тулчейна. Диагноз оказался НЕВЕРНЫМ: причина была в моём build-arg —
# `--build-arg "BUILD_DATE=(Get-Date -Format yyyy-MM-dd)"` PowerShell не
# вычисляет внутри кавычек, и литерал `(Get-Date -Format yyyy-MM-dd)` попадал в
# `-ldflags`, где пробелы разбивали его на лишние флаги (`flag provided but not
# defined: -Format`). Одинаковый симптом давал и отказ сборки cppworker ранее.
#
# Пин оставлен как самостоятельная польза (сборка не зависит от того, какой патч
# Go отдал реестр), но НЕ как «лечение» той ошибки — она лечится корректной
# передачей BUILD_DATE (переменная, а не выражение в кавычках).
FROM dockerhub.timeweb.cloud/library/golang:1.24.3-alpine AS deps

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
#
# R83 (2026-09-28): GIT_COMMIT/BUILD_DATE подставляются в pkg/version, чтобы
# стартовая шапка логов показывала, ЧТО именно запущено. Передаются как
# build-args (см. scripts/rebuild_balancer_r66a.ps1 / release-all.ps1).
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
# R127 (2026-10-16): -extldflags БЕЗ вложенных кавычек.
#
# ЖИВОЙ СЛУЧАЙ. Базовая стадия — плавающий тег `golang:1.24-alpine`; при
# пересборке он подтянул более новый патч тулчейна, и сборка упала: линкер Go
# печатал свою usage, то есть `-extldflags` доезжал до него как НЕИЗВЕСТНЫЙ
# ВЕРХНЕУРОВНЕВЫЙ флаг. Причина — вложенные одинарные кавычки внутри
# double-quoted `-ldflags`: старый тулчейн их съедал, новый отдаёт как есть.
# Форма `-extldflags=-static` равнозначна и не зависит от разбора кавычек.
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -tags llama_stub -trimpath \
    -ldflags "-w -s -extldflags=-static \
      -X ollama-loadbalancer/pkg/version.Commit=${GIT_COMMIT} \
      -X ollama-loadbalancer/pkg/version.BuildDate=${BUILD_DATE}" \
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

# Копирование webui статики (все страницы, css, js, img, webfonts, favicon).
#
# R134 (2026-10-16): ЖИВАЯ ЖАЛОБА ОПЕРАТОРА «в панели балансера нет вкладки
# агенты и не отображаются иконки». Причина была здесь: копировались только
# monitor.html и health.html, а index.html (дашборд со всей навигацией, включая
# «Агенты»), rpc-status.html, tp-pipeline.html, virtual-models.html и КАТАЛОГ
# webfonts — нет. Следствие: /webfonts/fa-solid-900.woff2 отдавал 404 и иконки
# Font Awesome не рисовались, /index.html отдавал 404, поэтому панель
# балансера выглядела «обрезанной» версией WebUI. Проверка этого класса —
# scripts/check_webui_assets.py (--balancer-dockerfile): она падает, если
# маска страниц или каталог шрифтов не скопированы.
#
# Маска `*.html` вместо перечисления файлов — та же доктрина, что в
# docker/webui/Dockerfile: новая страница попадает в образ сама, без правки
# Dockerfile (иначе /virtual-models.html однажды уже отдавал 404).
COPY --from=builder /build/webui/*.html ./webui/
COPY --from=builder /build/webui/css ./webui/css
COPY --from=builder /build/webui/js ./webui/js
COPY --from=builder /build/webui/img ./webui/img
COPY --from=builder /build/webui/webfonts ./webui/webfonts
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




















