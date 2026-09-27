<#
.SYNOPSIS
  Проверка распределения запросов между бэкендами через балансер.

.DESCRIPTION
  Отправляет N параллельных запросов в /api/chat и показывает, какой бэкенд
  обслужил каждый (по заголовку X-Backend-Id), плюс сводку и то, что видит
  оператор (нагрузка по бэкендам из admin API).

  ЗАЧЕМ ЭТОТ СКРИПТ. Распределение в балансере — не round-robin: выбирается
  бэкенд с максимальным score (scoring.go), а при равных метриках — первый по
  порядку. Поэтому «все запросы ушли на один бэкенд» — это ОЖИДАЕМО для
  быстрых ответов, и проверить реальное распределение можно только
  перекрывающимися запросами. Скрипт измеряет именно это.

  Как сделать запросы «долгими» для теста:
    * стенд на stub-образе: CPPWORKER_STUB_INFER_DELAY_MS=2000 у cppworker
      (см. deployments/docker-compose.fanout-test.yml);
    * реальный стенд: длинный ответ модели (num_predict побольше) либо
      несколько запросов на модель с большим prefill.

.PARAMETER BalancerUrl
  Клиентский API балансера, например http://127.0.0.1:18080

.PARAMETER AdminUrl
  Admin API балансера (для сводки по нагрузке), по умолчанию выводится из
  BalancerUrl заменой 18080 → 18081.

.PARAMETER Token
  X-API-Token балансера.

.PARAMETER Model
  Модель, которая УЖЕ загружена на проверяемых бэкендах (скрипт не грузит её).

.PARAMETER Count
  Сколько параллельных запросов отправить (по умолчанию 10).

.PARAMETER Container
  Если задан — запросы идут через `docker exec <container>`, что удобно, когда
  балансер доступен только изнутри docker-сети.

.EXAMPLE
  pwsh -File scripts/test-backend-fanout.ps1 -BalancerUrl http://127.0.0.1:18080 `
      -Token mytoken -Model qwen3.8:latest -Count 10

.EXAMPLE
  # Балансер в docker: обращаемся изнутри его же контейнера
  pwsh -File scripts/test-backend-fanout.ps1 -Container ol-bundled-balancer `
      -Token changeme-bundled-with-agent-token -Model qwen3.8:latest
#>
param(
    [string]$BalancerUrl = "http://127.0.0.1:18080",
    [string]$AdminUrl = "",
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$Model,
    [int]$Count = 10,
    [string]$Container = ""
)

$ErrorActionPreference = "Continue"

if ([string]::IsNullOrWhiteSpace($AdminUrl)) {
    $AdminUrl = $BalancerUrl -replace ':18080', ':18081'
}
$chatUrl = "$BalancerUrl/api/chat"

# Тело запроса кладём в контейнер (если работаем через docker exec) или во
# временный файл, чтобы не бороться с экранированием кавычек в PowerShell.
$body = @{ model = $Model; messages = @(@{ role = "user"; content = "hello" }); stream = $false } | ConvertTo-Json -Compress -Depth 5
$tmp = New-TemporaryFile
Set-Content -Path $tmp.FullName -Value $body -Encoding ascii
if ($Container) {
    docker cp $tmp.FullName "${Container}:/tmp/fanout.json" | Out-Null
} else {
    $localBody = $tmp.FullName
}
Remove-Item $tmp.FullName -Force -ErrorAction SilentlyContinue

function Invoke-One {
    param(
        [int]$N,
        [string]$Token,
        [string]$ChatUrl,
        [string]$Container,
        [string]$BodyFile
    )
    $ua = "fanout-$N/1.0"
    if ($Container) {
        $cmd = "curl -s -D - -o /dev/null -H 'X-API-Token: $Token' -H 'Content-Type: application/json' -A '$ua' --data-binary @/tmp/fanout.json $ChatUrl"
        $raw = docker exec $Container sh -c $cmd 2>&1 | Out-String
    } else {
        $raw = curl.exe -s -D - -o NUL -H "X-API-Token: $Token" -H 'Content-Type: application/json' -A $ua --data-binary "@$BodyFile" $ChatUrl 2>&1 | Out-String
    }
    [pscustomobject]@{
        N       = $N
        Code    = [regex]::Match($raw, '(?im)^HTTP/\S+\s+(\d+)').Groups[1].Value
        Backend = [regex]::Match($raw, '(?im)^X-Backend-Id:\s*(\S+)').Groups[1].Value
        Queue   = [regex]::Match($raw, '(?im)^X-Queue-Position:\s*(\S+)').Groups[1].Value
        Ms      = [regex]::Match($raw, '(?im)^X-Queue-Wait-Ms:\s*(\S+)').Groups[1].Value
    }
}

Write-Host "=== Распределение запросов: $Count параллельных на '$Model' ===" -ForegroundColor Cyan
Write-Host "балансер: $chatUrl"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$jobs = 1..$Count | ForEach-Object {
    Start-Job -ScriptBlock ${function:Invoke-One} -ArgumentList $_, $Token, $chatUrl, $Container, $localBody
}
$results = $jobs | Wait-Job -Timeout 600 | Receive-Job
$jobs | Remove-Job -Force
$sw.Stop()

$results = $results | Sort-Object N
foreach ($r in $results) {
    $extra = if ($r.Queue) { " queue=$($r.Queue) wait=$($r.Ms)ms" } else { "" }
    Write-Host ("  #{0,-3} http={1} → {2}{3}" -f $r.N, $r.Code, $r.Backend, $extra)
}

Write-Host ""
Write-Host "--- сводка ---" -ForegroundColor Cyan
$used = $results | Where-Object { $_.Backend } | Group-Object Backend | Sort-Object Count -Descending
if (-not $used) {
    Write-Host "  ни один запрос не вернул X-Backend-Id — проверьте токен/URL/модель" -ForegroundColor Yellow
} else {
    foreach ($g in $used) {
        $pct = [math]::Round(100.0 * $g.Count / $results.Count, 0)
        Write-Host ("  {0}: {1} ({2}%)" -f $g.Name, $g.Count, $pct)
    }
    $distinct = ($used | Measure-Object).Count
    if ($distinct -lt 2) {
        Write-Host "  ! все запросы ушли на ОДИН бэкенд." -ForegroundColor Yellow
        Write-Host "    Для быстрых ответов это ожидаемо: выбор идёт по score, и порог" -ForegroundColor DarkGray
        Write-Host "    prewarm.triggerLoadThreshold (по умолчанию 0.7) не включается." -ForegroundColor DarkGray
        Write-Host "    Повторите с длинными ответами / большим CPPWORKER_STUB_INFER_DELAY_MS." -ForegroundColor DarkGray
    }
    $queuePos = ($results | Where-Object { $_.Queue }).Count
    Write-Host ("  ответов с X-Queue-Position: {0} (ожидается >0, когда запросы конкурируют за слоты)" -f $queuePos)
}
Write-Host ("  всего: {0} запросов за {1:N1} с" -f $results.Count, $sw.Elapsed.TotalSeconds)

# Что видит оператор: нагрузка и вместимость по бэкендам.
Write-Host ""
Write-Host "--- как это видит балансер (admin API) ---" -ForegroundColor Cyan
$adminCmd = "curl -s -H 'X-API-Token: $Token' $AdminUrl/api/v1/backends"
$adminRaw = if ($Container) { docker exec $Container sh -c $adminCmd 2>&1 | Out-String }
            else { curl.exe -s -H "X-API-Token: $Token" "$AdminUrl/api/v1/backends" 2>&1 | Out-String }
try {
    $j = $adminRaw | ConvertFrom-Json
    foreach ($b in $j.backends) {
        Write-Host ("  {0}: status={1} maxConcurrent={2} active={3} loadedModels={4}" -f `
            $b.id, $b.status, $b.maxConcurrentRequests, $b.ollama.activeRequests, $b.loadedModelCount)
    }
} catch {
    Write-Host "  (не удалось разобрать ответ admin API)" -ForegroundColor DarkGray
}
