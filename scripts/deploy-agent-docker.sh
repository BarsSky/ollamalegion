#!/bin/bash
#
# Скрипт для быстрого развертывания агента Ollama Load Balancer на GPU серверах
#
# Использование:
#   ./deploy-agent-docker.sh [OPTIONS]
#
# Параметры:
#   --balancer-url  URL балансировщика (обязательно)
#   --agent-id      Уникальный идентификатор агента (обязательно)
#   --agent-port    Порт агента (по умолчанию: 18032)
#   --ollama-url    URL локального Ollama (по умолчанию: http://localhost:11434)
#   --nvml-enabled  Включить NVML (по умолчанию: true)
#   --metrics-interval Интервал отправки метрик (по умолчанию: 5s)
#   --help          Показать справку
#
# Примеры:
#   ./deploy-agent-docker.sh --balancer-url http://192.168.1.100:18081 --agent-id gpu-1
#   ./deploy-agent-docker.sh -b http://lb:18081 -i gpu-2 -p 18032
#

set -e

# Цвета для вывода
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Параметры по умолчанию
BALANCER_URL=""
AGENT_ID=""
AGENT_PORT="18032"
OLLAMA_URL="http://localhost:11434"
NVML_ENABLED="true"
METRICS_INTERVAL="5s"
HEARTBEAT_INTERVAL="3s"
COLLECT_INTERVAL="5s"
LOG_LEVEL="info"
LOG_FORMAT="text"
COMPOSE_FILE="docker-compose.agent.yml"

# Функция для вывода справки
show_help() {
    cat << EOF
Скрипт для развертывания агента Ollama Load Balancer

Использование:
    $0 [OPTIONS]

Обязательные параметры:
    -b, --balancer-url      URL балансировщика (например, http://192.168.1.100:18081)
    -i, --agent-id          Уникальный идентификатор агента (например, gpu-1)

Опциональные параметры:
    -p, --agent-port        Порт агента (по умолчанию: 18032)
    -o, --ollama-url        URL локального Ollama (по умолчанию: http://localhost:11434)
    -n, --nvml-enabled      Включить NVML (по умолчанию: true)
    -m, --metrics-interval  Интервал отправки метрик (по умолчанию: 5s)
    -H, --heartbeat-interval Интервал heartbeat (по умолчанию: 3s)
    -c, --collect-interval  Интервал сбора метрик (по умолчанию: 5s)
    -l, --log-level         Уровень логиров��ния (по умолчанию: info)
    -f, --log-format        Формат логов (по умолчанию: text)
    --compose-file          Путь к docker-compose файлу (по умолчанию: docker-compose.agent.yml)
    -h, --help              Показать эту справку

Примеры:
    $0 -b http://192.168.1.100:18081 -i gpu-1
    $0 --balancer-url http://lb:18081 --agent-id gpu-2 --agent-port 18032
    $0 -b http://lb:18081 -i gpu-3 -m 10s -l debug

EOF
    exit 0
}

# Парсинг аргументов командной строки
while [[ $# -gt 0 ]]; do
    case $1 in
        -b|--balancer-url)
            BALANCER_URL="$2"
            shift 2
            ;;
        -i|--agent-id)
            AGENT_ID="$2"
            shift 2
            ;;
        -p|--agent-port)
            AGENT_PORT="$2"
            shift 2
            ;;
        -o|--ollama-url)
            OLLAMA_URL="$2"
            shift 2
            ;;
        -n|--nvml-enabled)
            NVML_ENABLED="$2"
            shift 2
            ;;
        -m|--metrics-interval)
            METRICS_INTERVAL="$2"
            shift 2
            ;;
        -H|--heartbeat-interval)
            HEARTBEAT_INTERVAL="$2"
            shift 2
            ;;
        -c|--collect-interval)
            COLLECT_INTERVAL="$2"
            shift 2
            ;;
        -l|--log-level)
            LOG_LEVEL="$2"
            shift 2
            ;;
        -f|--log-format)
            LOG_FORMAT="$2"
            shift 2
            ;;
        --compose-file)
            COMPOSE_FILE="$2"
            shift 2
            ;;
        -h|--help)
            show_help
            ;;
        *)
            echo -e "${RED}Неизвестный параметр: $1${NC}"
            echo "Используйте --help для получения справки"
            exit 1
            ;;
    esac
done

# Проверка обязательных параметров
if [[ -z "$BALANCER_URL" ]]; then
    echo -e "${RED}Ошибка: Параметр --balancer-url (-b) обязателен${NC}"
    echo "Используйте --help для получения справки"
    exit 1
fi

if [[ -z "$AGENT_ID" ]]; then
    echo -e "${RED}Ошибка: Параметр --agent-id (-i) обязателен${NC}"
    echo "Используйте --help для получения справки"
    exit 1
fi

# Определение директории скрипта
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
DEPLOYMENTS_DIR="$PROJECT_ROOT/deployments"

# Проверка существования docker-compose файла
if [[ ! -f "$DEPLOYMENTS_DIR/$COMPOSE_FILE" ]]; then
    echo -e "${RED}Ошибка: Файл $DEPLOYMENTS_DIR/$COMPOSE_FILE не найден${NC}"
    exit 1
fi

echo -e "${GREEN}╔═══════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║      Ollama Load Balancer - Развертывание агента          ║${NC}"
echo -e "${GREEN}╠═══════════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║ Agent ID:       ${AGENT_ID}%*s║${NC}" 46
echo -e "${GREEN}║ Balancer URL:   ${BALANCER_URL}%*s║${NC}" 46
echo -e "${GREEN}║ Agent Port:     ${AGENT_PORT}%*s║${NC}" 46
echo -e "${GREEN}║ Ollama URL:     ${OLLAMA_URL}%*s║${NC}" 46
echo -e "${GREEN}║ NVML Enabled:   ${NVML_ENABLED}%*s║${NC}" 46
echo -e "${GREEN}║ Metrics:        ${METRICS_INTERVAL}%*s║${NC}" 46
echo -e "${GREEN}╚═══════════════════════════════════════════════════════════╝${NC}"

# Проверка Docker
echo -e "\n${YELLOW}[1/4] Проверка Docker...${NC}"
if ! command -v docker &> /dev/null; then
    echo -e "${RED}Ошибка: Docker не найден${NC}"
    exit 1
fi
docker --version
echo -e "${GREEN}Docker найден${NC}"

# Проверка docker-compose
echo -e "\n${YELLOW}[2/4] Проверка docker-compose...${NC}"
if command -v docker-compose &> /dev/null; then
    COMPOSE_CMD="docker-compose"
elif docker compose version &> /dev/null; then
    COMPOSE_CMD="docker compose"
else
    echo -e "${RED}Ошибка: docker-compose не найден${NC}"
    exit 1
fi
$COMPOSE_CMD version --short
echo -e "${GREEN}docker-compose найден${NC}"

# Проверка NVIDIA Container Toolkit (опционально)
echo -e "\n${YELLOW}[3/4] Проверка NVIDIA Container Toolkit...${NC}"
if docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi &> /dev/null; then
    echo -e "${GREEN}NVIDIA Container Toolkit доступен${NC}"
else
    echo -e "${YELLOW}Предупреждение: NVIDIA Container Toolkit может быть недоступен${NC}"
    echo "GPU метрики могут быть недоступны"
fi

# Развертывание
echo -e "\n${YELLOW}[4/4] Развертывание агента...${NC}"

cd "$DEPLOYMENTS_DIR"

# Экспорт переменных окружения
export AGENT_ID
export BALANCER_URL
export AGENT_PORT
export OLLAMA_URL
export NVML_ENABLED
export METRICS_INTERVAL
export HEARTBEAT_INTERVAL
export COLLECT_INTERVAL
export LOG_LEVEL
export LOG_FORMAT

# Запуск docker-compose
echo -e "\n${YELLOW}Запуск контейнера...${NC}"
$COMPOSE_CMD -f "$COMPOSE_FILE" up -d

# Проверка статуса
echo -e "\n${YELLOW}Проверка статуса контейнера...${NC}"
sleep 2
$COMPOSE_CMD -f "$COMPOSE_FILE" ps

echo -e "\n${GREEN}═══════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}Агент успешно развернут!${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
echo -e "\n${YELLOW}Полезные команды:${NC}"
echo -e "  Просмотр логов:     ${COMPOSE_CMD} -f $COMPOSE_FILE logs -f"
echo -e "  Остановка:          ${COMPOSE_CMD} -f $COMPOSE_FILE down"
echo -e "  Перезапуск:         ${COMPOSE_CMD} -f $COMPOSE_FILE restart"
echo -e "  Статус:             ${COMPOSE_CMD} -f $COMPOSE_FILE ps"
echo -e "\n${YELLOW}Проверка метрик:${NC}"
echo -e "  curl http://localhost:$AGENT_PORT/metrics"
