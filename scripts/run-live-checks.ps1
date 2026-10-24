# scripts/run-live-checks.ps1 — R219 (2026-10-21).
#
# Живые проверки стенда: то, что unit-тесты поймать не могут — поведение
# балансера и воркера целиком. Ловит регрессии класса R214-R219:
#   - окно контекста в /v1/models и X-Model-Context-Length (R216);
#   - размышления не в content, не дублируются, ответ не пустой (R216/R219);
#   - параллельные запросы не путают ответы (очередь);
#   - история диалога доезжает до модели;
#   - объявление image-инструмента соответствует LB_IMAGE_TOOL_INJECT.
#
# Требования: стенд поднят, модель загружена.
# Запуск:
#   powershell -File scripts/run-live-checks.ps1
#   powershell -File scripts/run-live-checks.ps1 -Model "Qwen3.8-27B-UD-Q4_K_M.gguf"
#
# Код возврата: 0 — всё зелёное, 1 — есть провалы.
[CmdletBinding()]
param(
    [string]$Model = "gemma-4-E4B-it-Q4_K_M.gguf",
    [string]$Base = "http://127.0.0.1:18080",
    [string]$TimeoutSec = "300"
)

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root

# Ожидаемый режим инъекции — из deployments/.env (то, что реально задеплоено).
$inject = ''
$envFile = Join-Path $root 'deployments/.env'
if (Test-Path $envFile) {
    $line = Select-String -Path $envFile -Pattern '^LB_IMAGE_TOOL_INJECT=' | Select-Object -First 1
    if ($line) { $inject = ($line.Line -split '=', 2)[1].Trim() }
}

# R229 (2026-10-23): ПРОГРЕВ перед проверками.
#
# ЖИВОЙ ДЕФЕКТ САМОГО ГЕЙТА. Если модель не загружена (стенд только что поднят или
# модель выгружена), первый же запрос лениво её поднимает — и две проверки падают
# ЛОЖНО: X-Model-Context-Length на этом ответе отсутствует (заголовок строится по
# уже загруженной модели), а поведение размышлений на холодной загрузке другое.
# Проверки описывают установившийся контракт, поэтому сначала один запрос «на
# прогрев» (без замеров), и только потом сами проверки. Стоимость холодной
# загрузки уже покрыта contract-suite (шаг 0) и тестами lazy-load.
Write-Host "=== прогрев (холодная загрузка не в проверках) ===" -ForegroundColor DarkGray
try {
    $warmModel = ($Model -replace '\.gguf$', '')
    $warmBody = @{
        model    = $warmModel
        stream   = $false
        messages = @(@{ role = 'user'; content = 'Ответь одним словом: ок.' })
        options  = @{ num_predict = 5 }
    } | ConvertTo-Json -Depth 5
    # Токен НЕ хардкодим: сам live-regression-r219.js ходит без токена
    # (inference-поверхность стенда доступна без него), поэтому прогрев ведёт себя
    # так же — иначе он мог бы пройти по другой авторизации, чем проверки.
    $warmHeaders = @{ 'Content-Type' = 'application/json' }
    $warmResp = Invoke-WebRequest -Uri "$Base/api/chat" -Method Post -Headers $warmHeaders -Body $warmBody -UseBasicParsing -TimeoutSec 300
    Write-Host ("  прогрев: HTTP " + $warmResp.StatusCode) -ForegroundColor DarkGray

    # ЖДЁМ, ПОКА МОДЕЛЬ ПОЯВИТСЯ В /v1/models. Живой случай (2026-10-23):
    # прогрев вернул 200, но метрики балансера ещё не содержали загруженную модель —
    # live-regression через миллисекунды получал PRECONDITION «модель не загружена»
    # и объявлял часть проверок недействительными. Ответ 200 и видимость модели —
    # РАЗНЫЕ события (метрики приходят следующим тиком поллера).
    $visible = $false
    $deadline = (Get-Date).AddSeconds(90)
    while ((Get-Date) -lt $deadline) {
        try {
            $models = (Invoke-WebRequest -Uri "$Base/v1/models" -UseBasicParsing -TimeoutSec 10).Content | ConvertFrom-Json
            foreach ($mm in @($models.data)) {
                $id = [string]$mm.id
                $ctx = 0; try { $ctx = [int]$mm.context_length } catch { $ctx = 0 }
                # Контекст обязателен: PRECONDITION live-regression проверяет именно
                # context_length, а балансер на первом тике метрик отдаёт запись БЕЗ
                # окна. «Модель есть» ≠ «окно уже видно».
                if ($id -and $ctx -gt 0 -and ($id -eq $warmModel -or $id -like ($warmModel + '*') -or $warmModel -like ($id + '*'))) { $visible = $true; break }
            }
        } catch { }
        if ($visible) { break }
        Start-Sleep -Seconds 2
    }
    if ($visible) { Write-Host '  модель видна в /v1/models: да' -ForegroundColor DarkGray } else { Write-Host '  модель видна в /v1/models: НЕТ (90 c)' -ForegroundColor Yellow }
} catch {
    Write-Host ("  прогрев не удался (проверки продолжаются): " + $_.Exception.Message) -ForegroundColor Yellow
}
Write-Host "=== Живая регрессия стенда ===" -ForegroundColor Cyan
Write-Host ("base={0}  model={1}  inject={2}" -f $Base, $Model, $(if ($inject) { $inject } else { '(не задан)' }))

$nodeArgs = @(
    'scripts/live-regression-r219.js',
    "--base=$Base",
    "--model=$Model",
    "--timeout=$TimeoutSec"
)
if ($inject) { $nodeArgs += "--inject=$inject" }

$out = & node @nodeArgs 2>&1
$code = $LASTEXITCODE
$out | ForEach-Object { Write-Host $_ }

Pop-Location
if ($code -ne 0) {
    Write-Host "ИТОГ: есть провалы (см. FAIL выше)" -ForegroundColor Red
    exit 1
}
Write-Host "ИТОГ: живая регрессия зелёная" -ForegroundColor Green
exit 0
