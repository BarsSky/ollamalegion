# Скрипт для запуска тестов CppWorker в stub-режиме на Windows.
# Проблема: go test ./cmd/cppworker -tags llama_stub может не сработать,
# потому что C-исходник c/bridge/bridge.c не фильтруется build tag'ом.
# Решение: собираем тестовый бинарник отдельно и запускаем его.
# Источник: .clinerules, раздел 10.

param(
    [string]$OutDir = "."
)

$ErrorActionPreference = "Stop"

$exe = Join-Path $OutDir "cppworker_test.exe"
$pkg = "./cmd/cppworker"

Write-Host "Building CppWorker test binary (stub)..."
go test -c $pkg -tags llama_stub -o $exe

if ($LASTEXITCODE -ne 0) {
    throw "go test -c failed"
}

Write-Host "Running CppWorker tests..."
& $exe

if ($LASTEXITCODE -ne 0) {
    throw "CppWorker tests failed"
}

Write-Host "CppWorker stub tests passed."