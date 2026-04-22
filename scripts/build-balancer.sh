#!/bin/bash
#
# Скрипт сборки балансировщика Ollama Load Balancer
# 
# Использование:
#   ./build-balancer.sh [output_dir]
#
# Аргументы:
#   output_dir - директория для бинарного файла (по умолчанию: ./bin)
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="${1:-$PROJECT_ROOT/bin}"
BALANCER_BINARY="$OUTPUT_DIR/balancer"

echo "╔═══════════════════════════════════════════════════════════╗"
echo "║      Ollama Load Balancer - Balancer Builder              ║"
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

# Сборка балансировщика
echo "[3/4] Building balancer..."

if [ "$TARGET_OS" = "windows" ]; then
    BALANCER_BINARY="$OUTPUT_DIR/balancer.exe"
fi

CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build -a -installsuffix cgo -ldflags="-s -w" -o "$BALANCER_BINARY" ./cmd/balancer

# Проверка результата
if [ -f "$BALANCER_BINARY" ]; then
    FILE_SIZE=$(du -h "$BALANCER_BINARY" | cut -f1)
    echo "[4/4] Build successful!"
    echo ""
    echo "       Binary: $BALANCER_BINARY"
    echo "       Size:   $FILE_SIZE"
    echo ""
    
    # Вывод информации о бинарном файле
    if command -v file &> /dev/null; then
        echo "       Info:   $(file "$BALANCER_BINARY")"
    fi
else
    echo "ERROR: Build failed - binary not found"
    exit 1
fi

echo ""
echo "Usage example:"
echo "  $BALANCER_BINARY -config config/config.json"
echo ""
