# release-all-imageworker-identity.tests.ps1 — R591 (2026-11-19): страж
# идентичности образа imageworker в выпуске.
#
# ЗАЧЕМ. Живой отказ выпуска r261119-r590: `release-all.ps1` собирает imageworker
# ЦЕЛЬЮ compose (`docker compose build imageworker`), а compose берёт build-args
# идентичности из ОКРУЖЕНИЯ ПРОЦЕССА:
#
#     deployments/docker-compose.stack.yml, сервис imageworker:
#         GIT_COMMIT:      ${GIT_COMMIT:-}
#         GIT_COMMIT_FULL: ${GIT_COMMIT_FULL:-}
#         GIT_COMMIT_DATE: ${GIT_COMMIT_DATE:-}
#         BUILD_DATE:      ${BUILD_DATE:-}
#         BUILD_AUTHOR:    ${BUILD_AUTHOR:-}
#
# В скрипте выставлялся только `IMAGE_WORKER_TAG`. Итог: образ собран, но меток
# `ru.ollamalegion.git.commit` / `created` в нём нет, и страховка R493/15b
# отказалась писать манифест выпуска:
#
#     R493/15b: манифест выпуска неполный — записи нет.
#       - imageworker: нет commit в OCI-метках образа ollama-legion/imageworker:…
#
# Остальные сервисы не страдали: они получают идентичность флагами
# `docker build @idArgs` (`Get-BuildArgLinesR486`), а этот путь — единственный,
# где идентичность идёт через окружение.
#
# ЧТО ПРОВЕРЯЕТСЯ (по исходнику, без сборки):
#   1. в ветке `"imageworker"` выставляются ВСЕ пять переменных, которые читает
#      compose, и каждая — из `$identity` (одно число, один источник);
#   2. `IMAGE_WORKER_TAG` по-прежнему выставляется (иначе `up -d` не пересоздаст
#      контейнер: он смотрит на тег образа);
#   3. прежние значения окружения восстанавливаются в `finally` — иначе
#      подменённый BUILD_AUTHOR/GIT_COMMIT утёк бы в последующие сборки;
#   4. имена переменных с обеих сторон совпадают с compose-файлом: тест падает и
#      если переименуют переменную в compose, и если её уберут из скрипта.
#
# Запуск:  Invoke-Pester scripts/release-all-imageworker-identity.tests.ps1
# (Pester 3.4; в CI — шаг «PowerShell guards».)

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent $here
$scriptPath = Join-Path $here 'release-all.ps1'
$composePath = Join-Path $repoRoot 'deployments/docker-compose.stack.yml'

# identity-переменные, которые compose-сервис imageworker читает из окружения.
$identityVars = @('GIT_COMMIT', 'GIT_COMMIT_FULL', 'GIT_COMMIT_DATE', 'BUILD_DATE', 'BUILD_AUTHOR')

Describe 'release-all.ps1: идентичность образа imageworker (R591)' {

    $scriptText = Get-Content -LiteralPath $scriptPath -Raw -Encoding UTF8
    $composeText = Get-Content -LiteralPath $composePath -Raw -Encoding UTF8

    # Ветка imageworker: от маркера до следующей ветки switch (её закрывает `}`).
    $branchStart = $scriptText.IndexOf('"imageworker" {')
    $branchEnd = $scriptText.IndexOf('if ($LASTEXITCODE -ne 0) {', $branchStart)
    $branch = ''
    if ($branchStart -ge 0 -and $branchEnd -gt $branchStart) {
        $branch = $scriptText.Substring($branchStart, $branchEnd - $branchStart)
    }

    It 'ветка imageworker существует и содержит вызов compose build' {
        ($branchStart -ge 0) | Should Be $true
        $branch | Should Match 'compose -f \$ComposeFile'
        $branch | Should Match 'build imageworker'
    }

    It 'compose-сервис imageworker читает идентичность из окружения' {
        # Страховка от «проверяем не то»: если compose перестанет брать эти
        # переменные из `${VAR}`, тест ниже потеряет смысл и должен упасть.
        foreach ($v in $identityVars) {
            $composeText | Should Match ("(?m)^\s*{0}:\s*\`$\{{{0}:-" -f $v)
        }
    }

    It 'в скрипте выставляются все пять identity-переменных из $identity' {
        foreach ($v in $identityVars) {
            $pattern = '(?m)^\s*\$env:{0}\s*=\s*\$identity\.' -f $v
            $branch | Should Match $pattern
        }
    }

    It 'тег образа по-прежнему передаётся через окружение' {
        $branch | Should Match '(?m)^\s*\$env:IMAGE_WORKER_TAG\s*=\s*\$s\.ImageTag'
    }

    It 'прежние значения окружения восстанавливаются в finally' {
        $branch | Should Match '(?m)finally\s*\{'
        $branch | Should Match 'GetEnvironmentVariable'
        $branch | Should Match '(?m)Set-Item "Env:\$n"'
    }

    It 'нет «голого» Remove-Item Env:IMAGE_WORKER_TAG без сохранения прежнего значения' {
        # Так было ДО R591: единственная переменная и безусловное удаление.
        # Если кто-то вернёт эту форму, identity снова потеряется.
        $branch | Should Not Match '(?m)^\s*Remove-Item Env:IMAGE_WORKER_TAG\s+-ErrorAction\s+SilentlyContinue\s*$'
    }

    # R591, вторая половина: перенос прошлого манифеста не должен добавлять вторую
    # запись про тот же образ под другим именем сервиса.
    #
    # ЖИВОЙ ФАКТ (выпуск r261119-r590). Прошлый манифест знал образ imageworker как
    # `sdworker` (внутри образа — бинарь sdworker), compose-сервис называется
    # `imageworker`. Перенос «сервисов, которых не было в запуске» сверял ТОЛЬКО
    # имя и добавил `sdworker=72ec210a` рядом с `imageworker=r261119-r590`: манифест
    # стал утверждать, что на стенде два разных image-worker'а.
    It 'перенос прошлого манифеста пропускает образы, репозиторий которых уже собран' {
        $scriptText | Should Match '\$builtImageRepos'
        $scriptText | Should Match 'ContainsKey\(\$prevRepo\)'
        # Пропуск должен быть видимым, а не молчаливым.
        $scriptText | Should Match 'запись прошлого манифеста'
    }
}
