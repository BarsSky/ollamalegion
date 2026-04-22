#!/bin/bash
#
# Скрипт сборки агента Ollama Load Balancer
# 
# Использование:
#   ./build-agent.sh [output_dir]
#
# Аргументы:
#   output_dir - директория для бинарного файла (по умолчанию: ./bin)
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="${1:-$PROJECT_ROOT/bin}"
AGENT_BINARY="$OUTPUT_DIR/agent"

echo "╔═══════════════════════════════════════════════════════════╗"
echo "║         Ollama Load Balancer - Agent Builder              ║"
echo "╚═══════════════════════════════════════════════════════════╝"

# Создание директории вывода
mkdir -p "$OUTPUT_DIR"

# Переход в корень проекта
cd "$PROJECT_ROOT"

# Получение версии Go
echo "[1/4] Checking Go installation..."
GO_VERSION=$(go version 2>&1) || {
    echo "ERROR: Go is not installed or not in PATH"
    exit 1
}
echo "       Found: $GO_VERSION"

# Определение целевой ОС и архитектуры
TARGET_OS="${TARGET_OS:-$(go env GOOS)}"
TARGET_ARCH="${TARGET_ARCH:-$(go env GOARCH)}"

echo "[2/4] Target platform: $TARGET_OS/$TARGET_ARCH"

# Сборка агента
echo "[3/4] Building agent..."

if [ "$TARGET_OS" = "windows" ]; then
    AGENT_BINARY="$OUTPUT_DIR/agent.exe"
fi

CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build -a -installsuffix cgo -ldflags="-s -w" -o "$AGENT_BINARY" ./cmd/agent

# Проверка результата
if [ -f "$AGENT_BINARY" ]; then
    FILE_SIZE=$(du -h "$AGENT_BINARY" | cut -f1)
    echo "[4/4] Build successful!"
    echo ""
    echo "       Binary: $AGENT_BINARY"
    echo "       Size:   $FILE_SIZE"
    echo ""
    
    # Вывод информации о бинарном файле
    if command -v file &> /dev/null; then
        echo "       Info:   $(file "$AGENT_BINARY")"
    fi
else
    echo "ERROR: Build failed - binary not found"
    exit 1
fi

echo ""
echo "Usage example:"
echo "  $AGENT_BINARY -id gpu-1 -balancer http://localhost:8081"
echo ""
