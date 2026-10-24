@echo off
rem donor-install-keepalive.cmd - OPTIONAL. Registers a SILENT keepalive task.
rem
rem DEFAULT IS OFF (R168, 2026-10-17). The donor is started by a human, and the
rem rpc-server is started by the balancer command (R133/R164), so no periodic
rem auto-start is needed. Earlier this task ran every 3 minutes through cmd.exe
rem and flashed a console window in the user session - that is why it was removed.
rem
rem Install ONLY if you really want auto-recovery of the container. The task is
rem registered through wscript.exe + legion-keepalive.vbs: no console, no flash.
setlocal
net session >nul 2>&1
if errorlevel 1 (echo Not elevated. Right-click -^> Run as administrator. & exit /b 1)

echo --- files ---
dir /b "%USERPROFILE%\legion-keepalive.ps1" "%USERPROFILE%\legion-keepalive.vbs"

echo --- delete previous task if any ---
schtasks /delete /tn "LegionStackDockerKeepalive" /f 2>nul

echo --- create SILENT task: every 5 minutes, through wscript ---
schtasks /create /tn "LegionStackDockerKeepalive" /tr "wscript.exe //B \"%USERPROFILE%\legion-keepalive.vbs\"" /sc MINUTE /mo 5 /f
if errorlevel 1 goto fail

echo --- verify ---
schtasks /query /tn "LegionStackDockerKeepalive" /fo LIST | findstr /i "TaskName Status"
echo --- run once now (must NOT show any window) ---
schtasks /run /tn "LegionStackDockerKeepalive"
if errorlevel 1 goto fail
echo OK
exit /b 0

:fail
echo FAIL: task not created or not started
exit /b 1
