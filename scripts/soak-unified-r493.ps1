<#
.SYNOPSIS
    Живой соук единого пути инференса (R493): четыре флага = 1, ответы сверяются по инвариантам.

.DESCRIPTION
    Чем этот скрипт отличается от `run-live-checks.ps1` и `docker-stack-smoke.ps1`:
    те проверяют отдельные ручки и состав стенда, а здесь один прогон смотрит на то,
    что unit-тестами не проверяется вообще — что НОВЫЕ пути (единое окно, единая
    admission-очередь, NDJSON-конвейер, единый путь инференса) под параллельной
    нагрузкой не рвут контракт клиента и не теряют заголовки/терминаторы.

    Флаги, которые включаются на время прогона (и возвращаются как были):
      LB_WINDOW_UNIFIED     — единый расчёт окна (новый путь вместо старого);
      LB_UNIFIED_ADMISSION  — единая admission-очередь для /api/chat и /v1;
      LB_STREAM_PIPE        — NDJSON пишется через конвейер, а не старым путём;
      LB_ONE_INFERENCE      — единый путь инференса на generic-поверхности.

    Проверки на КАЖДЫЙ ответ (а не «в среднем»):
      a  /api/chat stream: 200, каждая строка — валидный JSON, ровно один
         `"done":true` и он последний, сумма окон (load+prompt+eval) равна
         total_duration в пределах 25 мс, текст ответа не пуст;
      b  /api/chat non-stream + tools: 200, валидный JSON, ровно один tool_call,
         done_reason=tool_calls, аргументы разобраны;
      c  /v1/chat/completions stream: 200, все чанки — валидный JSON, ровно один
         `data: [DONE]` и он последний, finish_reason присутствует, текст не пуст.

    Плюс по заголовкам: X-Backend-Id у всех ответов и честный X-Queue-Wait-Ms там,
    где ожидание было (при нулевом ожидании заголовок не выставляется — это норма).

    ГЛАВНОЕ ПРАВИЛО: скрипт НЕ оставляет стенд с включёнными флагами. Значения
    `effective` четырёх флагов снимаются ДО прогона и возвращаются в finally —
    в том числе при падении по исключению.

    ЛОВУШКА, ИЗ-ЗА КОТОРОЙ СКРИПТ НАПИСАН ИМЕННО ТАК (R493, живой соук):
    `Start-Process -ArgumentList @(...)` не квотит элементы, а склеивает их через
    пробел. Любой аргумент с пробелом внутри (`-H "Content-Type: application/json"`)
    распадается на два, и второй curl считает ВТОРЫМ URL: `-o` достаётся первому
    (битому) адресу, тело реального ответа уходит в stdout, а `-w` печатается
    дважды — `000;...` перед телом и код после. Внешне это выглядит как «curl
    проигнорировал -o», и разбор такого лога приводил к ложным выводам о стенде.
    Поэтому здесь: пути и значения с пробелами квотируются функцией Quote-Arg,
    `-w` не используется вообще (код берётся из файла заголовков), а тело
    читается только из `.out`; если `.out` нет — это провал, а не «пустой ответ».

.PARAMETER Model
    Имя модели для запросов (как в /api/tags).

.PARAMETER Base
    База универсального порта (Ollama-поверхность). По умолчанию 127.0.0.1:18080.

.PARAMETER ApiPort
    Порт management-API балансера (настройки). По умолчанию 18081.

.PARAMETER Rounds
    Сколько раундов по три «агента» (три параллельных клиента) прогнать. По умолчанию 2.

.PARAMETER Token
    API-токен. По умолчанию берётся из deployments/.env (CPPWORKER_API_TOKEN),
    а если там пусто — из окружения контейнера балансера.

.PARAMETER WorkDir
    Куда складывать тела/заголовки ответов. По умолчанию — свежий каталог в TEMP.

.PARAMETER NoSetFlags
    Не трогать флаги (проверять то, что уже включено оператором).

.PARAMETER KeepArtifacts
    Не удалять каталог артефактов в конце (по умолчанию он остаётся только при провале).

.EXAMPLE
    powershell -NoProfile -File scripts/soak-unified-r493.ps1

.EXAMPLE
    powershell -NoProfile -File scripts/soak-unified-r493.ps1 -Rounds 3 -NoSetFlags
#>
[CmdletBinding()]
param(
    [string]$Model = 'Qwen3-4B-Instruct-2507-Q4_K_M',
    [string]$Base = 'http://127.0.0.1:18080',
    [int]$ApiPort = 18081,
    [int]$Rounds = 2,
    [int]$TimeoutSec = 300,
    [string]$Token = '',
    [string]$WorkDir = '',
    [switch]$NoSetFlags,
    [switch]$KeepArtifacts
)

$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }

$script:flags = @('LB_WINDOW_UNIFIED', 'LB_UNIFIED_ADMISSION', 'LB_STREAM_PIPE', 'LB_ONE_INFERENCE')
$script:agents = @('soak-agent-1', 'soak-agent-2', 'soak-agent-3')
$script:fails = New-Object System.Collections.ArrayList
$script:api = "http://127.0.0.1:$ApiPort"
$script:headers = @{}
$script:work = ''

$repoRoot = Split-Path -Parent $PSScriptRoot

# Quote-Arg — обернуть значение в кавычки для командной строки curl.exe.
# Нужно потому, что Start-Process склеивает -ArgumentList через пробел и не квотит.
function Quote-Arg {
    param([string]$Value)
    return '"' + $Value + '"'
}

function Get-DotEnvValue {
    param([string]$Path, [string]$Key)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match "^\s*$([regex]::Escape($Key))\s*=\s*(.+?)\s*$") { return $Matches[1].Trim('"').Trim("'") }
    }
    return ''
}

function Fail {
    param([string]$What, [string]$Detail)
    [void]$script:fails.Add(("$What :: $Detail"))
    Write-Host ("  [FAIL] {0} :: {1}" -f $What, $Detail) -ForegroundColor Red
}

# Get-Setting — текущее состояние настройки. ВАЖНО: балансер отдаёт `effective`
# (а не `value`); чтение несуществующего поля даёт 0 и «флаги выключены» на бумаге
# при реально включённых флагах — так уже было один раз, повторять не надо.
function Get-Setting {
    param([string]$Name)
    $r = Invoke-WebRequest -UseBasicParsing -Uri "$script:api/api/v1/settings" -Headers $script:headers -TimeoutSec 30
    $all = [System.Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray()) | ConvertFrom-Json
    foreach ($e in @($all.settings)) {
        if ($e.name -eq $Name) { return $e }
    }
    return $null
}

function Set-Setting {
    param([string]$Name, [int]$Value)
    $json = @{ name = $Name; value = $Value } | ConvertTo-Json -Compress
    $null = Invoke-WebRequest -UseBasicParsing -Method Put -Uri "$script:api/api/v1/settings" `
        -Headers $script:headers -Body ([System.Text.Encoding]::UTF8.GetBytes($json)) `
        -ContentType 'application/json; charset=utf-8' -TimeoutSec 30
}

# Get-ResponseCode — HTTP-код из файла заголовков (`-D`). Берём ПОСЛЕДНЮЮ строку
# статуса: curl дописывает блоки при редиректах, интересует финальный ответ.
function Get-ResponseCode {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $m = [regex]::Matches((Get-Content -LiteralPath $Path -Raw), '(?m)^HTTP/\d\.\d\s+(\d{3})')
    if ($m.Count -eq 0) { return '' }
    return $m[$m.Count - 1].Groups[1].Value
}

function Get-HeaderValue {
    param([string]$Path, [string]$Name)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $m = [regex]::Match((Get-Content -LiteralPath $Path -Raw), "(?im)^$([regex]::Escape($Name)):\s*(\S+)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Read-TextFile {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $v = Get-Content -LiteralPath $Path -Raw -Encoding UTF8
    if ($null -eq $v) { return '' }
    return $v
}

# --- Предусловия -------------------------------------------------------------
if (-not $Token) { $Token = Get-DotEnvValue -Path (Join-Path $repoRoot 'deployments/.env') -Key 'CPPWORKER_API_TOKEN' }
if (-not $Token) {
    $Token = (& docker exec ol-stack-balancer printenv LB_API_TOKEN 2>$null | Out-String).Trim()
}
if (-not $Token) { throw 'нет API-токена: ни deployments/.env (CPPWORKER_API_TOKEN), ни контейнера балансера' }
$script:headers['X-API-Token'] = $Token

try { $null = Invoke-WebRequest -UseBasicParsing -Uri "$script:Base/api/tags" -TimeoutSec 20 } catch {
    throw "стенд на $script:Base не отвечает: $($_.Exception.Message)"
}

$script:work = $WorkDir
if (-not $script:work) { $script:work = Join-Path $env:TEMP ("ol_soak_{0}" -f (Get-Date -Format 'yyyyMMdd_HHmmss')) }
if (Test-Path -LiteralPath $script:work) { Remove-Item -LiteralPath $script:work -Recurse -Force }
New-Item -ItemType Directory -Force -Path $script:work | Out-Null

$saved = @{}
foreach ($f in $script:flags) {
    $e = Get-Setting $f
    if ($null -eq $e) { throw "настройка $f не найдена в /api/v1/settings" }
    $saved[$f] = [int]$e.effective
}

Write-Host "=== стенд: $script:Base, модель: $Model, раундов: $Rounds, каталог: $script:work"
Write-Host ("=== флаги до прогона: " + (($script:flags | ForEach-Object { "$_=$($saved[$_])" }) -join ', '))

try {
    if (-not $NoSetFlags) {
        foreach ($f in $script:flags) { Set-Setting $f 1 }
        foreach ($f in $script:flags) {
            $e = Get-Setting $f
            if ([int]$e.effective -ne 1) { throw "$f не включился: effective=$($e.effective)" }
        }
        Write-Host ("=== флаги включены: " + (($script:flags | ForEach-Object { "$_=1" }) -join ', '))
    } else {
        foreach ($f in $script:flags) {
            $e = Get-Setting $f
            if ([int]$e.effective -ne 1) { Write-Host "  [WARN] $f = $($e.effective) — прогон без включённого флага" -ForegroundColor Yellow }
        }
    }

    # Тела запросов: a — поток NDJSON, b — non-stream с инструментом, c — SSE.
    $bodies = @{
        a = @{ url = "$script:Base/api/chat"; json = (@{
                    model = $Model; stream = $true; temperature = 0; seed = 42
                    options = @{ num_predict = 32 }
                    messages = @(@{ role = 'user'; content = 'Перечисли три цвета.' })
                } | ConvertTo-Json -Depth 6 -Compress) }
        b = @{ url = "$script:Base/api/chat"; json = (@{
                    model = $Model; stream = $false; temperature = 0; seed = 42
                    options = @{ num_predict = 32 }
                    tools = @(@{ type = 'function'; function = @{
                                name = 'get_weather'; description = 'weather'
                                parameters = @{ type = 'object'; properties = @{ city = @{ type = 'string' } }; required = @('city') }
                            } })
                    messages = @(@{ role = 'user'; content = 'Какая погода в Москве? Вызови инструмент.' })
                } | ConvertTo-Json -Depth 8 -Compress) }
        c = @{ url = "$script:Base/v1/chat/completions"; json = (@{
                    model = $Model; stream = $true; temperature = 0; seed = 42; max_tokens = 32
                    messages = @(@{ role = 'user'; content = 'Назови два цвета.' })
                } | ConvertTo-Json -Depth 6 -Compress) }
    }
    foreach ($k in 'a', 'b', 'c') {
        [IO.File]::WriteAllText((Join-Path $script:work "$k.json"), $bodies[$k].json, (New-Object System.Text.UTF8Encoding($false)))
    }

    $rows = New-Object System.Collections.ArrayList
    $swAll = [Diagnostics.Stopwatch]::StartNew()

    foreach ($round in 1..$Rounds) {
        $jobs = New-Object System.Collections.ArrayList
        foreach ($ua in $script:agents) {
            $dir = Join-Path $script:work ("r{0}_{1}" -f $round, $ua)
            New-Item -ItemType Directory -Force -Path $dir | Out-Null
            foreach ($k in 'a', 'b', 'c') {
                # -w квотируется: после Quote-Arg точка с запятой безопасна, а тело
                # по-прежнему уходит в .out — в stdout попадает только строка таймингов.
                $curlArgs = @(
                    '-s', '--max-time', "$TimeoutSec",
                    '-o', (Quote-Arg (Join-Path $dir "$k.out")),
                    '-D', (Quote-Arg (Join-Path $dir "$k.hdr")),
                    '-w', (Quote-Arg '%{http_code};%{time_starttransfer};%{time_total}'),
                    '-H', (Quote-Arg 'Content-Type: application/json'),
                    '-A', (Quote-Arg $ua),
                    '--data-binary', (Quote-Arg "@$(Join-Path $script:work "$k.json")"),
                    (Quote-Arg $bodies[$k].url)
                )
                $proc = Start-Process -FilePath curl.exe -ArgumentList $curlArgs -PassThru -NoNewWindow `
                    -RedirectStandardOutput (Join-Path $dir "$k.std") `
                    -RedirectStandardError (Join-Path $dir "$k.err")
                $started = Get-Date
                [void]$jobs.Add([pscustomobject]@{ Label = "r$round/$ua/$k"; Kind = $k; Proc = $proc; Started = $started; Dir = $dir })
            }
        }

        $swRound = [Diagnostics.Stopwatch]::StartNew()
        foreach ($j in $jobs) {
            $left = ($TimeoutSec + 30) * 1000 - [int]$swRound.ElapsedMilliseconds
            if ($left -lt 1000) { $left = 1000 }
            if (-not $j.Proc.WaitForExit($left)) {
                try { $j.Proc.Kill() } catch { }
                Fail $j.Label "curl не уложился в $($TimeoutSec + 30) с"
            }
        }

        foreach ($j in $jobs) {
            $k = $j.Kind
            $label = $j.Label
            $body = Read-TextFile (Join-Path $j.Dir "$k.out")
            $hdrPath = Join-Path $j.Dir "$k.hdr"
            # Тайминги — из -w (http_code;time_starttransfer;time_total). Если строки
            # нет (старый curl/сбой), код берём из заголовков, а времена остаются -1.
            $stdRaw = (Read-TextFile (Join-Path $j.Dir "$k.std")).Trim()
            $parts = @($stdRaw -split ';')
            $code = if ($parts.Count -ge 1 -and $parts[0] -match '^\d{3}$') { $parts[0] } else { Get-ResponseCode $hdrPath }
            $firstByte = if ($parts.Count -ge 2) { [double]$parts[1] } else { -1 }
            $wall = if ($parts.Count -ge 3) { [double]$parts[2] } else { -1 }
            $waitMs = Get-HeaderValue -Path $hdrPath -Name 'X-Queue-Wait-Ms'
            $backendId = Get-HeaderValue -Path $hdrPath -Name 'X-Backend-Id'
            $row = [pscustomobject]@{
                label = $label; kind = $k; code = $code; wall = $wall; firstByte = $firstByte; wait = $waitMs
                backend = $backendId; bytes = $body.Length
                doneTrue = 0; tailAfterDone = -1; badLines = 0; windowSum = -1; windowTotal = -1
                toolCalls = 0; args = ''; reason = ''; doneMarkers = 0; badChunks = 0; text = ''
            }

            if (-not $code) { Fail $label 'нет файла заголовков или в нём нет строки статуса (curl не выполнил запрос)' }
            elseif ($code -ne '200') {
                $snippet = ($body -replace '\s+', ' ').Trim()
                if ($snippet.Length -gt 160) { $snippet = $snippet.Substring(0, 160) + '…' }
                Fail $label "HTTP $code (body: $snippet)"
            }
            if (-not $body) { Fail $label 'пустое тело ответа (проверьте .std: тело могло уйти в stdout)' }
            if (-not $backendId) { Fail $label 'нет X-Backend-Id (ответ не от бэкенда?)' }

            if ($k -eq 'a') {
                $lines = @($body -split "`r?`n" | Where-Object { $_.Trim() })
                $objs = New-Object System.Collections.ArrayList
                foreach ($l in $lines) {
                    try { [void]$objs.Add(($l | ConvertFrom-Json)) } catch { $row.badLines++ }
                }
                if ($row.badLines -gt 0) { Fail $label "$($row.badLines) строк NDJSON не разбираются" }
                $doneIdx = @()
                for ($i = 0; $i -lt $objs.Count; $i++) { if ($objs[$i].done -eq $true) { $doneIdx += $i } }
                $row.doneTrue = $doneIdx.Count
                if ($doneIdx.Count -ne 1) {
                    Fail $label "ожидался ровно один done:true, найдено $($doneIdx.Count)"
                } elseif ($doneIdx[0] -ne $objs.Count - 1) {
                    $row.tailAfterDone = $objs.Count - $doneIdx[0] - 1
                    Fail $label "после done:true ещё $($row.tailAfterDone) объектов"
                } else { $row.tailAfterDone = 0 }
                $sb = New-Object System.Text.StringBuilder
                foreach ($o in $objs) { if ($o.message -and $o.message.content) { [void]$sb.Append($o.message.content) } }
                $row.text = $sb.ToString()
                if (-not $row.text.Trim()) { Fail $label 'пустой текст ответа' }
                if ($doneIdx.Count -eq 1) {
                    $f = $objs[$doneIdx[0]]
                    $row.reason = [string]$f.done_reason
                    $sum = [int64]$f.load_duration + [int64]$f.prompt_eval_duration + [int64]$f.eval_duration
                    $row.windowSum = $sum
                    $row.windowTotal = [int64]$f.total_duration
                    if ([math]::Abs($sum - [int64]$f.total_duration) -gt 25000000) {
                        Fail $label ("сумма окон {0} нс != total {1} нс" -f $sum, $f.total_duration)
                    }
                }
            } elseif ($k -eq 'b') {
                $o = $null
                try { $o = $body | ConvertFrom-Json } catch { Fail $label 'ответ не JSON' }
                if ($o) {
                    $row.reason = [string]$o.done_reason
                    $tc = @()
                    if ($o.message -and $o.message.tool_calls) { $tc = @($o.message.tool_calls) }
                    $row.toolCalls = $tc.Count
                    if ($row.toolCalls -ne 1) {
                        Fail $label "ожидался ровно один tool_call, найдено $($row.toolCalls)"
                    } else {
                        $row.args = ($tc[0].function.arguments | ConvertTo-Json -Depth 4 -Compress)
                        if ([string]$tc[0].function.name -ne 'get_weather') { Fail $label "имя инструмента '$($tc[0].function.name)'" }
                        if ($row.args -notmatch 'Москва') { Fail $label "в аргументах нет города: $($row.args)" }
                    }
                    if ($row.reason -ne 'tool_calls') { Fail $label "done_reason='$($row.reason)', ожидался tool_calls" }
                    if ($o.done -ne $true) { Fail $label 'done != true в non-stream ответе' }
                }
            } else {
                $row.doneMarkers = ([regex]::Matches($body, 'data:\s*\[DONE\]')).Count
                if ($row.doneMarkers -ne 1) { Fail $label "ожидался ровно один data: [DONE], найдено $($row.doneMarkers)" }
                if ($row.doneMarkers -eq 1) {
                    $tail = $body.Substring($body.LastIndexOf('[DONE]') + 6)
                    if ($tail.Trim()) { Fail $label "после [DONE] есть данные: '$($tail.Trim())'" }
                }
                $sb = New-Object System.Text.StringBuilder
                foreach ($m in [regex]::Matches($body, '(?m)^data: (\{.*\})\s*$')) {
                    $chunk = $null
                    try { $chunk = $m.Groups[1].Value | ConvertFrom-Json } catch { $row.badChunks++; continue }
                    $ch = @($chunk.choices) | Select-Object -First 1
                    if ($ch.delta -and $ch.delta.content) { [void]$sb.Append([string]$ch.delta.content) }
                    if ($ch.finish_reason) { $row.reason = [string]$ch.finish_reason }
                }
                $row.text = $sb.ToString()
                if ($row.badChunks -gt 0) { Fail $label "$($row.badChunks) SSE-чанков не разбираются" }
                if (-not $row.reason) { Fail $label 'нет finish_reason ни в одном чанке' }
                if (-not $row.text.Trim()) { Fail $label 'пустой текст ответа' }
            }

            [void]$rows.Add($row)
        }
    }
    $swAll.Stop()

    Write-Host "`n=== ответы (по одному на запрос):" -ForegroundColor Cyan
    foreach ($r in $rows) {
        $win = if ($r.windowSum -ge 0) { "{0:N2}/{1:N2} с" -f ($r.windowSum / 1e9), ($r.windowTotal / 1e9) } else { '—' }
        if ($r.kind -eq 'a') {
            Write-Host ("  {0,-14} HTTP {1}  {2,6:N2} с  ждал {3,7} мс  {4}  окна {5}  done:true={6}  '{7}'" -f `
                    $r.label, $r.code, $r.wall, $r.wait, $r.backend, $win, $r.doneTrue, $r.text)
        } elseif ($r.kind -eq 'b') {
            Write-Host ("  {0,-14} HTTP {1}  {2,6:N2} с  ждал {3,7} мс  {4}  tool_calls={5} {6}  reason={7}" -f `
                    $r.label, $r.code, $r.wall, $r.wait, $r.backend, $r.toolCalls, $r.args, $r.reason)
        } else {
            Write-Host ("  {0,-14} HTTP {1}  {2,6:N2} с  первый байт {3,6:N2} с  ждал {4,7} мс  {5}  [DONE]={6}  finish={7}  '{8}'" -f `
                    $r.label, $r.code, $r.wall, $r.firstByte, $r.wait, $r.backend, $r.doneMarkers, $r.reason, $r.text)
        }
    }

    $codes = @($rows | ForEach-Object { $_.code } | Sort-Object -Unique)
    $waits = @($rows | Where-Object { $_.wait } | ForEach-Object { [int]$_.wait })
    Write-Host ("`n=== сводка: запросов {0}; коды {1}; ожидание слотов: {2}" -f `
            $rows.Count, ($codes -join ','),
        $(if ($waits.Count) { "$($waits.Count) из $($rows.Count), max $((($waits) | Measure-Object -Maximum).Maximum) мс" } else { 'никто не ждал' }))
    Write-Host ("=== общее время прогона: {0:N1} с" -f $swAll.Elapsed.TotalSeconds)

    if ($script:fails.Count -gt 0) {
        Write-Host "`n=== ПРОВАЛОВ: $($script:fails.Count)" -ForegroundColor Red
        Write-Host "    артефакты: $script:work"
    } else {
        Write-Host "`n=== ПРОЙДЕНО: все инварианты держатся" -ForegroundColor Green
        if (-not $KeepArtifacts) { Remove-Item -LiteralPath $script:work -Recurse -Force }
    }
} finally {
    if (-not $NoSetFlags) {
        foreach ($f in $script:flags) {
            try { Set-Setting $f $saved[$f] } catch { Write-Host "  [WARN] не вернул $f = $($saved[$f]): $($_.Exception.Message)" -ForegroundColor Yellow }
        }
        $after = @()
        foreach ($f in $script:flags) {
            try { $after += "$f=$((Get-Setting $f).effective)" } catch { $after += "$f=?" }
        }
        Write-Host ("`n=== флаги после прогона: " + ($after -join ', '))
    }
}

if ($script:fails.Count -gt 0) { exit 1 }
exit 0
