@echo off
rem donor-remove-keepalive.cmd - remove the periodic task from the donor.
rem ASCII only: Cyrillic in .cmd breaks on the console codepage (proven twice).
rem Why remove: LegionStackDockerKeepalive fired through cmd.exe and flashed a
rem console window in the user session. Operator decision (2026-10-17): the donor
rem is started by a human, and rpc-server is started by the balancer (R133/R164).
echo --- before ---
schtasks /query /tn "LegionStackDockerKeepalive" /fo LIST 2>nul | findstr /i "TaskName Status"
echo --- deleting ---
schtasks /delete /tn "LegionStackDockerKeepalive" /f
echo --- after ---
schtasks /query /tn "LegionStackDockerKeepalive" 2>nul
if not errorlevel 1 (echo STILL PRESENT & exit /b 1)
echo removed
echo --- wrapper files (the .cmd one is no longer used: silent path is .vbs) ---
del /q "%USERPROFILE%\legion-keepalive.cmd" 2>nul
echo   wrapper deleted; ps1/vbs kept (documented, install through the .vbs if ever needed)
echo --- container must not depend on the task ---
docker ps --format "{{.Names}}={{.Status}}"
echo ---END---
