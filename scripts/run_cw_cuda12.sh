#!/bin/sh
# Force CUDA 12.2 lib path FIRST to avoid WSL libcuda conflict
export LD_LIBRARY_PATH=/usr/local/cuda/targets/x86_64-linux/lib:/usr/local/nvidia/lib:/usr/local/nvidia/lib64
echo "LD_LIBRARY_PATH=$LD_LIBRARY_PATH"
echo "=== ldd ==="
ldd /app/cppworker | head -15
echo "=== run ==="
timeout 8 ./cppworker > /tmp/cw6.log 2>&1
CODE=$?
echo "EXIT=$CODE"
echo "=== log ==="
cat /tmp/cw6.log