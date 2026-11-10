# Web UI Dockerfile
# Сборка статического Web UI на nginx с подстановкой ${VAR} в nginx.conf
#
# R67a (2026-09-23): образ собирается БЕЗ сетевых запросов (offline-build),
# кроме заранее загруженного базового образа:
#   * все статические ассеты (css/, js/, img/, webfonts/ — включая шрифты
#     Font Awesome) копируются из репозитория, внешних CDN нет;
#   * `apk add gettext` УБРАН: envsubst больше не нужен, подстановку делает
#     busybox sed в entrypoint.sh (см. комментарий там). Раньше этот RUN
#     требовал интернета, и на машине без сети WebUI нельзя было пересобрать.
# Регресс-проверка ассетов: scripts/check_webui_assets.py (в CI-джобе
# webui-static-checks) — падает, если страница ссылается на внешний CDN или на
# несуществующий локальный файл.

FROM dockerhub.timeweb.cloud/library/nginx:alpine

# === Build args: пробрасываем git tag / commit / build date в рантайм ===
# Используются entrypoint.sh для подстановки в js/modules/config.js
# как window.WEBUI_CONFIG.VERSION / GIT_COMMIT / BUILD_DATE.
# Передаются через `docker build --build-arg VERSION=...`.
# По умолчанию `dev` (локальная сборка без тега).
#
# R486 (2026-11-10): идентичность ЗАПЕКАЕТСЯ в образ — файлом
# /usr/share/nginx/html/identity.json (см. RUN ниже) и OCI-метками. entrypoint.sh
# читает ФАЙЛ первым и падает на env только когда файла нет (локальный запуск).
# Go-бинарника в этом образе нет, поэтому стражи (guard) тоже нет: сборка WebUI
# должна оставаться возможной без выпускного пайплайна, дефолты `dev`/`unknown`
# сохранены намеренно.
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_COMMIT_FULL
ARG GIT_COMMIT_DATE
ARG BUILD_DATE=unknown
ARG IMAGE_TAG
ARG BUILD_AUTHOR
ARG SERVICE=webui
# R486: у «девовой» сборки стражи нет намеренно. Release-сборка (compose всегда
# передаёт RELEASE=1) обязана прийти с идентичностью: иначе WebUI запёк бы
# `dev/unknown` и остался единственным образом, который «не может доказать, что
# он собран».
ARG RELEASE=0
RUN if [ "$RELEASE" = "1" ]; then \
        test -n "$GIT_COMMIT" && test "$GIT_COMMIT" != "unknown" || \
            (echo "FATAL: GIT_COMMIT not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1); \
        test -n "$GIT_COMMIT_FULL" && test "$GIT_COMMIT_FULL" != "unknown" || \
            (echo "FATAL: GIT_COMMIT_FULL not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1); \
        test -n "$BUILD_DATE" && test "$BUILD_DATE" != "unknown" || \
            (echo "FATAL: BUILD_DATE not supplied (build arg) — identity must be baked, see scripts/build-identity.ps1" >&2; exit 1); \
    fi

# Делаем ARG доступными как ENV для entrypoint.sh
ENV VERSION=${VERSION} \
    GIT_COMMIT=${GIT_COMMIT} \
    BUILD_DATE=${BUILD_DATE}

# OCI image labels (для `docker inspect` / registries)
# R486: .revision — ПОЛНЫЙ коммит (раньше здесь был короткий), короткий лежит в
# ru.ollamalegion.git.commit; .version/.created уже брались из build-arg.
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT_FULL}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/BarsSky/ollamalegion" \
      org.opencontainers.image.title="ollama-legion/webui" \
      org.opencontainers.image.authors="${BUILD_AUTHOR}" \
      ru.ollamalegion.git.commit="${GIT_COMMIT}" \
      ru.ollamalegion.image.tag="${IMAGE_TAG}"

WORKDIR /usr/share/nginx/html

# Удаление стандартных файлов nginx
RUN rm -rf ./*

# Копирование статических файлов UI
#
# R66c (2026-09-22): HTML копируем МАСКОЙ, а не поимённо. Раньше здесь были
# только index.html, monitor.html, health.html, тогда как в webui/ есть ещё
# virtual-models.html, rpc-status.html, tp-pipeline.html. Навигация в
# index.html ссылается на virtual-models.html → в образе его не было →
# /virtual-models.html отдавал 404 и вкладка выглядела пустой. Маска
# закрывает этот класс расхождений: новый html-файл попадает в образ сам.
COPY webui/*.html ./
COPY webui/css/ ./css/
COPY webui/js/ ./js/
COPY webui/img/ ./img/
COPY webui/webfonts/ ./webfonts/
COPY webui/logo.svg ./
COPY webui/favicon-16x16.png .
COPY webui/favicon-32x32.png .
COPY webui/favicon.ico .

# R402 (2026-11-05): схема жизненного цикла запроса.
#
# ЗАЧЕМ ИМЕННО В ОБРАЗЕ. Оператор должен открыть схему для ТОЙ сборки, которая
# сейчас работает, и пройти по цепочке без GitHub и без сети: страница
# /scheme.html читает docs/scheme.json, а ссылки на техдокумент ведут на
# docs/architecture/*.md внутри того же образа. Поэтому копируем и структуру
# канваса (webui/docs), и сами документы (docs/architecture).
COPY webui/docs/ ./docs/
COPY docs/architecture/request-lifecycle.md ./docs/architecture/
COPY docs/architecture/request-lifecycle.ru.md ./docs/architecture/

# === R83 (2026-09-25): единый токен cache-busting на образ ===
#
# ЗАЧЕМ. В репозитории ?v= проставлен литералом (это нужно, чтобы страницу можно
# было отдавать прямо из webui/ без сборки), и исторически он был ПЕР-ФАЙЛОВЫМ:
# monitor.html тянул monitor-metrics.js?v=R66d рядом с ui-renderer.js?v=R77,
# index.html — сразу R66e/R66g/R66i. Следствие: частичный релиз давал СМЕШАННЫЕ
# поколения JS в одной странице, а забытый бамп версии оставлял у клиента старый
# файл из кэша (именно так правка R70 не доехала до оператора).
#
# Теперь в образе токен один на всю сборку и берётся из того же VERSION, что
# уходит в js/modules/config.js (window.WEBUI_CONFIG.VERSION). VERSION
# прокидывается compose'ом: `args: VERSION: ${WEBUI_VERSION:-dev}`,
# а rebuild_webui_*.ps1 выставляет WEBUI_VERSION=$Tag — руками править HTML
# при выпуске больше не нужно.
#
# Все *.html лежат в корне WORKDIR (COPY webui/*.html ./), поэтому glob достаточен.
#
# R134 (2026-10-16): токен — ХЕШ СОДЕРЖИМОГО статики, а не VERSION.
#
# ЖИВОЙ ДЕФЕКТ. Токен брался из VERSION (git describe или тег сборки). Пересборка
# того же коммита, сборка из «грязного» дерева или `docker load` уже собранного
# образа дают ТОТ ЖЕ токен при ДРУГОМ содержимом — и браузер оставляет старые
# js/css/i18n на год (`expires 1y` ниже в nginx.conf). Оператор видит прежний
# интерфейс: «нет вкладки», «иконки не отображаются», сырые ключи i18n вместо
# подписей. Хеш содержимого закрывает класс: меняется любой файл статики —
# меняется URL, и браузер обязан перезапросить.
RUN set -eux; \
    HASH="$(find . -type f \( -name '*.html' -o -name '*.js' -o -name '*.css' -o -name '*.svg' -o -name '*.png' -o -name '*.woff2' \) | sort | xargs md5sum | md5sum | cut -c1-12)"; \
    echo "static-cache-token=${HASH}"; \
    sed -i "s/?v=[A-Za-z0-9._-]*/?v=${HASH}/g" *.html

# Копирование nginx шаблона конфига (будет обработан envsubst при старте)
COPY webui/nginx.conf /etc/nginx/conf.d/default.conf.template

# Копирование entrypoint скрипта
COPY docker/webui/entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

# R486: identity.json — ЗАПЕЧЁННЫЙ манифест сборки (Go-бинарника здесь нет, но
# идентичность нужна тем же способом: файл, а не env).
#
# ПУТИ — ДВА ФАЙЛА ИЗ ОДНОГО printf, чтобы не разъехались:
#   /app/identity.json — КАНОНИЧЕСКИЙ путь контракта (pkg/version:
#     identityDefaultPathR486 = "/app/identity.json"; тот же путь у всех остальных
#     образов, поэтому `docker cp <образ>:/app/identity.json .` работает единообразно;
#     этот же путь называет deployments/docker-compose.stack.yml в описании сборки).
#   /usr/share/nginx/html/identity.json — тот же байт-в-байт файл в корне статики:
#     nginx отдаёт его как /identity.json, то есть «что запечено» видно curl'ом,
#     без docker exec. Каталога /app в nginx-образе нет — создаём его здесь.
RUN mkdir -p /app && \
    printf '{"service":"%s","version":"%s","image_tag":"%s","commit":"%s","commit_full":"%s","commit_date":"%s","build_date":"%s","author":"%s","llama_commit":"%s","llama_date":"%s","rpc_proto":"%s"}\n' "$SERVICE" "$VERSION" "$IMAGE_TAG" "$GIT_COMMIT" "$GIT_COMMIT_FULL" "$GIT_COMMIT_DATE" "$BUILD_DATE" "$BUILD_AUTHOR" "$LLAMA_BUILD" "$LLAMA_BUILD_DATE" "$RPC_PROTO" > /app/identity.json && chmod 0644 /app/identity.json && \
    cp /app/identity.json /usr/share/nginx/html/identity.json && chmod 0644 /usr/share/nginx/html/identity.json

EXPOSE 80

    HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
        CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:80/health || exit 1

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]
