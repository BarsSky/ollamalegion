#!/bin/bash
#
# Скрипт быстрого развертывания агента Ollama Load Balancer
#
# Использование:
#   ./deploy-agent.sh <AGENT_ID> <BALANCER_URL> [OPTIONS]
#
# Аргументы:
#   AGENT_ID      - Уникальный идентификатор агента (обязательно)
#   BALANCER_URL  - URL балансировщика (обязательно)
#   OPTIONS       - Дополнительные опции
#
# Примеры:
#   ./deploy-agent.sh gpu-1 http://192.168.1.100:8081
#   ./deploy-agent.sh gpu-1 http://192.168.1.100:8081 --port 9090 --interval 5
#

set -e

# Цвета для вывода
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Параметры по умолчанию
AGENT_ID="${1:-}"
BALANCER_URL="${2:-}"
METRICS_PORT="${3:-18032}"
COLLECT_INTERVAL="${4:-5}"
HEARTBEAT_INTERVAL="${5:-3}"
IMAGE_NAME="ollama-legion/agent:latest"
CONTAINER_NAME="ollama-agent"

# Функция вывода
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Вывод заголовка
print_header() {
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║     Ollama Load Balancer - Agent Deployment Script        ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
}

# Проверка аргументов
check_args() {
    if [ -z "$AGENT_ID" ]; then
        log_error "AGENT_ID is required"
        echo "Usage: $0 <AGENT_ID> <BALANCER_URL> [METRICS_PORT] [COLLECT_INTERVAL] [HEARTBEAT_INTERVAL]"
        exit 1
    fi

    if [ -z "$BALANCER_URL" ]; then
        log_error "BALANCER_URL is required"
        echo "Usage: $0 <AGENT_ID> <BALANCER_URL> [METRICS_PORT] [COLLECT_INTERVAL] [HEARTBEAT_INTERVAL]"
        exit 1
    fi

    log_info "Agent ID:      $AGENT_ID"
    log_info "Balancer URL:  $BALANCER_URL"
    log_info "Metrics Port:  $METRICS_PORT"
    log_info "Collect:       ${COLLECT_INTERVAL}s"
    log_info "Heartbeat:     ${HEARTBEAT_INTERVAL}s"
    echo ""
}

# Проверка Docker
check_docker() {
    log_info "Checking Docker installation..."
    
    if ! command -v docker &> /dev/null; then
        log_error "Docker is not installed"
        echo ""
        echo "Please install Docker:"
        echo "  curl -fsSL https://get.docker.com | sh"
        exit 1
    fi

    DOCKER_VERSION=$(docker --version)
    log_success "Docker found: $DOCKER_VERSION"
}

# Проверка NVIDIA Container Toolkit
check_nvidia() {
    log_info "Checking NVIDIA Container Toolkit..."
    
    # Проверка nvidia-smi на хосте
    if ! command -v nvidia-smi &> /dev/null; then
        log_warn "nvidia-smi not found on host - GPU metrics may not be available"
        log_warn "Install NVIDIA drivers if GPU monitoring is required"
        return 1
    fi

    # Проверка доступа к GPU через Docker
    if ! docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi &> /dev/null; then
        log_warn "NVIDIA Container Toolkit may not be configured correctly"
        log_warn "GPU metrics may not be available"
        echo ""
        echo "To install NVIDIA Container Toolkit:"
        echo "  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg"
        echo "  distribution=$(. /etc/os-release;echo $ID$VERSION_ID)"
        echo "  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \\"
        echo "    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \\"
        echo "    sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list"
        echo "  sudo apt-get update"
        echo "  sudo apt-get install -y nvidia-container-toolkit"
        echo "  sudo systemctl restart docker"
        echo ""
        return 1
    fi

    log_success "NVIDIA Container Toolkit is configured"
    return 0
}

# Проверка доступности балансировщика
check_balancer() {
    log_info "Checking balancer availability..."
    
    # Извлечение хоста из URL
    BALANCER_HOST=$(echo "$BALANCER_URL" | sed -E 's|https?://||' | cut -d':' -f1)
    BALANCER_PORT=$(echo "$BALANCER_URL" | sed -E 's|https?://[^:]+:||' | cut -d'/' -f1)
    
    if command -v curl &> /dev/null; then
        if curl -s --connect-timeout 5 "${BALANCER_URL}/api/v1/health" &> /dev/null; then
            log_success "Balancer is accessible"
        else
            log_warn "Balancer may not be accessible at ${BALANCER_URL}"
            log_warn "Continuing anyway - agent will retry on startup"
        fi
    else
        log_warn "curl not found - skipping balancer check"
    fi
}

# Загрузка или сборка образа
prepare_image() {
    log_info "Preparing agent image..."
    
    # Проверка локального образа
    if docker image inspect "$IMAGE_NAME" &> /dev/null; then
        log_success "Local image found: $IMAGE_NAME"
        
        # Попытка обновления
        log_info "Checking for updates..."
        if docker pull "$IMAGE_NAME" &> /dev/null; then
            log_success "Image updated"
        else
            log_info "Using local image"
        fi
    else
        log_info "Image not found locally. Attempting to pull..."
        
        if docker pull "$IMAGE_NAME" &> /dev/null; then
            log_success "Image pulled successfully"
        else
            log_warn "Failed to pull image. Attempting to build locally..."
            
            # Поиск директории проекта
            SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
            PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
            
            if [ -f "$PROJECT_ROOT/docker/agent/Dockerfile" ]; then
                log_info "Building from $PROJECT_ROOT..."
                docker build -f "$PROJECT_ROOT/docker/agent/Dockerfile" -t "$IMAGE_NAME" "$PROJECT_ROOT"
                log_success "Image built successfully"
            else
                log_error "Dockerfile not found"
                exit 1
            fi
        fi
    fi
}

# Остановка существующего контейнера
stop_existing() {
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        log_info "Stopping existing container..."
        docker stop "$CONTAINER_NAME" &> /dev/null || true
        docker rm "$CONTAINER_NAME" &> /dev/null || true
        log_success "Existing container removed"
    fi
}

# Запуск контейнера
start_container() {
    log_info "Starting agent container..."
    
    # Формирование команды docker run
    DOCKER_CMD="docker run -d"
    DOCKER_CMD="$DOCKER_CMD --name $CONTAINER_NAME"
    DOCKER_CMD="$DOCKER_CMD --restart unless-stopped"
    
    # Переменные окружения
    DOCKER_CMD="$DOCKER_CMD -e AGENT_ID=$AGENT_ID"
    DOCKER_CMD="$DOCKER_CMD -e BALANCER_URL=$BALANCER_URL"
    DOCKER_CMD="$DOCKER_CMD -e METRICS_PORT=$METRICS_PORT"
    DOCKER_CMD="$DOCKER_CMD -e COLLECT_INTERVAL=$COLLECT_INTERVAL"
    DOCKER_CMD="$DOCKER_CMD -e HEARTBEAT_INTERVAL=$HEARTBEAT_INTERVAL"
    
    # Тома для доступа к системе
    DOCKER_CMD="$DOCKER_CMD -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro"
    DOCKER_CMD="$DOCKER_CMD -v /var/run/nvidia-top-level-device:/var/run/nvidia-top-level-device:ro"
    DOCKER_CMD="$DOCKER_CMD -v /proc:/host/proc:ro"
    DOCKER_CMD="$DOCKER_CMD -v /sys:/host/sys:ro"
    
    # Сеть и GPU
    DOCKER_CMD="$DOCKER_CMD --network host"
    DOCKER_CMD="$DOCKER_CMD --gpus all"
    
    # Образ
    DOCKER_CMD="$DOCKER_CMD $IMAGE_NAME"
    
    # Выполнение
    eval $DOCKER_CMD
    
    if [ $? -eq 0 ]; then
        log_success "Container started successfully"
    else
        log_error "Failed to start container"
        exit 1
    fi
}

# Проверка запуска
verify_startup() {
    log_info "Waiting for agent to start..."
    sleep 3
    
    # Проверка статуса контейнера
    if docker ps | grep -q "$CONTAINER_NAME"; then
        log_success "Agent is running"
        
        # Проверка логов
        echo ""
        log_info "Recent logs:"
        docker logs --tail 10 "$CONTAINER_NAME" 2>/dev/null || true
        
        # Проверка метрик
        echo ""
        log_info "Checking metrics endpoint..."
        if command -v curl &> /dev/null; then
            if curl -s --connect-timeout 2 "http://localhost:$METRICS_PORT/metrics" &> /dev/null; then
                log_success "Metrics endpoint is accessible"
            else
                log_warn "Metrics endpoint not yet available - agent may still be starting"
            fi
        fi
    else
        log_error "Agent failed to start"
        echo ""
        log_info "Full logs:"
        docker logs "$CONTAINER_NAME" 2>&1 || true
        exit 1
    fi
}

# Вывод итоговой информации
print_summary() {
    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║              Deployment Complete                          ║"
    echo "╠═══════════════════════════════════════════════════════════╣"
    printf "║ Agent ID:     %-46s║\n" "$AGENT_ID"
    printf "║ Balancer:     %-46s║\n" "$BALANCER_URL"
    printf "║ Container:    %-46s║\n" "$CONTAINER_NAME"
      printf "║ Agent Port:   %-46s║\n" "$METRICS_PORT"
    echo "╠═══════════════════════════════════════════════════════════╣"
    echo "║ Useful commands:                                          ║"
    echo "║   Check status:  docker ps | grep $CONTAINER_NAME"
    echo "║   View logs:     docker logs -f $CONTAINER_NAME"
    echo "║   Stop agent:    docker stop $CONTAINER_NAME"
    echo "║   Remove agent:  docker rm -f $CONTAINER_NAME"
      echo "║   Metrics:       curl http://localhost:$METRICS_PORT/metrics"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
}

# Основная функция
main() {
    print_header
    check_args
    check_docker
    check_nvidia || true  # Продолжаем даже если NVIDIA не настроена
    check_balancer
    prepare_image
    stop_existing
    start_container
    verify_startup
    print_summary
}

# Запуск
main
