# scripts/diagnose-webui-auth.ps1 — R229 (2026-10-23).
#
# ЗАЧЕМ. Живая жалоба: «задал пользователя и пароль в env, перезапустил, но WebUI
# не даёт войти и не создаёт в папке config user.json». У этой проблемы ровно
# несколько типовых причин, и они различимы по ФАКТАМ, а не по догадкам:
#   1. env задан не тому контейнеру (пользователи живут в БАЛАНСЕРЕ, не в webui);
#   2. контейнер перезапущен, а не пересоздан (restart не перечитывает .env);
#   3. запущен только webui — /api/v1/auth/* обслуживать некому;
#   4. старый образ балансера (до R201) — обработчиков входа нет;
#   5. users.json пишется в ефемерный путь (LB_USERS_FILE не на томе).
#
# Скрипт только ЧИТАЕТ (никаких stop/up/env-изменений) и печатает вердикт.
# Запуск:
#   pwsh -File scripts/diagnose-webui-auth.ps1
#   pwsh -File scripts/diagnose-webui-auth.ps1 -BalancerContainer ol-stack-balancer `
#        -BalancerUrl http://127.0.0.1:18080 -WebUIUrl http://127.0.0.1:18083
[CmdletBinding()]
param(
    [string]$BalancerContainer = 'ol-stack-balancer',
    [string]$WebUIContainer = 'ol-stack-webui',
    [string]$BalancerUrl = 'http://127.0.0.1:18080',
    [string]$WebUIUrl = 'http://127.0.0.1:18083'
)

$ErrorActionPreference = 'Continue'

function Say([string]$Text, [string]$Tone = 'Info') {
    $color = switch ($Tone) { 'Ok' { 'Green' } 'Bad' { 'Red' } 'Warn' { 'Yellow' } default { 'Gray' } }
    Write-Host $Text -ForegroundColor $color
}
function Docker([string]$ArgsText) { return (& cmd /c "docker $ArgsText" 2>&1 | Out-String) }
function TryJson([string]$Url) {
    try { return ((Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 10).Content | ConvertFrom-Json) }
    catch { return $null }
}

$fails = 0
function Verdict([bool]$Ok, [string]$What, [string]$Hint) {
    if ($Ok) { Say ('  OK    ' + $What) 'Ok' } else { Say ('  ПРОБЛЕМА  ' + $What + $(if ($Hint) { ' — ' + $Hint } else { '' })) 'Bad'; $script:fails++ }
}

Say "=== Диагностика входа WebUI: $(Get-Date -Format 'yyyy-MM-dd HH:mm') ===" 'Info'

# 0. Контейнеры
Say "`n0. Контейнеры" 'Info'
$ps = Docker 'ps --format "{{.Names}}|{{.Image}}|{{.Status}}"'
Say $ps.Trim() 'Info'
$balancerRunning = $ps -match [regex]::Escape($BalancerContainer)
$webuiRunning = $ps -match [regex]::Escape($WebUIContainer)
Verdict $balancerRunning "контейнер балансера ($BalancerContainer) запущен" "без балансера /api/v1/auth/* обслуживать некому"
Verdict $webuiRunning "контейнер webui ($WebUIContainer) запущен" 'проверьте имя контейнера (-WebUIContainer)'

# 1. env именно балансера (не webui!)
Say "`n1. Переменные балансера (LB_ADMIN_*, LB_USERS_FILE, LB_SESSIONS_FILE)" 'Info'
$envRaw = ''
$usersFile = ''
$sessionsFile = ''
$adminUser = ''
$adminPassSet = $false
if ($balancerRunning) {
    $envRaw = Docker "inspect $BalancerContainer --format `"{{range .Config.Env}}{{println .}}{{end}}`""
    foreach ($line in ($envRaw -split "`n")) {
        $l = $line.Trim()
        if ($l -match '^LB_USERS_FILE=(.*)$') { $usersFile = $Matches[1] }
        elseif ($l -match '^LB_SESSIONS_FILE=(.*)$') { $sessionsFile = $Matches[1] }
        elseif ($l -match '^LB_ADMIN_USER=(.*)$') { $adminUser = $Matches[1] }
        elseif ($l -match '^LB_ADMIN_PASSWORD=(.*)$') { $adminPassSet = -not [string]::IsNullOrWhiteSpace($Matches[1]) }
    }
    Say ("  LB_ADMIN_USER=" + $(if ($adminUser) { $adminUser } else { '(не задан)' })) 'Info'
    Say ("  LB_ADMIN_PASSWORD=" + $(if ($adminPassSet) { '(задан)' } else { '(НЕ задан или пуст)' })) $(if ($adminPassSet) { 'Ok' } else { 'Warn' })
    Say ("  LB_USERS_FILE=" + $(if ($usersFile) { $usersFile } else { '(не задан → внутри контейнера config/users.json)' })) 'Info'
    Say ("  LB_SESSIONS_FILE=" + $(if ($sessionsFile) { $sessionsFile } else { '(не задан → рядом с users.json)' })) 'Info'
}
Verdict ($adminUser -and $adminPassSet) 'на балансере задана пара LB_ADMIN_USER/LB_ADMIN_PASSWORD' 'пароль пустой игнорируется: пользователя не создадут'

# image tag (старый образ = нет обработчиков входа)
$img = Docker "inspect $BalancerContainer --format `"{{.Config.Image}}`""
Say ("  образ балансера: " + $img.Trim()) 'Info'

# 2. Файл пользователей внутри контейнера
Say "`n2. Где реально лежит users.json (внутри контейнера)" 'Info'
if ($balancerRunning) {
    $where = if ($usersFile) { $usersFile } else { '/app/config/users.json' }
    $ls = Docker "exec $BalancerContainer sh -c `"ls -l $where /app/data /app/config 2>&1`""
    Say $ls.Trim() 'Info'
    $exists = ($ls -match 'users\.json')
    Verdict $exists "файл users.json существует ($where)" "файл не создан: проверьте путь/том; без тома он пропадёт при пересоздании"
    Say "  ВАЖНО: в compose-стеке путь по умолчанию — /app/data/users.json на томе balancer_data." 'Warn'
    Say "  В каталоге репозитория config/users.json не появится: это внутренний путь контейнера." 'Warn'
}

# 3. Что отвечает сам балансер
Say "`n3. GET /api/v1/auth/me" 'Info'
$meBalancer = TryJson ($BalancerUrl.TrimEnd('/') + '/api/v1/auth/me')
$meWebui = TryJson ($WebUIUrl.TrimEnd('/') + '/api/v1/auth/me')
foreach ($pair in @(@('напрямую (балансер)', $meBalancer), @('через webui', $meWebui))) {
    $name = $pair[0]; $me = $pair[1]
    if ($null -eq $me) {
        Say ("  $name : НЕТ ОТВЕТА/не JSON — вход обслуживать некому или старый образ") 'Bad'
    } else {
        Say ("  $name : usersConfigured=" + $me.usersConfigured + " authenticated=" + $me.authenticated) $(if ($me.usersConfigured) { 'Ok' } else { 'Bad' })
    }
}
$usersConfigured = ($meBalancer -and $meBalancer.usersConfigured) -or ($meWebui -and $meWebui.usersConfigured)
Verdict $usersConfigured 'пользователи сконфигурированы (usersConfigured=true)' 'usersConfigured=false: env не применился (restart вместо пересоздания / не тот контейнер) или старый образ'

# 4. Логи балансера про auth
Say "`n4. Логи балансера (auth)" 'Info'
if ($balancerRunning) {
    $log = Docker "logs $BalancerContainer --since 2h"
    $authLines = ($log -split "`n") | Where-Object { $_ -match 'auth:|users_file|users\.json|LB_ADMIN' } | Select-Object -Last 12
    if ($authLines) { $authLines | ForEach-Object { Say ('  ' + $_.Trim()) 'Info' } }
    else { Say '  (строк про auth нет: балансер мог не дойти до инициализации пользователей)' 'Warn' }
    if ($log -match 'администратор создан в ПАМЯТИ') {
        Say '  НАЙДЕНО: администратор создан только в памяти — файл не записан. Задайте LB_USERS_FILE на томе.' 'Bad'
    }
}

# 5. Вердикт
Say "`n=== ВЕРДИКТ ===" 'Info'
if ($fails -eq 0) {
    Say 'Вход настроен: пользователи есть, файл на месте, /api/v1/auth/me отвечает usersConfigured=true.' 'Ok'
    Say 'Если браузер всё равно не пускает — очистите куки этого хоста (старая сессия/аварийный токен в sessionStorage) и попробуйте снова.' 'Info'
    exit 0
}
Say ("Проблем: $fails. Разбор по порядку:") 'Bad'
Say '  1) пользователи живут в БАЛАНСЕРЕ (LB_ADMIN_USER/LB_ADMIN_PASSWORD), НЕ в webui;' 'Info'
Say '  2) после правки .env нужен `docker compose up -d --force-recreate <балансер>` (restart env НЕ перечитывает);' 'Info'
Say '  3) users.json в compose-стеке лежит в томе: /app/data/users.json (docker exec + ls), а не в каталоге репозитория config/;' 'Info'
Say '  4) старый образ балансера (до R201) обработчиков /api/v1/auth/* не имеет;' 'Info'
Say '  5) если в логе «администратор создан в ПАМЯТИ, но файл users.json не записан» — LB_USERS_FILE должен быть на томе.' 'Info'
Say '  6) если /auth/me НАПРЯМУЮ (18080/18081) отвечает 200, а ЧЕРЕЗ webui — 503: образ webui старше R229 — nginx держал старый IP балансера; обновите webui (r229m+).' 'Info'
exit 1
