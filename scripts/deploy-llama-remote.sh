#!/bin/bash
# ============================================================
# OllamaLegion — Удалённый деплоймент llama.cpp узла
# ============================================================
# Копирует docker-compose.llama.cpp.yml, .env и конфигурацию
# на удалённую машину по SSH и запускает контейнеры.
#
# Использование:
#   ./deploy-llama-remote.sh <user@host> <models_dir> <balancer_url> [node_name] [api_token] [gpu_count] [variant]
#
# Аргументы:
#   user@host     — SSH-адрес удалённой машины (обязательно)
#   models_dir    — путь к GGUF-моделям на удалённой машине (обязательно)
#   balancer_url  — URL балансера для регистрации (обязательно)
#   node_name     — имя узла (по умолчанию: llama-{host})
#   api_token     — API токен безопасности (опционально)
#   gpu_count     — количество GPU (по умолчанию: 1)
#   variant       — cpu или gpu (по умолчанию: gpu)
#
# Примеры:
#   # LAN с GPU:
#   ./deploy-llama-remote.sh root@192.0.2.50 /data/models http://192.0.2.10:18081
#
#   # WAN с GPU:
#   ./deploy-llama-remote.sh root@node.example.com /data/models https://balancer.example.com:18081 llama-gpu-1 mytoken123
#
#   # CPU-only:
#   ./deploy-llama-remote.sh root@192.0.2.60 /data/models http://192.0.2.10:18081 llama-cpu-1 "" 0 cpu
# ============================================================

set -euo pipefail

# --- Проверка аргументов ---
if [ $# -lt 3 ]; then
    echo "Ошибка: недостаточно аргументов."
    echo "Использование: $0 <user@host> <models_dir> <balancer_url> [node_name] [api_token] [gpu_count] [variant]"
    exit 1
fi

SSH_HOST="$1"
MODELS_DIR="$2"
BALANCER_URL="$3"
NODE_NAME="${4:-}"
API_TOKEN="${5:-}"
GPU_COUNT="${6:-1}"
VARIANT="${7:-gpu}"

# Автоопределение имени узла из хоста
if [ -z "$NODE_NAME" ]; then
    NODE_NAME="llama-${SSH_HOST#*@}"
    NODE_NAME="${NODE_NAME//./-}"
fi

# --- Пути в проекте ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
REMOTE_DIR="/opt/ollamalegion"

# --- Выбор compose-файла ---
if [ "$VARIANT" = "cpu" ]; then
    COMPOSE_FILE="docker-compose.llama.cpu.yml"
    echo ">>> Режим: CPU-only"
else
    COMPOSE_FILE="docker-compose.llama.cpp.yml"
    echo ">>> Режим: GPU (count=$GPU_COUNT)"
fi

# --- Отображение конфигурации ---
echo "============================================"
echo " Деплоймент llama.cpp узла"
echo "============================================"
echo " Хост:            $SSH_HOST"
echo " Модели:          $MODELS_DIR"
echo " Балансер:        $BALANCER_URL"
echo " Узел:            $NODE_NAME"
echo " GPU:             $GPU_COUNT"
echo " Compose:         $COMPOSE_FILE"
echo " Удалённый путь:  $REMOTE_DIR"
echo "============================================"
echo ""

# --- Шаг 1: Создание директорий на удалённой машине ---
echo ">>> Шаг 1/5: Создание директорий на $SSH_HOST ..."
ssh "$SSH_HOST" "mkdir -p $REMOTE_DIR/config $REMOTE_DIR/models"

# --- Шаг 2: Копирование compose-файла ---
echo ">>> Шаг 2/5: Копирование $COMPOSE_FILE ..."
scp "$PROJECT_DIR/deployments/$COMPOSE_FILE" "$SSH_HOST:$REMOTE_DIR/docker-compose.yml"

# --- Шаг 3: Копирование конфигурации cppworker.env ---
echo ">>> Шаг 3/5: Копирование cppworker.env ..."
TMP_ENV=$(mktemp)
cp "$PROJECT_DIR/config/cppworker.example.env" "$TMP_ENV"

# Подстановка значений в .env
sed -i "s|^NODE_NAME=.*|NODE_NAME=$NODE_NAME|" "$TMP_ENV"
sed -i "s|^BALANCER_URL=.*|BALANCER_URL=$BALANCER_URL|" "$TMP_ENV"
sed -i "s|^LLAMA_MODELS_DIR=.*|LLAMA_MODELS_DIR=/models|" "$TMP_ENV"
sed -i "s|^API_TOKEN=.*|API_TOKEN=$API_TOKEN|" "$TMP_ENV"

if [ "$VARIANT" = "cpu" ]; then
    sed -i "s|^LLAMA_N_GPU_LAYERS=.*|LLAMA_N_GPU_LAYERS=0|" "$TMP_ENV"
    sed -i "s|^LLAMA_FLASH_ATTN=.*|LLAMA_FLASH_ATTN=false|" "$TMP_ENV"
fi

scp "$TMP_ENV" "$SSH_HOST:$REMOTE_DIR/config/cppworker.env"
rm -f "$TMP_ENV"

# --- Шаг 4: Создание .env для docker compose ---
echo ">>> Шаг 4/5: Создание .env для docker compose ..."
TMP_DOTENV=$(mktemp)
cat > "$TMP_DOTENV" << DOTENVEOF
MODELS_DIR=$MODELS_DIR
BALANCER_URL=$BALANCER_URL
NODE_NAME=$NODE_NAME
GPU_COUNT=$GPU_COUNT
API_TOKEN=$API_TOKEN
LLAMA_HTTP_PORT=18091
LLAMA_GRPC_PORT=19000
AGENT_PORT=18032
HEARTBEAT_INTERVAL=30
COLLECT_INTERVAL=15
LLAMA_N_GPU_LAYERS=-1
LLAMA_CTX_SIZE=8192
LLAMA_BATCH_SIZE=512
LLAMA_FLASH_ATTN=true
LLAMA_IDLE_UNLOAD=30m
LLAMA_ENFORCE_REPLICATION=false
NODE_LABELS=zone=lan
DOTENVEOF

if [ "$VARIANT" = "cpu" ]; then
    sed -i "s|^LLAMA_N_GPU_LAYERS=.*|LLAMA_N_GPU_LAYERS=0|" "$TMP_DOTENV"
    sed -i "s|^LLAMA_FLASH_ATTN=.*|LLAMA_FLASH_ATTN=false|" "$TMP_DOTENV"
    sed -i "s|^LLAMA_CTX_SIZE=.*|LLAMA_CTX_SIZE=4096|" "$TMP_DOTENV"
    sed -i "s|^LLAMA_BATCH_SIZE=.*|LLAMA_BATCH_SIZE=256|" "$TMP_DOTENV"
    sed -i "s|zone=lan|zone=lan,cpu_only=true|" "$TMP_DOTENV"
fi

scp "$TMP_DOTENV" "$SSH_HOST:$REMOTE_DIR/.env"
rm -f "$TMP_DOTENV"

# --- Шаг 5: Запуск контейнеров ---
echo ">>> Шаг 5/5: Запуск контейнеров на $SSH_HOST ..."
ssh "$SSH_HOST" "cd $REMOTE_DIR && docker compose up -d"

# --- Проверка статуса ---
echo ""
echo ">>> Ожидание запуска (15 секунд) ..."
sleep 15

echo ""
echo ">>> Статус контейнеров:"
ssh "$SSH_HOST" "cd $REMOTE_DIR && docker compose ps"

echo ""
echo "============================================"
echo " Деплоймент завершён!"
echo "============================================"
echo ""
echo "Проверка health:"
echo "  ssh $SSH_HOST 'curl -s http://localhost:18091/api/v1/cppworker/health'"
echo ""
echo "Просмотр логов:"
echo "  ssh $SSH_HOST 'cd $REMOTE_DIR && docker compose logs -f'"
echo ""
echo "Остановка:"
echo "  ssh $SSH_HOST 'cd $REMOTE_DIR && docker compose down'"
echo ""
echo "Проверка регистрации на балансере:"
echo "  curl -s $BALANCER_URL/api/v1/cluster/backends | jq"
echo ""