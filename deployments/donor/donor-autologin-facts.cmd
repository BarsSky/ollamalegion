@echo off
rem donor-autologin-facts.cmd - what blocks autologon on this machine.
echo ---PASSWORDLESS-BUILD---
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\PasswordLess\Device" /v DevicePasswordLessBuildVersion
echo ---HELLO-PIN---
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\WinBio\Credential Provider" 2>nul
dir /b "C:\Windows\ServiceProfiles\LocalService\AppData\Local\Microsoft\Ngc" 2>nul
echo ---SIGNIN-REQUIREMENTS---
reg query "HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System" /v DisableCAD
reg query "HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System" /v EnforceLockoutPolicy
reg query "HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System" /v DontDisplayLockedUserId
echo ---FASTSTARTUP---
reg query "HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Power" /v HiberbootEnabled
echo ---END---
