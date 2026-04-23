# Web UI Dockerfile
# Простой nginx контейнер для статического Web UI

FROM nginx:alpine

WORKDIR /usr/share/nginx/html

# Удаление стандартных файлов nginx
RUN rm -rf ./*

# Копирование статических файлов UI
COPY dist/ .

# Копирование конфигурации nginx
COPY docker/webui/nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:80/ || exit 1

CMD ["nginx", "-g", "daemon off;"]
