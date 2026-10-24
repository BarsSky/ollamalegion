# donor-autologon-enable.ps1 — включает авто-вход для текущего пользователя.
#
# ПАРОЛЬ НЕ УХОДИТ НИКУДА: он вводится ИНТЕРАКТИВНО на самой машине и пишется
# только в HKLM\...\Winlogon на доноре. Этот файл можно прочитать целиком — в нём
# нет ни пароля, ни сетевых вызовов.
#
# КАКОЙ ПАРОЛЬ НУЖЕН. Если учётка донора — MicrosoftAccount
# (Get-LocalUser: PrincipalSource=MicrosoftAccount), Windows ждёт ПАРОЛЬ
# УЧЁТНОЙ ЗАПИСИ MICROSOFT — тот, который вводится на экране входа, если выбрать
# «Пароль» вместо PIN. PIN (Windows Hello) для авто-входа не подходит
# принципиально: он привязан к TPM и не может быть введён программой.
# SSH-ключ тоже не подходит: он аутентифицирует службу sshd, а не интерактивный
# вход, — сессия для Docker Desktop так не создаётся.
#
# ТРЕБУЕТ ПРАВ АДМИНИСТРАТОРА (учётка уже в группе «Администраторы»).
$ErrorActionPreference = 'Stop'

$key    = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
$user   = $env:USERNAME
$domain = $env:COMPUTERNAME

Write-Host "Машина: $domain, пользователь: $user"
$src = (Get-LocalUser -Name $user).PrincipalSource
Write-Host "Тип учётной записи: $src"
if ("$src" -eq 'MicrosoftAccount') {
    Write-Host 'Это учётная запись Microsoft: нужен ПАРОЛЬ УЧЁТНОЙ ЗАПИСИ MICROSOFT, не PIN.'
} else {
    Write-Host 'Это локальная учётная запись: нужен её локальный пароль.'
}

$sec   = Read-Host -AsSecureString 'Введите пароль (ввод не отображается)'
$bstr  = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
$plain = [Runtime.InteropServices.Marshal]::PtrToStringAuto($bstr)
[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
if ([string]::IsNullOrEmpty($plain)) { Write-Host 'Пароль пустой — отмена.'; exit 1 }

# netplwiz-галочка в Windows 11 скрыта, если DevicePasswordLessBuildVersion=2.
# Ставим 0: это только возвращает возможность авто-входа, вход по PIN работает как раньше.
$plKey = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\PasswordLess\Device'
New-Item -Path $plKey -Force | Out-Null
Set-ItemProperty -Path $plKey -Name DevicePasswordLessBuildVersion -Value 0 -Type DWord

Set-ItemProperty -Path $key -Name AutoAdminLogon     -Value '1'
Set-ItemProperty -Path $key -Name DefaultUserName    -Value $user
Set-ItemProperty -Path $key -Name DefaultDomainName  -Value $domain
Set-ItemProperty -Path $key -Name DefaultPassword    -Value $plain

Write-Host ''
Write-Host 'Готово. Записано:'
Get-ItemProperty -Path $key | Select-Object AutoAdminLogon, DefaultUserName, DefaultDomainName |
    Format-List
Write-Host 'ВНИМАНИЕ: DefaultPassword хранится в реестре ОТКРЫТЫМ ТЕКСТОМ (так работает'
Write-Host 'сам механизм Windows). Если пароль учётной записи Microsoft сменится, авто-вход'
Write-Host 'сломается и на экране входа появится «неверный пароль» — тогда запустите этот'
Write-Host 'файл снова с новым паролем.'
Write-Host ''
Write-Host 'Проверка: перезагрузить и посмотреть, залогинен ли пользователь без ввода пароля.'
