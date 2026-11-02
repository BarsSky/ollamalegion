# audit-webui-settings.ps1 - полный обход настроек WebUI через ТЕ ЖЕ HTTP-вызовы,
# которые делает интерфейс, с проверкой чтения-обратно и эффекта на стенде.
#
# ЗАЧЕМ. Оператор просил проверить «полностью работу настроек WebUI»: кнопки
# могут отвечать успехом, а значение не доезжать до бэкенда (так было с
# «применить и перезагрузить» до R398, с окном профиля до R397, с mmap до R396).
# Скрипт на каждую настройку делает: сохранить -> прочитать обратно -> сравнить ->
# вернуть исходное значение. Всё, что не совпало, печатается как FAIL с числами.
#
# ЧТО НЕ ДЕЛАЕТСЯ (осознанно, печатается как SKIP):
#   - config import (перезапись конфигурации стенда);
#   - image profiles apply (нужен загруженный diffusion-бандл);
#   - termininate сессий и удаление пользователей (разрушающие операции).
#
# Использование:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/audit-webui-settings.ps1 `
#     -Token <CPPWORKER_API_TOKEN> -Model gemma-4-E4B-it-Q4_K_M -Apply
#
# Параметры:
#   -BaseUrl     админ-порт балансера (по умолчанию http://127.0.0.1:18081)
#   -WorkerUrl   порт воркера (по умолчанию http://127.0.0.1:18092)
#   -Backend     id выбранного бэкенда (по умолчанию cppworker-gpu-bundled-agent)
#   -Model       модель для секций загрузки/применения профиля
#   -Apply       выполнить реальный apply профиля (перезагрузка модели, ~1-2 мин)
#   -SkipLoad    не выгружать/загружать модель (быстрый прогон)
param(
    [string]$BaseUrl = "http://127.0.0.1:18081",
    [string]$WorkerUrl = "http://127.0.0.1:18092",
    [string]$Token = "",
    [string]$Backend = "cppworker-gpu-bundled-agent",
    [string]$Model = "gemma-4-E4B-it-Q4_K_M",
    [switch]$Apply,
    [switch]$SkipLoad
)

$ErrorActionPreference = "Continue"
$script:Pass = 0
$script:Fail = 0
$script:Skip = 0
$script:Failures = @()

function Write-Result([string]$name, [string]$kind, [string]$detail) {
    switch ($kind) {
        "PASS" { $script:Pass++ }
        "FAIL" { $script:Fail++; $script:Failures += "$name :: $detail" }
        "SKIP" { $script:Skip++ }
    }
    Write-Host ("{0,-4} {1}{2}" -f $kind, $name, $(if ($detail) { " -- " + $detail } else { "" }))
}

function Invoke-Api {
    param(
        [string]$Method = "GET",
        [string]$Url,
        [string]$Body = $null,
        [int]$TimeoutSec = 60
    )
    $headers = @{ "X-API-Token" = $script:Token }
    $reqArgs = @{
        UseBasicParsing = $true
        Method          = $Method
        Uri             = $Url
        Headers         = $headers
        TimeoutSec      = $TimeoutSec
    }
    if (-not [string]::IsNullOrEmpty($Body)) {
        $reqArgs.ContentType = "application/json"
        $reqArgs.Body = $Body
    }
    try {
        $resp = Invoke-WebRequest @reqArgs
        return @{ Ok = $true; Status = $resp.StatusCode; Content = $resp.Content }
    } catch {
        $status = 0
        $text = $_.Exception.Message
        try {
            if ($_.Exception.Response) {
                $status = [int]$_.Exception.Response.StatusCode
                $sr = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
                $text = $sr.ReadToEnd()
            }
        } catch { }
        return @{ Ok = $false; Status = $status; Content = $text }
    }
}

function Get-Json([string]$url, [int]$timeoutSec = 60) {
    $r = Invoke-Api -Method GET -Url $url -TimeoutSec $timeoutSec
    if (-not $r.Ok) { return $null }
    try { return ($r.Content | ConvertFrom-Json) } catch { return $null }
}

function Probe-Setting([string]$type, $orig) {
    $v = [int64]$orig
    switch ($type) {
        "bool" { if ($v -eq 0) { return 1 } else { return 0 } }
        "int" { if ($v -eq 0) { return 1 } else { return $v + 1 } }
        "duration_sec" { if ($v -eq 0) { return 30 } else { return $v + 7 } }
        "bytes" { if ($v -eq 0) { return 1024 } else { return $v * 2 } }
        default { return $v + 1 }
    }
}

if ([string]::IsNullOrWhiteSpace($Token)) {
    $envFile = Join-Path $PSScriptRoot "..\deployments\.env"
    if (Test-Path $envFile) {
        Get-Content $envFile | ForEach-Object {
            if ($_ -match '^CPPWORKER_API_TOKEN=(.*)$') { $Token = $matches[1].Trim() }
        }
    }
}
if ([string]::IsNullOrWhiteSpace($Token)) { Write-Host "нужен -Token или deployments/.env"; exit 2 }
$script:Token = $Token

Write-Host "=== audit webui settings ==="
Write-Host ("balancer={0} worker={1} backend={2} model={3}" -f $BaseUrl, $WorkerUrl, $Backend, $Model)
Write-Host ""

# ---------------------------------------------------------------- S0. доступность
$ping = Invoke-Api -Url "$BaseUrl/api/v1/ping"
if (-not $ping.Ok) { Write-Result "S0/ping" "FAIL" ("балансер недоступен: " + $ping.Content); exit 2 }
Write-Result "S0/ping" "PASS" "200"

# ------------------------------------------------------- S1. реестр горячих настроек
$settings = Get-Json "$BaseUrl/api/v1/settings"
if (-not $settings -or -not $settings.settings) {
    Write-Result "S1/settings.list" "FAIL" "GET /api/v1/settings не отдал список"
} else {
    Write-Result "S1/settings.list" "PASS" ("настроек: " + $settings.settings.Count)
    foreach ($s in $settings.settings) {
        $name = $s.name
        if (-not $s.hotReload) {
            $probe = Probe-Setting $s.type $s.effective
            $body = @{ name = $name; value = $probe } | ConvertTo-Json
            $r = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/settings" -Body $body
            if ($r.Status -eq 400 -or $r.Status -eq 409) {
                Write-Result ("S1/" + $name) "PASS" ("не горячая: PUT отклонён как ожидалось (" + $r.Status + ")")
            } else {
                Write-Result ("S1/" + $name) "FAIL" ("не горячая настройка приняла PUT: " + $r.Status + " " + $r.Content)
            }
            continue
        }
        $orig = $s.effective
        $probe = Probe-Setting $s.type $orig
        $body = @{ name = $name; value = $probe } | ConvertTo-Json
        $r = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/settings" -Body $body
        if (-not $r.Ok) { Write-Result ("S1/" + $name) "FAIL" ("PUT " + $probe + " отклонён: " + $r.Status + " " + $r.Content); continue }
        $after = Get-Json "$BaseUrl/api/v1/settings"
        $cur = $after.settings | Where-Object { $_.name -eq $name }
        $got = [int64]$cur.effective
        if ($got -ne [int64]$probe) {
            Write-Result ("S1/" + $name) "FAIL" ("после PUT " + $probe + " effective=" + $got + " (источник: " + $cur.source + ")")
        } else {
            Write-Result ("S1/" + $name) "PASS" ("PUT " + $probe + " -> GET " + $got)
        }
        # Вернуть исходное значение. Если оно пришло из OVERRIDE, reset вернёт
        # env/дефолт (это его смысл), поэтому исходное возвращаем явным PUT.
        if ([string]$s.source -eq "override") {
            $rst = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/settings" -Body (@{ name = $name; value = [int64]$orig } | ConvertTo-Json)
            if (-not $rst.Ok) { Write-Result ("S1/" + $name + "/restore") "FAIL" ("PUT " + $orig + " отклонён: " + $rst.Status + " " + $rst.Content); continue }
            $after2 = Get-Json "$BaseUrl/api/v1/settings"
            $cur2 = $after2.settings | Where-Object { $_.name -eq $name }
            if ([int64]$cur2.effective -ne [int64]$orig) {
                Write-Result ("S1/" + $name + "/restore") "FAIL" ("после PUT " + $cur2.effective + ", было " + $orig)
            } else {
                Write-Result ("S1/" + $name + "/restore") "PASS" ("override вернулся: " + $orig)
            }
        } else {
            $rst = Invoke-Api -Method POST -Url "$BaseUrl/api/v1/settings/reset" -Body (@{ name = $name } | ConvertTo-Json)
            if (-not $rst.Ok) { Write-Result ("S1/" + $name + "/reset") "FAIL" ("reset отклонён: " + $rst.Status + " " + $rst.Content); continue }
            $after2 = Get-Json "$BaseUrl/api/v1/settings"
            $cur2 = $after2.settings | Where-Object { $_.name -eq $name }
            if ([int64]$cur2.effective -ne [int64]$orig) {
                Write-Result ("S1/" + $name + "/reset") "FAIL" ("после reset " + $cur2.effective + ", было " + $orig)
            } else {
                Write-Result ("S1/" + $name + "/reset") "PASS" ("вернулось " + $orig + " (источник: " + $cur2.source + ")")
            }
        }
    }
}

# ------------------------------------------------- S2. профиль модели: поля
$profUrl = "$BaseUrl/api/v1/cppworker/model-profiles/" + [uri]::EscapeDataString($Model)
$profResp = Invoke-Api -Url $profUrl
if (-not $profResp.Ok) {
    Write-Result "S2/profile.get" "FAIL" ("GET профиля: " + $profResp.Status + " " + $profResp.Content)
} else {
    $prof = ($profResp.Content | ConvertFrom-Json).profile
    Write-Result "S2/profile.get" "PASS" "профиль получен"

    function Set-ProfileField([string]$key, $value) {
        $body = @{}; $body[$key] = $value
        return Invoke-Api -Method PUT -Url $profUrl -Body ($body | ConvertTo-Json -Depth 6)
    }
    function Get-ProfileField([string]$key) {
        $r = Invoke-Api -Url $profUrl
        if (-not $r.Ok) { return $null }
        $p = ($r.Content | ConvertFrom-Json).profile
        return $p.PSObject.Properties[$key].Value
    }
    function Test-ProfileField([string]$key, $orig, $probe) {
        $r = Set-ProfileField $key $probe
        if (-not $r.Ok) {
            # contextLength могут не пропустить по памяти — пробуем вниз
            if ($key -eq "contextLength") {
                $down = [Math]::Max(256, [int]$orig - 1024)
                if ($down -ne $orig) {
                    $r = Set-ProfileField $key $down
                    if ($r.Ok) { $probe = $down }
                }
            }
            if (-not $r.Ok) { Write-Result ("S2/" + $key) "FAIL" ("PUT " + $probe + " отклонён: " + $r.Status + " " + $r.Content); return }
        }
        $got = Get-ProfileField $key
        # bool-поля с omitempty (primary/auto/...) при false ОТСУТСТВУЮТ в JSON:
        # «нет ключа» и «false» для WebUI одно и то же (!!profile.primary).
        $gotCmp = $got
        if ($probe -is [bool] -and $null -eq $got) { $gotCmp = $false }
        if ($gotCmp -ne $probe) {
            Write-Result ("S2/" + $key) "FAIL" ("после PUT " + $probe + " прочитано " + $got)
        } else {
            Write-Result ("S2/" + $key) "PASS" ("PUT " + $probe + " -> GET " + $(if ($null -eq $got) { "<absent=false>" } else { $got }))
        }
        $back = Set-ProfileField $key $orig
        if (-not $back.Ok) { Write-Result ("S2/" + $key + "/restore") "FAIL" ("не удалось вернуть " + $orig + ": " + $back.Status); return }
        $got2 = Get-ProfileField $key
        if ($got2 -ne $orig) { Write-Result ("S2/" + $key + "/restore") "FAIL" ("вернулось " + $got2 + ", было " + $orig) }
        else { Write-Result ("S2/" + $key + "/restore") "PASS" ("вернулось " + $orig) }
    }

    $fields = @("contextLength", "batchSize", "numGpuLayers", "flashAttn", "numa", "useMmap", "parallel", "kvCacheType", "enableReasoning", "contextLengthAuto", "contextLengthMax", "primary", "notes")
    foreach ($k in $fields) {
        $prop = $prof.PSObject.Properties[$k]
        if ($null -eq $prop) { Write-Result ("S2/" + $k) "SKIP" "поле не задано в профиле"; continue }
        $orig = $prop.Value
        switch ($k) {
            { $_ -in @("flashAttn", "useMmap", "numa", "enableReasoning", "contextLengthAuto", "primary") } {
                $probe = -not [bool]$orig
            }
            "contextLength" { $probe = if ([int]$orig -ge 262144) { [int]$orig - 1024 } else { [int]$orig + 1024 } }
            "batchSize" { $probe = [int]$orig + 1 }
            "numGpuLayers" { $probe = if ([int]$orig -ge 200) { [int]$orig - 1 } else { [int]$orig + 1 } }
            "parallel" { $probe = if ([int]$orig -ge 8) { [int]$orig - 1 } else { [int]$orig + 1 } }
            "contextLengthMax" { $probe = [int]$orig + 1024 }
            "kvCacheType" { $probe = switch ([string]$orig) { "q8_0" { "q4_0" } "q4_0" { "q8_0" } default { "q8_0" } } }
            "notes" { $probe = "audit-probe" }
            default { $probe = [int]$orig + 1 }
        }
        Test-ProfileField $k $orig $probe
    }
}

# ------------------------------------- S2b. ВСЕ поля профиля на временном профиле
# Зачем: у реальной модели часть полей не задана (numa/parallel/notes/auto/max/
# primary), и «поле отсутствует» ничего не говорит о том, что WebUI его сохранит.
# Создаём временный профиль со всеми полями, прогоняем каждое и удаляем.
$tmpModel = "audit-profile-tmp-" + (Get-Date -Format "HHmmss")
$tmpUrl = "$BaseUrl/api/v1/cppworker/model-profiles/" + [uri]::EscapeDataString($tmpModel)
$tmpSeed = @{
    contextLength     = 4096
    batchSize         = 128
    numGpuLayers      = 0
    flashAttn         = $true
    numa              = $false
    useMmap           = $false
    parallel          = 2
    kvCacheType       = "q8_0"
    enableReasoning   = $true
    contextLengthAuto = $true
    contextLengthMax  = 8192
    primary           = $false
    notes             = "audit seed"
}
$r = Invoke-Api -Method PUT -Url $tmpUrl -Body ($tmpSeed | ConvertTo-Json)
if (-not $r.Ok) {
    Write-Result "S2b/create" "FAIL" ($r.Status.ToString() + " " + $r.Content)
} else {
    function Get-TmpField([string]$key) {
        $rr = Invoke-Api -Url $tmpUrl
        if (-not $rr.Ok) { return $null }
        return (($rr.Content | ConvertFrom-Json).profile.PSObject.Properties[$key]).Value
    }
    function Set-TmpField([string]$key, $value) {
        $b = @{}; $b[$key] = $value
        return Invoke-Api -Method PUT -Url $tmpUrl -Body ($b | ConvertTo-Json -Depth 6)
    }
    # 1) все поля доехали при создании
    $missing = @()
    foreach ($k in $tmpSeed.Keys) {
        $got = Get-TmpField $k
        $want = $tmpSeed[$k]
        $eq = if ($want -is [bool]) { ([bool]$got) -eq $want } elseif ($want -is [int]) { [int]$got -eq [int]$want } else { [string]$got -eq [string]$want }
        if (-not $eq) { $missing += ("{0}: got={1} want={2}" -f $k, $got, $want) }
    }
    if ($missing.Count -eq 0) { Write-Result "S2b/create.all-fields" "PASS" ("полей: " + $tmpSeed.Count) }
    else { Write-Result "S2b/create.all-fields" "FAIL" ($missing -join "; ") }
    # 2) каждое поле: PUT probe -> GET -> restore
    $tmpProbes = @{
        contextLength     = 5120
        batchSize         = 129
        numGpuLayers      = 1
        flashAttn         = $false
        numa              = $true
        useMmap           = $true
        parallel          = 3
        kvCacheType       = "q4_0"
        enableReasoning   = $false
        contextLengthAuto = $false
        contextLengthMax  = 9216
        primary           = $true
        notes             = "audit probe"
    }
    foreach ($k in $tmpProbes.Keys) {
        $orig = Get-TmpField $k
        $want = $tmpProbes[$k]
        $r2 = Set-TmpField $k $want
        if (-not $r2.Ok) { Write-Result ("S2b/" + $k) "FAIL" ("PUT отклонён: " + $r2.Status + " " + $r2.Content); continue }
        $got = Get-TmpField $k
        $eq = if ($want -is [bool]) { ([bool]$got) -eq $want } elseif ($want -is [int]) { [int]$got -eq [int]$want } else { [string]$got -eq [string]$want }
        if (-not $eq) { Write-Result ("S2b/" + $k) "FAIL" ("временный профиль: PUT " + $want + " прочитано " + $got); continue }
        Write-Result ("S2b/" + $k) "PASS" ("PUT " + $want + " -> GET " + $got)
        $null = Set-TmpField $k $orig
    }
    $del = Invoke-Api -Method Delete -Url $tmpUrl
    if ($del.Ok) { Write-Result "S2b/delete" "PASS" "временный профиль удалён" }
    else { Write-Result "S2b/delete" "FAIL" ($del.Status.ToString() + " " + $del.Content) }
}

# ------------------------------------------------------ S3. load-defaults (страница GGUF)
$ld = Invoke-Api -Url "$BaseUrl/api/v1/cppworker/load-defaults"
if (-not $ld.Ok) {
    Write-Result "S3/load-defaults.get" "FAIL" ($ld.Status.ToString() + " " + $ld.Content)
} else {
    $ldj = $ld.Content | ConvertFrom-Json
    $origCtx = 0
    if ($ldj.defaultModelProfile) { $origCtx = [int]$ldj.defaultModelProfile.contextLength }
    Write-Result "S3/load-defaults.get" "PASS" ("contextLength=" + $origCtx)
    $probeCtx = if ($origCtx -ge 262144) { $origCtx - 1024 } else { [Math]::Max(4096, $origCtx + 1024) }
    $r = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/cppworker/load-defaults" -Body (@{ contextLength = $probeCtx } | ConvertTo-Json)
    if (-not $r.Ok) {
        Write-Result "S3/load-defaults.put" "FAIL" ($r.Status.ToString() + " " + $r.Content)
    } else {
        $ld2 = (Invoke-Api -Url "$BaseUrl/api/v1/cppworker/load-defaults").Content | ConvertFrom-Json
        $got = [int]$ld2.defaultModelProfile.contextLength
        if ($got -ne $probeCtx) { Write-Result "S3/load-defaults.put" "FAIL" ("после PUT " + $probeCtx + " прочитано " + $got) }
        else { Write-Result "S3/load-defaults.put" "PASS" ("PUT " + $probeCtx + " -> GET " + $got) }
        $rst = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/cppworker/load-defaults" -Body (@{ contextLength = $origCtx } | ConvertTo-Json)
        if (-not $rst.Ok) { Write-Result "S3/load-defaults.restore" "FAIL" ($rst.Status.ToString() + " " + $rst.Content) }
        else { Write-Result "S3/load-defaults.restore" "PASS" ("вернулось " + $origCtx) }
    }
}

# --------------------------------------------------------- S4. бэкенд: поля и роль
# ВАЖНО: карточка бэкенда (GET /api/v1/backends/{id}) отдаёт МЕТРИКИ и не
# содержит labels/role — читаем их из списка, как это делает таблица WebUI.
function Get-BackendFromList {
    $lst = Get-Json ("$BaseUrl/api/v1/backends?includeUnhealthy=true")
    if (-not $lst -or -not $lst.backends) { return $null }
    return ($lst.backends | Where-Object { $_.id -eq $Backend } | Select-Object -First 1)
}
$bk = Invoke-Api -Url "$BaseUrl/api/v1/backends/$Backend"
if (-not $bk.Ok) {
    Write-Result "S4/backend.get" "FAIL" ($bk.Status.ToString() + " " + $bk.Content)
} else {
    $bj = Get-BackendFromList
    if ($null -eq $bj) { Write-Result "S4/backend.get" "FAIL" "бэкенд не найден в списке"; $bj = @{ labels = @(); role = "" } }
    $origLabels = @($bj.labels)
    Write-Result "S4/backend.get" "PASS" ("labels=" + ($origLabels -join ",") + " role=" + $bj.role)
    $probeLabels = @($origLabels) + @("audit-probe")
    $r = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/backends/$Backend" -Body (@{ labels = $probeLabels } | ConvertTo-Json)
    if (-not $r.Ok) {
        Write-Result "S4/backend.labels" "FAIL" ($r.Status.ToString() + " " + $r.Content)
    } else {
        $bj2 = Get-BackendFromList
        if (@($bj2.labels) -notcontains "audit-probe") { Write-Result "S4/backend.labels" "FAIL" ("метка не сохранилась: " + (@($bj2.labels) -join ",")) }
        else { Write-Result "S4/backend.labels" "PASS" ("PUT метки -> GET " + (@($bj2.labels) -join ",")) }
        $rr = Invoke-Api -Method PUT -Url "$BaseUrl/api/v1/backends/$Backend" -Body (@{ labels = $origLabels } | ConvertTo-Json)
        if (-not $rr.Ok) { Write-Result "S4/backend.labels.restore" "FAIL" ($rr.Status.ToString() + " " + $rr.Content) }
        else {
            $bj3 = Get-BackendFromList
            if (@($bj3.labels) -contains "audit-probe") { Write-Result "S4/backend.labels.restore" "FAIL" "audit-probe остался" }
            else { Write-Result "S4/backend.labels.restore" "PASS" ("вернулось " + (@($bj3.labels) -join ",")) }
        }
    }
    # Роль: ставим ТЕКУЩУЮ (идемпотентно) и проверяем чтение из списка.
    # "standalone" и "" эквивалентны (standalone = роль не задана).
    $origRole = [string]$bj.role
    $roleProbe = if ([string]::IsNullOrWhiteSpace($origRole) -or $origRole -eq "standalone") { "standalone" } else { $origRole }
    $r = Invoke-Api -Method Put -Url "$BaseUrl/api/v1/backends/$Backend/role" -Body (@{ role = $roleProbe } | ConvertTo-Json)
    if (-not $r.Ok) {
        Write-Result "S4/backend.role" "FAIL" ($r.Status.ToString() + " " + $r.Content)
    } else {
        $bj4 = Get-BackendFromList
        $gotRole = [string]$bj4.role
        $okRole = ($gotRole -eq $roleProbe) -or ($roleProbe -eq "standalone" -and [string]::IsNullOrWhiteSpace($gotRole))
        if (-not $okRole) { Write-Result "S4/backend.role" "FAIL" ("после PUT " + $roleProbe + " прочитано '" + $gotRole + "'") }
        else { Write-Result "S4/backend.role" "PASS" ("PUT " + $roleProbe + " -> GET '" + $gotRole + "'") }
    }
}

# ------------------------------------------------------------- S5. режим и конфиг
# Страница «Настройки» читает и сохраняет конфиг ЧЕРЕЗ /api/v1/cluster/config
# (WebUI: Api.config / Api.updateConfig). GET /api/v1/config/mode не существует
# (там только PUT), поэтому прежняя проверка была неверной.
$cc = Get-Json "$BaseUrl/api/v1/cluster/config"
if ($null -eq $cc) {
    Write-Result "S5/cluster.config.get" "FAIL" "GET /api/v1/cluster/config"
} else {
    $mode = [string]$cc.operatingMode
    $engine = [string]$cc.backendEngine
    $ctxBefore = 0
    if ($cc.llamaCpp) { $ctxBefore = [int]$cc.llamaCpp.contextLength }
    Write-Result "S5/cluster.config.get" "PASS" ("mode=" + $mode + " engine=" + $engine + " llamaCpp.contextLength=" + $ctxBefore)
    # 1) идемпотентное сохранение того же конфига (как нажатие «Сохранить»)
    $ccBody = $cc | ConvertTo-Json -Depth 12
    $r = Invoke-Api -Method Put -Url "$BaseUrl/api/v1/cluster/config" -Body $ccBody
    if (-not $r.Ok) {
        Write-Result "S5/cluster.config.put" "FAIL" ($r.Status.ToString() + " " + $r.Content)
    } else {
        $cc2 = Get-Json "$BaseUrl/api/v1/cluster/config"
        $same = ([string]$cc2.operatingMode -eq $mode)
        if ($cc2.llamaCpp) { $same = $same -and ([int]$cc2.llamaCpp.contextLength -eq $ctxBefore) }
        if (-not $same) { Write-Result "S5/cluster.config.put" "FAIL" ("после PUT mode=" + $cc2.operatingMode + " llamaCpp.contextLength=" + $cc2.llamaCpp.contextLength) }
        else { Write-Result "S5/cluster.config.put" "PASS" "идемпотентный PUT: значения те же" }
    }
    # 2) проба поля llamaCpp.ctxSize (форма «Настройки → llama.cpp»)
    if ($cc.llamaCpp -and $ctxBefore -gt 0) {
        $probeCtx = if ($ctxBefore -ge 262144) { $ctxBefore - 1024 } else { $ctxBefore + 1024 }
        $probeCfg = $ccBody | ConvertFrom-Json
        $probeCfg.llamaCpp.contextLength = $probeCtx
        $r2 = Invoke-Api -Method Put -Url "$BaseUrl/api/v1/cluster/config" -Body ($probeCfg | ConvertTo-Json -Depth 12)
        if (-not $r2.Ok) {
            Write-Result "S5/cluster.config.ctx_probe" "FAIL" ($r2.Status.ToString() + " " + $r2.Content)
        } else {
            $cc3 = Get-Json "$BaseUrl/api/v1/cluster/config"
            $gotCtx = [int]$cc3.llamaCpp.contextLength
            if ($gotCtx -ne $probeCtx) { Write-Result "S5/cluster.config.ctx_probe" "FAIL" ("PUT " + $probeCtx + " прочитано " + $gotCtx) }
            else { Write-Result "S5/cluster.config.ctx_probe" "PASS" ("PUT " + $probeCtx + " -> GET " + $gotCtx) }
            $r3 = Invoke-Api -Method Put -Url "$BaseUrl/api/v1/cluster/config" -Body $ccBody
            if (-not $r3.Ok) { Write-Result "S5/cluster.config.ctx_restore" "FAIL" ($r3.Status.ToString() + " " + $r3.Content) }
            else {
                $cc4 = Get-Json "$BaseUrl/api/v1/cluster/config"
                if ([int]$cc4.llamaCpp.contextLength -ne $ctxBefore) { Write-Result "S5/cluster.config.ctx_restore" "FAIL" ("вернулось " + $cc4.llamaCpp.contextLength + ", было " + $ctxBefore) }
                else { Write-Result "S5/cluster.config.ctx_restore" "PASS" ("вернулось " + $ctxBefore) }
            }
        }
    } else {
        Write-Result "S5/cluster.config.ctx_probe" "SKIP" "llamaCpp.contextLength отсутствует в конфиге"
    }
}
$exp = Invoke-Api -Url "$BaseUrl/api/v1/config/export"
if ($exp.Ok -and $exp.Content.Length -gt 50) { Write-Result "S5/config.export" "PASS" ("bytes=" + $exp.Content.Length) }
else { Write-Result "S5/config.export" "FAIL" ($exp.Status.ToString()) }
Write-Result "S5/config.import" "SKIP" "перезаписывает конфигурацию стенда - не делаем осознанно"

# ------------------------------------------------------------ S6. image / сессии / очередь
$img = Invoke-Api -Url "$BaseUrl/api/v1/image/backends"
if ($img.Ok) { Write-Result "S6/image.backends" "PASS" "200" } else { Write-Result "S6/image.backends" "FAIL" ($img.Status.ToString()) }
$imgp = Invoke-Api -Url "$BaseUrl/api/v1/image/model-profiles"
if ($imgp.Ok) { Write-Result "S6/image.profiles" "PASS" "200" } else { Write-Result "S6/image.profiles" "FAIL" ($imgp.Status.ToString()) }
Write-Result "S6/image.apply" "SKIP" "нужен загруженный diffusion-бандл"
$sess = Invoke-Api -Url "$BaseUrl/api/v1/sessions"
if ($sess.Ok) { Write-Result "S6/sessions" "PASS" "200" } else { Write-Result "S6/sessions" "FAIL" ($sess.Status.ToString()) }
Write-Result "S6/sessions.terminate" "SKIP" "разрушающая операция"
$q = Invoke-Api -Url "$BaseUrl/api/v1/queue/snapshot"
if ($q.Ok) { Write-Result "S6/queue.snapshot" "PASS" "200" } else { Write-Result "S6/queue.snapshot" "SKIP" ("нет ручки: " + $q.Status) }
$logs = Invoke-Api -Url "$BaseUrl/api/v1/logs/export?format=txt&limit=5"
if ($logs.Ok) { Write-Result "S6/logs.export" "PASS" "200" } else { Write-Result "S6/logs.export" "FAIL" ($logs.Status.ToString()) }

# ------------------------------------------------- S7. загрузка модели кнопкой «Загрузить»
if ($SkipLoad) {
    Write-Result "S7/load" "SKIP" "-SkipLoad"
} else {
    $hdrW = @{ "Authorization" = "Bearer " + $script:Token }
    $loadedBefore = $null
    try {
        $wm = (Invoke-WebRequest -UseBasicParsing -Uri "$WorkerUrl/api/models" -Headers $hdrW -TimeoutSec 30).Content | ConvertFrom-Json
        $loadedBefore = $wm.models | Where-Object { $_.name -eq $Model } | Select-Object -First 1
    } catch { }
    if ($loadedBefore) {
        $unloadUrl = $WorkerUrl + "/api/models/unload?name=" + [uri]::EscapeDataString($Model)
        $u = Invoke-Api -Method Post -Url $unloadUrl
        if (-not $u.Ok) { Write-Result "S7/unload" "FAIL" ($u.Status.ToString() + " " + $u.Content) }
        else { Write-Result "S7/unload" "PASS" "модель выгружена" }
        Start-Sleep -Seconds 6
    } else {
        Write-Result "S7/unload" "SKIP" "модель не была загружена"
    }
    $body = @{ operation = "load"; modelName = $Model } | ConvertTo-Json
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $r = Invoke-Api -Method Post -Url "$BaseUrl/api/v1/backends/$Backend/models" -Body $body -TimeoutSec 1200
    if (-not $r.Ok) {
        Write-Result "S7/load" "FAIL" ("HTTP " + $r.Status + " " + $r.Content)
    } else {
        $res = $r.Content | ConvertFrom-Json
        if ($res.success -eq $true) { Write-Result "S7/load" "PASS" ("success за " + [int]$sw.Elapsed.TotalSeconds + " с: " + $res.message) }
        else { Write-Result "S7/load" "FAIL" ("success=false: " + $r.Content) }
    }
    $wm2 = $null
    try { $wm2 = (Invoke-WebRequest -UseBasicParsing -Uri "$WorkerUrl/api/models" -Headers $hdrW -TimeoutSec 30).Content | ConvertFrom-Json } catch { }
    $now = $wm2.models | Where-Object { $_.name -eq $Model } | Select-Object -First 1
    if ($now -and $now.state -eq "loaded" -and $now.context_size -gt 0) {
        Write-Result "S7/loaded.facts" "PASS" ("ctx=" + $now.context_size + " kv=" + $now.kv_cache_type)
    } else {
        Write-Result "S7/loaded.facts" "FAIL" ("модель не загружена по факту: " + ($r.Content))
    }
}

# ------------------------------------ S8. apply профиля: только выбранный бэкенд + окно
if (-not $Apply) {
    Write-Result "S8/apply" "SKIP" "нужен -Apply (перезагрузка модели)"
} else {
    $pf = Get-Json $profUrl
    $wantCtx = 0
    if ($pf -and $pf.profile) { $wantCtx = [int]$pf.profile.contextLength }
    $body = @{} | ConvertTo-Json
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $r = Invoke-Api -Method Post -Url ("$profUrl/apply?backend=" + [uri]::EscapeDataString($Backend)) -Body $body -TimeoutSec 1200
    if (-not $r.Ok) {
        Write-Result "S8/apply" "FAIL" ("HTTP " + $r.Status + " " + $r.Content)
    } else {
        $res = $r.Content | ConvertFrom-Json
        $ids = @($res.backends | ForEach-Object { $_.backendId })
        $foreign = @($ids | Where-Object { $_ -ne $Backend })
        if ($foreign.Count -gt 0) {
            Write-Result "S8/apply.scope" "FAIL" ("затронуты чужие бэкенды: " + ($foreign -join ","))
        } else {
            Write-Result "S8/apply.scope" "PASS" ("только " + $Backend + " (" + ($res.backends | ForEach-Object { $_.status }) + ")")
        }
        Write-Result "S8/apply" "PASS" ("ответ за " + [int]$sw.Elapsed.TotalSeconds + " с")
    }
    # ждём загрузки и сверяем окно с профилем
    $hdrW = @{ "Authorization" = "Bearer " + $script:Token }
    $ctxFact = 0
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Seconds 5
        try {
            $wm = (Invoke-WebRequest -UseBasicParsing -Uri "$WorkerUrl/api/models" -Headers $hdrW -TimeoutSec 20).Content | ConvertFrom-Json
            $m = $wm.models | Where-Object { $_.name -eq $Model } | Select-Object -First 1
            if ($m -and $m.state -eq "loaded") { $ctxFact = [int]$m.context_size; break }
        } catch { }
    }
    if ($ctxFact -le 0) { Write-Result "S8/loaded.window" "FAIL" "модель не догрузилась после apply" }
    elseif ($ctxFact -eq $wantCtx) { Write-Result "S8/loaded.window" "PASS" ("окно применилось: " + $ctxFact + " = профиль") }
    else { Write-Result "S8/loaded.window" "FAIL" ("окно " + $ctxFact + " != профиль " + $wantCtx) }
}

Write-Host ""
Write-Host ("ИТОГ: PASS=" + $script:Pass + " FAIL=" + $script:Fail + " SKIP=" + $script:Skip)
if ($script:Fail -gt 0) {
    Write-Host "ПРОВАЛЫ:"
    $script:Failures | ForEach-Object { Write-Host ("  - " + $_) }
    exit 1
}
exit 0
