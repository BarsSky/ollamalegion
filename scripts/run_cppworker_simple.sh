#!/bin/sh
# Foreground run for cppworker diagnostics
cd /app
./cppworker > /tmp/cw3.log 2>&1
CODE=$?
echo "---CODE=$CODE---"
cat /tmp/cw3.log
echo "---END---"