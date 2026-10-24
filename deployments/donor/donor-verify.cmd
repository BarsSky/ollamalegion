@echo off
echo ---CONTAINERS---
docker ps --format "{{.Names}}={{.Status}}"
echo ---KEEPALIVE-LOG---
type "%USERPROFILE%\legion-keepalive.log" 2>nul
echo ---TASK---
schtasks /query /tn "LegionStackDockerKeepalive" /fo LIST | findstr /i "TaskName Status Next"
echo ---WORKER-INFO---
curl -s -m 10 http://127.0.0.1:18092/api/info
echo.
echo ---END---
