#!/bin/sh
# Diagnostics with merged output and unbuffered
cd /app
exec 2>&1
echo "===PRE-EXEC==="
date
./cppworker 1>/tmp/cw4.log 2>&1
CODE=$?
echo "===POST-EXEC CODE=$CODE==="
echo "===LOG CONTENT==="
cat /tmp/cw4.log
echo "===END==="