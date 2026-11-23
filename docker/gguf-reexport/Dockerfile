# =============================================================================
# gguf-reexport — сервис ре-экспорта GGUF (Go-хост + python-конвертер)
# =============================================================================
#
# ПОЧЕМУ ОТДЕЛЬНЫЙ ОБРАЗ, А НЕ СЛОЙ В CPPWORKER. Ре-экспорт — это Python-
# экосистема llama.cpp (сначала стандартная библиотека, для полного пути ещё
# numpy/torch/transformers) и работа на десятки гигабайт. Образ воркера инференса
# обязан оставаться предсказуемым: его размер не должен зависеть от того, нужен ли
# стенду ре-экспорт, а обновление конвертера не должно требовать пересборки
# llama.cpp. Отдельный образ даёт ровно это: воркер не меняется вообще, модуль
# появляется профилем `reexport` (deployments/docker-compose.stack.yml).
#
# ДВА ТАРГЕТА — ОДИН DOCKERFILE:
#   light (default) — python3 + стандартная библиотека + скрипт патча loop-
#     метаданных. Умеет `inkling2_loop`: переупаковка метаданных и раскладка уже
#     лежащих в файле матриц запросов. torch НЕ нужен: веса не пересчитываются.
#     Размер ~200 МБ, сборка без сети (кроме кэша Go-модулей).
#   full — тот же образ + convert_hf_to_gguf.py из сабмодуля + torch/numpy/
#     transformers. Умеет полный ре-экспорт из HF-чекпойнта. Собирается
#     `--build-arg WITH_TORCH=1 --target full`; по умолчанию НЕ собирается, потому
#     что torch — это +2.5 ГБ и минуты, которые нужны не всем.
#
# ПОЧЕМУ TORCH НЕ УБРАТЬ СОВСЕМ. Можно (и в light так и сделано) обойтись без него
# для метаданных и переупаковки — это проверено живой задачей на 16.6 ГиБ файле.
# Но полный ре-экспорт «из чего угодно в GGUF» — это convert_hf_to_gguf.py, а он
# импортирует torch в первой строке (c/llama.cpp/convert_hf_to_gguf.py:11) и без
# него не работает ни на одной архитектуре. Свой «лёгкий» конвертер весов написать
# нельзя: там per-arch логика на десятки тысяч строк (conversion/*.py), и она
# расходится с загрузчиком при каждом обновлении llama.cpp.
#
# ИДЕНТИЧНОСТЬ (R486). Образ обязан доказать, чем собран: /app/identity.json +
# -X pkg/version.*. Стража `GIT_COMMIT not supplied` валит сборку с пустым arg —
# иначе на стенд молча уезжает образ с чужим штампом (это уже случалось).
# =============================================================================

ARG GO_BASE=dockerhub.timeweb.cloud/library/golang:1.24.3-alpine
ARG PYTHON_BASE=dockerhub.timeweb.cloud/library/python:3.12-slim
# R508: ARG для FROM объявляются ДО первого FROM (глобальная область).
FROM ${GO_BASE} AS builder

WORKDIR /build
# git нужен `go mod download` для приватных модулей; ca-certificates — для https.
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Только Go-исходники: `COPY . .` затянуло бы в контекст c/llama.cpp (сотни
# мегабайт сабмодуля), который этому сервису не нужен вовсе — он не линкует
# llama.cpp, а вызывает python-конвертер процессом.
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY services ./services

# Идентичность сборки: значения считает scripts/build-identity.ps1 и передаёт
# build-arg'ами release-all.ps1 / up-stack.ps1 / compose (см.
# deployments/docker-compose.stack.yml). Дефолтов у них НЕТ намеренно.
ARG SERVICE=gguf-reexport
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR
ARG LLAMA_BUILD
ARG LLAMA_BUILD_DATE

# Стража R486: без коммита образ не собирается. Сообщение обязано называть
# причину и путь решения — «exit 1» без текста стоило нескольких итераций.
RUN [ -n "${GIT_COMMIT}" ] || (echo "FATAL: GIT_COMMIT not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1)

# llama_stub — чтобы сборка не требовала cgo/libllama: сервису llama.cpp не нужен
# вообще (он вызывает python-конвертер процессом, а не линкует его).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags llama_stub \
    -ldflags "-w -s \
      -X ollama-loadbalancer/pkg/version.Service=${SERVICE} \
      -X ollama-loadbalancer/pkg/version.Version=${VERSION} \
      -X ollama-loadbalancer/pkg/version.ImageTag=${IMAGE_TAG} \
      -X ollama-loadbalancer/pkg/version.Commit=${GIT_COMMIT} \
      -X ollama-loadbalancer/pkg/version.CommitFull=${GIT_COMMIT_FULL} \
      -X ollama-loadbalancer/pkg/version.CommitDate=${GIT_COMMIT_DATE} \
      -X ollama-loadbalancer/pkg/version.BuildDate=${BUILD_DATE} \
      -X ollama-loadbalancer/pkg/version.Author=${BUILD_AUTHOR} \
      -X ollama-loadbalancer/pkg/version.LlamaCommit=${LLAMA_BUILD} \
      -X ollama-loadbalancer/pkg/version.LlamaDate=${LLAMA_BUILD_DATE}" \
    -o /out/gguf-reexport ./services/gguf-reexport

# -----------------------------------------------------------------------------
# light — рабочий образ по умолчанию
# -----------------------------------------------------------------------------
FROM ${PYTHON_BASE} AS light

# PYTHONUNBUFFERED: события конвертера должны доезжать до Go-хоста сразу, иначе
# прогресс «замирает» (буферизация stdout в pipe).
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    REEXPORT_HOST=0.0.0.0 \
    REEXPORT_PORT=18095 \
    REEXPORT_MODELS_DIR=/app/models \
    REEXPORT_JOBS_DIR=/app/jobs \
    REEXPORT_PYTHON=/usr/local/bin/python3 \
    REEXPORT_DRIVER=/app/py/reexport_job.py \
    REEXPORT_LLAMA_CONVERTER=/app/llama/convert_hf_to_gguf.py

# Non-root: сервис пишет только в /app/jobs и в каталог результата, оба — тома.
RUN groupadd -g 1000 app && useradd -u 1000 -g app -M -s /usr/sbin/nologin app

WORKDIR /app
COPY --from=builder /out/gguf-reexport /app/gguf-reexport
COPY services/gguf-reexport/py/reexport_job.py /app/py/reexport_job.py
# Скрипт патча — ОДИН источник правды в репозитории (scripts/), а не копия рядом с
# драйвером: иначе правка патча в CI и в образе расходится молча. Попадает в
# контекст сборки благодаря точечному !scripts/... в .dockerignore.
COPY scripts/gguf_loop_meta_patch.py /app/py/gguf_loop_meta_patch.py
RUN mkdir -p /app/jobs /app/models && chown -R app:app /app

USER app

# R486: запечённый манифест сборки. Пишется ПОСЛЕ USER, чтобы файл сразу
# принадлежал app (иначе chown всего /app на каждом слое).
ARG SERVICE=gguf-reexport
ARG VERSION
ARG IMAGE_TAG
ARG GIT_COMMIT
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE
ARG BUILD_AUTHOR
ARG LLAMA_BUILD
ARG LLAMA_BUILD_DATE
RUN printf '{"service":"%s","version":"%s","image_tag":"%s","commit":"%s","commit_full":"%s","commit_date":"%s","build_date":"%s","author":"%s","llama_commit":"%s","llama_date":"%s"}\n' \
      "$SERVICE" "$VERSION" "$IMAGE_TAG" "$GIT_COMMIT" "$GIT_COMMIT_FULL" "$GIT_COMMIT_DATE" "$BUILD_DATE" "$BUILD_AUTHOR" "$LLAMA_BUILD" "$LLAMA_BUILD_DATE" \
      > /app/identity.json && chmod 0644 /app/identity.json

LABEL org.opencontainers.image.title="Ollama Legion GGUF Re-export"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_COMMIT_FULL}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion"
LABEL org.opencontainers.image.authors="${BUILD_AUTHOR}"
LABEL org.opencontainers.image.description="GGUF re-export service (llama.cpp converter, llama=${LLAMA_BUILD})"
# Тот же набор меток, что у остальных образов: scripts/lib-release-manifest.ps1
# читает идентичность из OCI-меток, а не из .env.
LABEL ru.ollamalegion.git.commit="${GIT_COMMIT}"
LABEL ru.ollamalegion.image.tag="${IMAGE_TAG}"
LABEL ru.ollamalegion.llama.commit="${LLAMA_BUILD}"
LABEL ru.ollamalegion.llama.date="${LLAMA_BUILD_DATE}"

EXPOSE 18095

# Healthcheck — свой же бинарь (в образе нет curl/wget, и не надо: лишний слой
# ради проверки живости). Compose задаёт свой, этот — для `docker run` без него.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/gguf-reexport", "-healthcheck"]

ENTRYPOINT ["/app/gguf-reexport"]
CMD []

# -----------------------------------------------------------------------------
# full — плюс полный ре-экспорт из HF-чекпойнта (torch + конвертер llama.cpp)
# -----------------------------------------------------------------------------
# Собирается только осознанно:
#   docker build -f docker/gguf-reexport/Dockerfile --target full \
#     --build-arg WITH_TORCH=1 --build-arg GIT_COMMIT=... -t diag-reexport-full .
# ВНИМАНИЕ: порядок слоёв важен — pip до COPY конвертера, чтобы правка конвертера
# не инвалидировала 2.5 ГБ torch.
FROM light AS full

USER root
ARG WITH_TORCH=0
ARG CONVERTER_REQUIREMENTS=/app/llama/requirements

# Конвертер llama.cpp + его gguf-py: конвертер импортирует пакет `gguf` из
# gguf-py (sys.path в первой строке), поэтому копируем оба.
COPY c/llama.cpp/convert_hf_to_gguf.py /app/llama/convert_hf_to_gguf.py
COPY c/llama.cpp/conversion/ /app/llama/conversion/
COPY c/llama.cpp/gguf-py/ /app/llama/gguf-py/
COPY c/llama.cpp/requirements/ /app/llama/requirements/

# Зависимости конвертера — ИЗ САБМОДУЛЯ, а не переписанные руками: список пакетов
# меняется вместе с llama.cpp, и своя копия разошлась бы с ним.
#
# CUDA НЕ СТАВИТСЯ: ре-экспорт весов — это арифметика на CPU; карта нужна
# инференсу, не конвертеру. Поэтому индекс — CPU-сборка torch (~200 МБ колесо
# вместо гигабайтов CUDA).
RUN if [ "$WITH_TORCH" = "1" ]; then \
      pip install --no-cache-dir -r /app/llama/requirements/requirements-convert_hf_to_gguf.txt ; \
    else \
      echo "WITH_TORCH=0: цель hf_to_gguf будет объявлена недоступной в /api/reexport/capabilities" ; \
    fi

RUN chown -R app:app /app/llama
USER app
