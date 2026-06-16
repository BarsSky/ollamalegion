#!/bin/sh
echo "=== ulimit -a ==="
ulimit -a 2>&1
echo "=== uname -a ==="
uname -a 2>&1
echo "=== nvidia-smi ==="
nvidia-smi 2>&1 | head -5
echo "=== ls /usr/local/cuda ==="
ls /usr/local/cuda 2>&1 | head -5
echo "=== strace -f -e openat -o /tmp/strace.log ./cppworker 2>&1 | head -30 ==="
timeout 5 strace -f -e openat -o /tmp/strace.log ./cppworker 2>&1 | head -30
echo "=== EXIT=$? ==="
echo "=== strace head ==="
head -50 /tmp/strace.log 2>&1