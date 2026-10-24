@echo off
rem donor-verify-quiet.cmd - confirm no flashing console source is left on the donor.
echo --- our tasks (must be empty) ---
schtasks /query /tn "LegionStackDockerKeepalive" 2>nul
if not errorlevel 1 (echo STILL PRESENT) else (echo none)
echo --- our Run entries (must be empty) ---
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" 2>nul | findstr /i "legion keepalive"
echo --- wrapper files ---
if exist "%USERPROFILE%\legion-keepalive.cmd" (echo cmd-wrapper present) else (echo cmd-wrapper absent)
if exist "%USERPROFILE%\legion-keepalive.vbs" (echo vbs-hidden present, not scheduled) else (echo vbs absent)
echo --- container ---
docker ps --format "{{.Names}}={{.Status}}"
echo ---END---
