#!/bin/sh
# Запуск cppworker в фоне с сохранением логов и PID для отслеживания
cd /app
echo "=== START $(date) ===" > /tmp/cw_run.log
./cppworker >> /tmp/cw_run.log 2>&1 &
PID=$!
echo "PID=$PID" >> /tmp/cw_run.log
echo "=== sleep 10s waiting ==="
sleep 10
if kill -0 $PID 2>/dev/null; then
    echo "=== STILL ALIVE at +10s ===" >> /tmp/cw_run.log
    curl -sS -m 3 http://127.0.0.1:18092/health >> /tmp/cw_run.log 2>&1
    echo "=== HEALTH EXIT=$? ===" >> /tmp/cw_run.log
    kill $PID 2>/dev/null
else
    echo "=== DEAD before +10s ===" >> /tmp/cw_run.log
fi
echo "=== END $(date) ===" >> /tmp/cw_run.log
cat /tmp/cw_run.log