#!/usr/bin/env pwsh
# lib-image-registry.tests.ps1 — проверка разбора IMAGE_REGISTRY.
#
# Зачем тест. Значение этой переменной влияет на то, КУДА уйдёт сборка и откуда
# compose возьмёт образ. Ошибка здесь не падает сразу: `docker compose up`
# просто пойдёт в registry за образом, который лежит локально под другим именем,
# и оператор увидит «manifest unknown» вместо своей опечатки в .env.
#
# Запуск: pwsh -File scripts/lib-image-registry.tests.ps1

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lib-image-registry.ps1')

$script:failed = 0
$script:passed = 0

# ВНИМАНИЕ: имя параметра не должно называться Input — в PowerShell это
# автоматическая переменная, и привязка аргумента ломается молча (функция
# получает $null, тест «падает» на ровном месте). Проверено на этом файле.
function Assert-Prefix {
    param(
        [Parameter(Mandatory = $true)][string]$Case,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Registry,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Want
    )

    $got = Format-ImageRegistryPrefix -Registry $Registry
    if ($got -eq $Want) {
        $script:passed++
        Write-Host ("  ok   {0}: '{1}' -> '{2}'" -f $Case, $Registry, $got) -ForegroundColor Green
    }
    else {
        $script:failed++
        Write-Host ("  FAIL {0}: '{1}' -> '{2}', ожидалось '{3}'" -f $Case, $Registry, $got, $Want) -ForegroundColor Red
    }
}

Write-Host "=== IMAGE_REGISTRY: нормализация ===" -ForegroundColor Cyan

# Пусто и «стандартный путь» → префикса нет (образы локальные).
Assert-Prefix -Case 'пусто'                     -Registry ''                        -Want ''
Assert-Prefix -Case 'пробелы'                   -Registry '   '                     -Want ''
Assert-Prefix -Case 'local'                     -Registry 'local'                   -Want ''
Assert-Prefix -Case 'docker.io'                 -Registry 'docker.io'               -Want ''
Assert-Prefix -Case 'index.docker.io'           -Registry 'index.docker.io'         -Want ''
Assert-Prefix -Case 'Docker.IO (регистр)'       -Registry 'Docker.IO'               -Want ''

# Реальный registry → префикс со слэшем.
Assert-Prefix -Case 'хост:порт'                 -Registry 'local-docker-hub:5000'   -Want 'local-docker-hub:5000/'
Assert-Prefix -Case 'хост без порта'            -Registry 'registry.local'          -Want 'registry.local/'
Assert-Prefix -Case 'со слэшем на конце'        -Registry 'local-docker-hub:5000/'  -Want 'local-docker-hub:5000/'
Assert-Prefix -Case 'два слэша на конце'        -Registry 'reg:5000//'              -Want 'reg:5000/'
Assert-Prefix -Case 'пробелы вокруг'            -Registry '  reg:5000  '            -Want 'reg:5000/'
Assert-Prefix -Case 'с путём внутри'            -Registry 'reg:5000/mirror'         -Want 'reg:5000/mirror/'

Write-Host "=== Resolve-ImageRef: сборка полного имени ===" -ForegroundColor Cyan

$cases = @(
    @{ Prefix = '';                 Name = 'ollama-legion/balancer'; Tag = 'r83-submodule-v23'; Want = 'ollama-legion/balancer:r83-submodule-v23' }
    @{ Prefix = 'local-docker-hub:5000/'; Name = 'ollama-legion/balancer'; Tag = 'r83-submodule-v23'; Want = 'local-docker-hub:5000/ollama-legion/balancer:r83-submodule-v23' }
)
foreach ($c in $cases) {
    $got = Resolve-ImageRef -Prefix $c.Prefix -Name $c.Name -Tag $c.Tag
    if ($got -eq $c.Want) {
        $script:passed++
        Write-Host ("  ok   {0}" -f $got) -ForegroundColor Green
    }
    else {
        $script:failed++
        Write-Host ("  FAIL {0}, ожидалось {1}" -f $got, $c.Want) -ForegroundColor Red
    }
}

Write-Host ""
if ($script:failed -gt 0) {
    Write-Host ("ПРОВАЛЕНО: {0} (успешно {1})" -f $script:failed, $script:passed) -ForegroundColor Red
    exit 1
}
Write-Host ("ВСЁ ХОРОШО: {0} проверок" -f $script:passed) -ForegroundColor Green
exit 0
