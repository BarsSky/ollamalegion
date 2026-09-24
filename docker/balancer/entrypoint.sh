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

# R72 (2026-09-24): предупреждение о рассинхроне конфига.
#
# Найденный на живом стенде сценарий: балансер запускается с writable-копией
# /app/data/config.json (её же правит WebUI/overrides), а смонтированный
# /app/config/config.json копируется в неё ТОЛЬКО при первом старте. Поэтому
# правки смонтированного конфига (например новая секция balancing.placement)
# молча не доезжают до процесса: оператор видит старые настройки и не понимает
# почему. Если смонтированный конфиг новее рабочей копии — говорим об этом
# громко, а с LB_CONFIG_SYNC=1 обновляем рабочую копию (с бэкапом).
if [ -f "$CONFIG_WRITABLE" ] && [ -f "$CONFIG_SOURCE" ] && [ "$CONFIG_SOURCE" != "$CONFIG_WRITABLE" ] \
    && [ "$CONFIG_SOURCE" -nt "$CONFIG_WRITABLE" ]; then
    case "$(echo "$LB_CONFIG_SYNC" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes)
            BACKUP="${CONFIG_WRITABLE}.bak.$(date +%s)"
            cp "$CONFIG_WRITABLE" "$BACKUP"
            cp "$CONFIG_SOURCE" "$CONFIG_WRITABLE"
            echo "[entrypoint] LB_CONFIG_SYNC: рабочая копия обновлена из $CONFIG_SOURCE (бэкап: $BACKUP)"
            ;;
        *)
            echo "[entrypoint] WARNING: $CONFIG_SOURCE новее рабочей копии $CONFIG_WRITABLE —"
            echo "[entrypoint] WARNING: балансер стартует со СТАРЫМ конфигом, правки смонтированного файла НЕ применены."
            echo "[entrypoint] WARNING: варианты: LB_CONFIG_SYNC=1 (обновить рабочую копию с бэкапом) или"
            echo "[entrypoint] WARNING: пересоздать volume данных (docker compose rm -sf loadbalancer)."
            ;;
    esac
fi

# Запускаем балансер с writable-конфигом
exec ./balancer -config "$CONFIG_WRITABLE" "$@"
