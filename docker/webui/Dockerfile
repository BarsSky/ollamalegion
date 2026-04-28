# Web UI Dockerfile
# Сборка статического Web UI на nginx с поддержкой envsubst

FROM nginx:alpine

# Установка gettext для envsubst
RUN apk add --no-cache gettext

WORKDIR /usr/share/nginx/html

# Удаление стандартных файлов nginx
RUN rm -rf ./*

# Копирование статических файлов UI
COPY webui/index.html .
COPY webui/monitor.html .
COPY webui/css/ ./css/
COPY webui/js/ ./js/
COPY webui/img/ ./img/
COPY webui/logo.svg ./

# Копирование nginx шаблона конфига (будет обработан envsubst при старте)
COPY webui/nginx.conf /etc/nginx/conf.d/default.conf.template

# Копирование entrypoint скрипта
COPY docker/webui/entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:80/health || exit 1

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]