#!/bin/sh
set -e

CONFIG_WRITABLE="/app/data/config.json"

# Приоритет источника конфига:
#   1. LB_CONFIG_PATH env var (из docker-compose environment)
#   2. /app/config/config.json (volume mount из host-директории ../config)
#   3. /app/config.json (example-конфиг, встроенный в образ COPY)
if [ -n "$LB_CONFIG_PATH" ]; then
    CONFIG_SOURCE="$LB_CONFIG_PATH"
    echo "[entrypoint] Using LB_CONFIG_PATH: $CONFIG_SOURCE"
elif [ -f "/app/config/config.json" ]; then
    CONFIG_SOURCE="/app/config/config.json"
    echo "[entrypoint] Using mounted config: $CONFIG_SOURCE"
else
    CONFIG_SOURCE="/app/config.json"
    echo "[entrypoint] Using built-in example config: $CONFIG_SOURCE"
fi

# Копируем конфиг в writable-директорию при первом запуске
if [ ! -f "$CONFIG_WRITABLE" ]; then
    echo "[entrypoint] Copying initial config to writable location: $CONFIG_WRITABLE"
    cp "$CONFIG_SOURCE" "$CONFIG_WRITABLE"
else
    # Миграция: добавляем пропущенные поля из шаблона (сохраняет существующие бэкенды)
    NEEDS_FIX=0
    if ! grep -q '"initialized"' "$CONFIG_WRITABLE"; then
        echo "[entrypoint] Adding missing 'initialized' field"
        NEEDS_FIX=1
    fi
    if ! grep -q '"backendEngine"' "$CONFIG_WRITABLE"; then
        echo "[entrypoint] Adding missing 'backendEngine' field"
        NEEDS_FIX=1
    fi
    if [ "$NEEDS_FIX" -eq 1 ]; then
        # Вставляем недостающие поля перед последней }
        INSERT='  ,"initialized": false\
  ,"backendEngine": "llama_cpp"'
        sed -i '/^}$/i\'"$INSERT" "$CONFIG_WRITABLE"
    fi
fi

# Запускаем балансер с writable-конфигом
exec ./balancer -config "$CONFIG_WRITABLE" "$@"
