#!/usr/bin/env bash
# =============================================================================
# Запуск bundled стека Ollama Legion: cppworker-gpu + balancer + webui
# =============================================================================
# Использование:
#   ./scripts/start-bundled.sh           # запуск в фоне
#   ./scripts/start-bundled.sh rebuild   # с пересборкой образов
#   ./scripts/start-bundled.sh down      # остановить
#   ./scripts/start-bundled.sh logs      # показать логи
#   ./scripts/start-bundled.sh status    # статус контейнеров
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$SCRIPT_DIR/../deployments"
ENV_FILE="$DEPLOY_DIR/.env.bundled"
ENV_EXAMPLE="$DEPLOY_DIR/.env.bundled.example"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.cppworker-bundled.yml"
BUNDLED_CONFIG="$SCRIPT_DIR/../config/config.bundled.json"
DEFAULT_CONFIG="$SCRIPT_DIR/../config/config.json"

cd "$DEPLOY_DIR"

green() { printf "\033[32m%s\033[0m\n" "$*"; }
cyan()  { printf "\033[36m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()   { printf "\033[31m%s\033[0m\n" "$*"; }

step() { echo ""; cyan "==> $*"; }

# ----- init .env.bundled -----
if [[ ! -f "$ENV_FILE" ]]; then
    step "Создаю .env.bundled из .env.bundled.example"
    cp "$ENV_EXAMPLE" "$ENV_FILE"
    green "    Скопирован $ENV_FILE"
    yellow "    Обязательно смените CPPWORKER_API_TOKEN на свой (или оставьте для теста)!"
fi

# ----- init config.json из bundled -----
if [[ -f "$BUNDLED_CONFIG" ]]; then
    if [[ -f "$DEFAULT_CONFIG" ]] && [[ -z "${OL_BUNDLED_FORCE_OVERWRITE_CONFIG:-}" ]]; then
        if ! grep -q '"Ollama Legion — bundled config' "$DEFAULT_CONFIG"; then
            step "config.json уже существует и не выглядит как bundled"
            yellow "    Использую существующий config.json"
        fi
    else
        step "Копирую config.bundled.json → config.json"
        cp "$BUNDLED_CONFIG" "$DEFAULT_CONFIG"
        # Подставляем токен
        TOKEN=$(grep '^CPPWORKER_API_TOKEN=' "$ENV_FILE" | head -1 | cut -d= -f2-)
        if [[ -n "$TOKEN" ]]; then
            # Заменяем placeholder токена
            sed -i.bak "s|\"name\": \"bundled-default\"|\"name\": \"$TOKEN\"|g" "$DEFAULT_CONFIG" || true
            rm -f "$DEFAULT_CONFIG.bak"
            green "    auth.tokens[0].name = $TOKEN"
        fi
    fi
fi

COMPOSE_ARGS=(-f "$COMPOSE_FILE" --env-file "$ENV_FILE")

case "${1:-up}" in
    down)
        step "Останавливаю стек"
        docker compose "${COMPOSE_ARGS[@]}" down
        ;;
    logs)
        step "Логи (Ctrl+C для выхода)"
        docker compose "${COMPOSE_ARGS[@]}" logs -f
        ;;
    status)
        step "Статус контейнеров"
        docker compose "${COMPOSE_ARGS[@]}" ps
        echo ""
        step "Health"
        docker compose "${COMPOSE_ARGS[@]}" ps --format json 2>/dev/null | \
            (command -v jq >/dev/null && jq -r '.[] | "\(.Name): \(.Health // "no healthcheck")"' || \
            docker compose "${COMPOSE_ARGS[@]}" ps) || true
        ;;
    rebuild)
        step "Пересобираю и запускаю"
        docker compose "${COMPOSE_ARGS[@]}" up -d --build
        print_success_info
        ;;
    up|"")
        step "Запускаю bundled стек"
        docker compose "${COMPOSE_ARGS[@]}" up -d
        print_success_info
        ;;
    *)
        red "Unknown command: $1"
        echo "Usage: $0 [up|rebuild|down|logs|status]"
        exit 1
        ;;
esac

print_success_info() {
    if [[ $? -eq 0 ]]; then
        step "Стек запущен"

        # -----------------------------------------------------------------------------
        # Smoke-check: убедиться, что DNS bundled-net зарегистрирован корректно
        # (защита от регрессии 2026-06-08: при внешнем `docker run` контейнер
        # loadbalancer не оказывался в `ol-bundled-net`, и webui получал 502 на
        # /ollama/api/* при первом запросе браузера OpenWebUI).
        # -----------------------------------------------------------------------------
        step "Smoke-check: проверяю DNS bundled-net и путь /ollama/* → balancer"
        sleep 5
        WEBUI_PORT=$(grep '^WEBUI_PORT=' "$ENV_FILE" | head -1 | cut -d= -f2-)
        WEBUI_PORT=${WEBUI_PORT:-18083}
        DNS_OK=0
        HTTP_OK=0
        if docker exec ol-bundled-webui nslookup loadbalancer 127.0.0.11 2>/dev/null | grep -qE 'Address:[[:space:]]+172\.[0-9]+\.[0-9]+\.[0-9]+'; then
            DNS_OK=1
        fi
        HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -H 'Origin: http://localhost' "http://localhost:${WEBUI_PORT}/ollama/api/version" 2>/dev/null || echo "000")
        if [[ "$HTTP_CODE" == "200" ]]; then
            HTTP_OK=1
        fi
        if [[ $DNS_OK -eq 1 && $HTTP_OK -eq 1 ]]; then
            green "    DNS loadbalancer в bundled-net: OK"
            green "    /ollama/api/version → 200 OK: OK"
        else
            yellow "    DNS loadbalancer в bundled-net: $([[ $DNS_OK -eq 1 ]] && echo OK || echo FAIL)"
            yellow "    /ollama/api/version → 200 OK: $([[ $HTTP_OK -eq 1 ]] && echo OK || echo FAIL)"
            echo ""
            yellow "    ВНИМАНИЕ: nginx в webui не может достучаться до балансера по DNS."
            yellow "    Типичное решение: 'docker network connect --alias loadbalancer ollama-legion-bundled-net ol-bundled-balancer'"
            yellow "    Подробности: docs/test-report-2026-06-08-cppworker-multiclient.md (Phase 10)"
        fi

        echo ""
        green "Полезные адреса:"
        echo "  WebUI:          http://localhost:18083"
        echo "  Balancer API:   http://localhost:18080 (OpenAI-совместимый)"
        echo "  Balancer admin: http://localhost:18081 (X-API-Token required)"
        echo "  CppWorker:      http://localhost:18092 (прямой доступ)"
        echo ""
        green "Проверить регистрацию cppworker в балансировщике:"
        echo "  curl -H 'X-API-Token: \$(grep CPPWORKER_API_TOKEN $ENV_FILE | cut -d= -f2)' http://localhost:18081/api/v1/backends"
        echo ""
        green "Посмотреть логи:"
        echo "  $0 logs"
    fi
}