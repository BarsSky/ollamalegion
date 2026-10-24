' legion-keepalive.vbs — запускает legion-keepalive.ps1 БЕЗ ОКНА КОНСОЛИ.
'
' ЗАЧЕМ ОТДЕЛЬНЫЙ VBS (R168, 2026-10-17). Задача Планировщика, запускающая .cmd или
' powershell.exe, всё равно создаёт консоль на мгновение — в сессии пользователя это
' выглядит как «постоянно мелькает чёрное окно» и мешает работе (живая жалоба).
' wscript.exe с Run(..., 0, True) запускает процесс со скрытым окном и без консоли.
'
' ПО УМОЛЧАНИЮ НЕ СТАВИТСЯ. Донор включает оператор, а rpc-server поднимается по
' приказу балансера (R133/R164) — периодический автоподъём не нужен. Скрипт лежит
' на случай, если понадобится именно автоподъём: тогда задачу ставить ЧЕРЕЗ ЭТОТ
' файл (см. donor-install-keepalive.cmd), а не через powershell.exe напрямую.
Option Explicit
Dim sh, fso, here, ps1
Set sh = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")
here = fso.GetParentFolderName(WScript.ScriptFullName)
ps1 = here & "\legion-keepalive.ps1"
If Not fso.FileExists(ps1) Then
    WScript.Quit 2
End If
' 0 = скрытое окно, True = ждать завершения (Планировщик видит результат)
sh.Run "powershell.exe -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File """ & ps1 & """", 0, True
