@echo off
rem donor-autologon-enable.cmd - wrapper: run the autologon setup elevated.
rem Right-click -> "Run as administrator" on the donor desktop, or run from an
rem elevated prompt. The password is typed INTO the prompt on that machine only.
net session >nul 2>&1
if errorlevel 1 (
  echo Not elevated. Right-click this file and choose "Run as administrator".
  pause
  exit /b 1
)
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0donor-autologon-enable.ps1"
pause
