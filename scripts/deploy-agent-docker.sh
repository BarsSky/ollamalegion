#!/usr/bin/env bash
#
# Ollama Load Balancer - Agent Docker Deployment Script
#
# Автоматически определяет GPU-сервер и выбирает правильный compose-файл.
# Поддерживает ручное переключение CPU ↔ GPU через флаги.
#
# Использование:
#   ./deploy-agent-docker.sh [OPTIONS]
#
# Опции:
#   --gpu              Принудительно использовать GPU-конфигурацию
#   --cpu              Принудительно использовать CPU-конфигурацию
#   --env-file FILE    Путь к файлу .env (по умолчанию: .env)
#   --build            Пересобрать образ (аналог --build)
#   --pull             Pull base images перед сборкой
#   --no-build         Использовать существующий образ (без --build)
#   -h, --help         Показать справку
#
# Примеры:
#   # Автоопределение (рекомендуется):
#   ./deploy-agent-docker.sh --env-file deployments/.env
#
#   # Принудительно GPU:
#   ./deploy-agent-docker.sh --gpu --env-file .env
#
#   # Принудительно CPU:
#   ./deploy-agent-docker.sh --cpu --env-file .env
#
#   # Только перезапуск без пересборки:
#   ./deploy-agent-docker.sh --no-build --env-file .env
#
#   # С pull базовых образов:
#   ./deploy-agent-docker.sh --pull --build --env-file .env
#

set -euo pipefail

# --- Цвета для вывода ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# --- Значения по умолчанию ---
COMPOSE_DIR="$(cd "$(dirname "$0")/.." && pwd)/deployments"
ENV_FILE="${COMPOSE_DIR}/.env"
FORCE_GPU=false
FORCE_CPU=false
BUILD_FLAG="--build"
PULL_FLAG=""

# --- Функции ---

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_debug() {
    echo -e "${BLUE}[DEBUG]${NC} $1"
}

show_help() {
    sed -n '/^# Опции/,/^#$/p' "$0" | sed 's/^# //' | sed 's/^#//'
    exit 0
}

# Проверка наличия Docker
check_docker() {
    if ! command -v docker &> /dev/null; then
        log_error "Docker не найден. Установите Docker: https://docs.docker.com/get-docker/"
        exit 1
    fi

    if ! docker info &> /dev/null; then
        log_error "Docker daemon не запущен или нет прав доступа."
        log_info "Попробуйте: sudo usermod -aG docker $USER && newgrp docker"
        exit 1
    fi
}

# Проверка наличия NVIDIA Container Toolkit
check_nvidia_toolkit() {
    # Проверяем наличие nvidia-ctk
    if command -v nvidia-ctk &> /dev/null; then
        if nvidia-ctk --version &> /dev/null; then
            return 0
        fi
    fi

    # Проверяем наличие nvidia-docker2 runtime
    if docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q nvidia; then
        return 0
    fi

    return 1
}

# Проверка наличия GPU
has_gpu() {
    # 1. Проверяем nvidia-smi
    if command -v nvidia-smi &> /dev/null; then
        if nvidia-smi -L &> /dev/null && nvidia-smi -L | grep -q "GPU"; then
            return 0
        fi
    fi

    # 2. Проверяем через lspci
    if command -v lspci &> /dev/null; then
        if lspci 2>/dev/null | grep -qi "nvidia\\|vga.*nvidia\\|3d.*nvidia"; then
            return 0
        fi
    fi

    # 3. Проверяем /proc/driver/nvidia/version
    if [ -f /proc/driver/nvidia/version ]; then
        return 0
    fi

    return 1
}

# Определение режима (GPU/CPU)
detect_mode() {
    if [ "$FORCE_GPU" = true ]; then
        echo "gpu"
        return
    fi

    if [ "$FORCE_CPU" = true ]; then
        echo "cpu"
        return
    fi

    if has_gpu; then
        echo "gpu"
    else
        echo "cpu"
    fi
}

# Проверка .env файла
check_env_file() {
    if [ ! -f "$ENV_FILE" ]; then
        log_warn "Файл .env не найден: $ENV_FILE"
        log_info "Создаю шаблон .env из config/agent.example.env..."
        
        if [ -f "${COMPOSE_DIR}/../config/agent.example.env" ]; then
            cp "${COMPOSE_DIR}/../config/agent.example.env" "$ENV_FILE"
            log_info "Шаблон скопирован в $ENV_FILE"
            log_warn "ВАЖНО: Отредактируйте $ENV_FILE и замените placeholder-значения!"
        else
            log_error "config/agent.example.env не найден. Создайте .env вручную."
            exit 1
        fi
    fi
}

# Валидация обязательных переменных
validate_env() {
    local missing=0

    # Загружаем переменные из .env
    set -a
    # shellcheck source=/dev/null
    source "$ENV_FILE"
    set +a

    if [ -z "${AGENT_ID:-}" ] || [ "$AGENT_ID" = "agent" ]; then
        log_warn "AGENT_ID не установлен или имеет значение по умолчанию (agent)"
        missing=$((missing + 1))
    fi

    if [ -z "${BALANCER_URL:-}" ] || echo "$BALANCER_URL" | grep -q "REPLACE_WITH"; then
        log_warn "BALANCER_URL не установлен или содержит placeholder"
        missing=$((missing + 1))
    fi

    if [ -z "${AGENT_PUBLIC_HOST:-}" ] || echo "$AGENT_PUBLIC_HOST" | grep -q "REPLACE_WITH"; then
        log_warn "AGENT_PUBLIC_HOST не установлен или содержит placeholder"
        missing=$((missing + 1))
    fi

    if [ $missing -gt 0 ]; then
        log_error "Найдены незаполненные обязательные переменные в $ENV_FILE"
        log_info "Отредактируйте файл и запустите скрипт снова."
        exit 1
    fi

    log_info "Переменные окружения валидны."
}

# Запуск Docker Compose
deploy() {
    local mode=$1
    local compose_files=()
    local build_args=()

    compose_files+=("-f" "${COMPOSE_DIR}/docker-compose.agent.yml")

    if [ "$mode" = "gpu" ]; then
        compose_files+=("-f" "${COMPOSE_DIR}/docker-compose.agent.gpu.yml")
        log_info "Режим: GPU (с NVIDIA Container Toolkit)"
        
        if ! check_nvidia_toolkit; then
            log_warn "NVIDIA Container Toolkit не обнаружен!"
            log_info "Для GPU-режима установите toolkit:"
            log_info "  https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html"
            log_info ""
            log_warn "Если toolkit установлен, но не обнаружен, используйте --gpu для принудительного режима."
            read -p "Продолжить anyway? [y/N] " -n 1 -r
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                log_info "Отменено. Установите NVIDIA Container Toolkit и попробуйте снова."
                exit 1
            fi
        fi
    else
        log_info "Режим: CPU"
    fi

    # Формируем команду
    local cmd=("docker" "compose" "${compose_files[@]}" "--env-file" "$ENV_FILE")

    if [ "$PULL_FLAG" = "--pull" ]; then
        log_info "Pull base images..."
        "${cmd[@]}" pull
    fi

    if [ "$BUILD_FLAG" = "--build" ]; then
        build_args+=("--build")
    fi

    log_info "Запуск контейнера..."
    # shellcheck disable=SC2086
    "${cmd[@]}" up -d "${build_args[@]}"

    log_info "Контейнер запущен. Просмотр логов:"
    echo ""
    echo "  ${cmd[*]} logs -f"
    echo ""
}

# --- Main ---

main() {
    # Разбор аргументов
    while [[ $# -gt 0 ]]; do
        case $1 in
            --gpu)
                FORCE_GPU=true
                shift
                ;;
            --cpu)
                FORCE_CPU=true
                shift
                ;;
            --env-file)
                ENV_FILE="$2"
                shift 2
                ;;
            --build)
                BUILD_FLAG="--build"
                shift
                ;;
            --no-build)
                BUILD_FLAG=""
                shift
                ;;
            --pull)
                PULL_FLAG="--pull"
                shift
                ;;
            -h|--help)
                show_help
                ;;
            *)
                log_error "Неизвестный аргумент: $1"
                show_help
                ;;
        esac
    done

    # Показываем help если не передано аргументов
    if [ $# -eq 0 ] && [ "$FORCE_GPU" = false ] && [ "$FORCE_CPU" = false ] && [ "$BUILD_FLAG" = "--build" ] && [ "$PULL_FLAG" = "" ]; then
        : # OK, запуск с дефолтами
    fi

    log_info "Ollama Load Balancer - Agent Deployment"
    log_debug "ENV_FILE: $ENV_FILE"
    log_debug "COMPOSE_DIR: $COMPOSE_DIR"

    check_docker
    check_env_file
    validate_env

    local mode
    mode=$(detect_mode)
    deploy "$mode"

    log_info "Готово! Агент развернут в режиме: $mode"
}

main "$@"