#!/bin/bash
#
# Ollama Load Balancer — unified build script
#
# Usage:
#   ./build.sh agent [output_dir]
#   ./build.sh balancer [output_dir]
#
set -e

COMPONENT="${1:?Usage: $0 <agent|balancer> [output_dir]}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="${2:-$PROJECT_ROOT/bin}"
BINARY="$OUTPUT_DIR/$COMPONENT"
CMD_PATH="./cmd/$COMPONENT"

case "$COMPONENT" in
  agent)     LABEL="Agent"   ; EXAMPLE="$BINARY -id gpu-1 -balancer http://localhost:8081" ;;
  balancer)  LABEL="Balancer"; EXAMPLE="$BINARY -config config/config.json"                ;;
  *) echo "ERROR: unknown component '$COMPONENT'. Use 'agent' or 'balancer'."; exit 1 ;;
esac

echo "╔═══════════════════════════════════════════════════════════╗"
echo "║         Ollama Load Balancer - $LABEL Builder              ║"
echo "╚═══════════════════════════════════════════════════════════╝"

mkdir -p "$OUTPUT_DIR"
cd "$PROJECT_ROOT"

# [1/4] Go check
echo "[1/4] Checking Go installation..."
GO_VERSION=$(go version 2>&1) || { echo "ERROR: Go is not installed or not in PATH"; exit 1; }
echo "       Found: $GO_VERSION"

# [2/4] Platform
TARGET_OS="${TARGET_OS:-$(go env GOOS)}"
TARGET_ARCH="${TARGET_ARCH:-$(go env GOARCH)}"
echo "[2/4] Target platform: $TARGET_OS/$TARGET_ARCH"

# [3/4] Build
echo "[3/4] Building $COMPONENT..."
if [ "$TARGET_OS" = "windows" ]; then
  BINARY="$BINARY.exe"
fi
CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" \
  go build -a -installsuffix cgo -ldflags="-s -w" -o "$BINARY" "$CMD_PATH"

# [4/4] Result
if [ -f "$BINARY" ]; then
  FILE_SIZE=$(du -h "$BINARY" | cut -f1)
  echo "[4/4] Build successful!"
  echo ""
  echo "       Binary: $BINARY"
  echo "       Size:   $FILE_SIZE"
  echo ""
  command -v file &>/dev/null && echo "       Info:   $(file "$BINARY")"
else
  echo "ERROR: Build failed - binary not found"
  exit 1
fi

echo ""
echo "Usage example:"
echo "  $EXAMPLE"
echo ""