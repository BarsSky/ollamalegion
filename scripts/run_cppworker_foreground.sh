#!/bin/sh
# Foreground run для диагностики cppworker
ls -la /app/cppworker
echo "===RUN==="
./cppworker --port 18092 --models-dir ./models --gpu-layers 30 --ctx-size 4096 --batch-size 512
echo "===EXIT=$?==="