# build-imageworker-cuda.ps1 — CUDA-образ imageworker под три карты (75;86;120).
#
# ЗАЧЕМ. Рабочий образ собран только под sm_86 (проверено cuobjdump по бинарю из
# контейнера), то есть на Quadro sm_75 и на RTX 5060 sm_120 движок упал бы так же,
# как падал cppworker до R135. Список IMAGE_WORKER_CUDA_ARCH=75;86;120 в .env не
# применялся, потому что базы стояли CUDA 12.2: sm_120 появился только в 12.8,
# nvcc 12.2 такой список арок не принимает.
#
# ВАЖНО: собираем ЦЕЛЬЮ imageworker-cuda. Дефолтная цель imageworker-vulkan тянет
# ГОТОВЫЙ релизный sd-server (арки заданы апстримом, наш CUDA_ARCH не участвует) и
# заменять работающий CUDA-движок на Vulkan нельзя: на NVIDIA в WSL2 Vulkan уходит
# на CPU (замер в Dockerfile: 251 с против 9-76 с).
$ErrorActionPreference = 'Continue'
$root = 'C:\Ollama\ollamalegion'
Set-Location $root

$tag = 'cuda-75-86-120'
$img = "ollama-legion/imageworker:$tag"

Write-Host '=== сборка imageworker-cuda (75;86;120, CUDA 12.8.0) ==='
docker build -f docker/imageworker/Dockerfile --target imageworker-cuda `
    --build-arg CUDA_ARCH="75;86;120" `
    --build-arg CUDA_DEVEL_BASE=mirror.gcr.io/nvidia/cuda:12.8.0-devel-ubuntu22.04 `
    --build-arg RUNTIME_BASE=mirror.gcr.io/nvidia/cuda:12.8.0-runtime-ubuntu22.04 `
    -t $img . > _diag\build-imageworker-cuda.log 2>&1
$rc = $LASTEXITCODE
Write-Host "  exit: $rc"
Select-String -Path _diag\build-imageworker-cuda.log -Pattern 'naming to|ERROR|error:|Unsupported gpu' |
    Select-Object -Last 4 | ForEach-Object { '  ' + $_.Line.Trim().Substring(0, [Math]::Min(150, $_.Line.Trim().Length)) }
if ($rc -ne 0) { Write-Host 'СБОРКА НЕ УДАЛАСЬ'; exit $rc }

Write-Host '=== приёмка: арки внутри бинаря (главный критерий, а не тег) ==='
$tmp = 'tmp_iw_cuda'
docker rm -f $tmp 2>&1 | Out-Null
docker create --name $tmp $img 2>&1 | Out-Null
docker cp "${tmp}:/app/sd-server/sd-server" _diag\sd-server-cuda 2>&1 | Out-Null
docker rm -f $tmp 2>&1 | Out-Null
$cuobjdump = 'C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v13.3\bin\cuobjdump.exe'
& $cuobjdump --list-elf _diag\sd-server-cuda 2>&1 |
    Select-String -Pattern '(sm|compute)_\d+' -AllMatches |
    ForEach-Object { $_.Matches } | ForEach-Object { $_.Value } |
    Sort-Object -Unique | ForEach-Object { Write-Host "  арка: $_" }

Write-Host '=== проверка CUDA-линковки рантайма ==='
docker run --rm --entrypoint sh $img -c "ldd /app/sd-server/sd-server | grep -Ei 'cudart|cublas' | head -3"
Write-Host 'ГОТОВО'
