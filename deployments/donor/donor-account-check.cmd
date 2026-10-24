@echo off
rem donor-account-check.cmd - what kind of credential autologon would need.
echo ---WHOAMI---
whoami
echo ---LOCAL-ACCOUNTS---
powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-LocalUser | Select-Object Name,PrincipalSource,Enabled,PasswordRequired,PasswordLastSet | Format-Table -AutoSize | Out-String -Width 160"
echo ---NET-USER---
net user %USERNAME% | findstr /C:"User name" /C:"Account active" /C:"Password required" /C:"Password last set" /C:"Local Group"
echo ---WINLOGON---
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" /v AutoAdminLogon
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" /v DefaultUserName
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" /v DefaultDomainName
echo ---ADMIN-RIGHTS---
net session >nul 2>&1 && echo ADMIN=yes || echo ADMIN=no
echo ---WSL---
wsl -l -v
echo ---DOCKER-INSTALLS---
if exist "C:\Program Files\Docker\Docker\Docker Desktop.exe" (echo MACHINE-WIDE-INSTALL=yes) else (echo MACHINE-WIDE-INSTALL=no)
if exist "%LOCALAPPDATA%\Programs\DockerDesktop\Docker Desktop.exe" (echo PER-USER-INSTALL=yes) else (echo PER-USER-INSTALL=no)
echo ---END---
