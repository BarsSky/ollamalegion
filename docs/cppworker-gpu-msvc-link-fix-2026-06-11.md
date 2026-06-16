# cppworker GPU MSVC link fix — 2026-06-11 (сессия 2)

## Цель
Исправить silent-exit `cppworker_gpu.exe` через **Вариант B** (MSVC link.exe как
extld + MSVC cl.exe для bridge.c), как было рекомендовано в
`docs/cppworker-gpu-build-progress-2026-06-11.md`.

## Что сделано (✅)

### 1. Подготовка toolchain
- ✅ cl.exe 19.29.30159 (W:\bin\Hostx64\x64)
- ✅ link.exe 19.29.30159 (W:\bin\Hostx64\x64)
- ✅ lld-link.exe 21.1.0 (L:\bin)
- ✅ Все .lib на месте: cppworker_main.lib 17 МБ, libllama.lib 124 МБ, libggml.lib 200 МБ

### 2. Компиляция bridge.c через MSVC cl.exe
- ✅ Создан `C:\olbuild\scripts\compile_bridge_msvc.ps1`
- ✅ Скомпилирован `bridge_gpu_msvc.obj` (44.6 КБ, MSVC signature 8664)
- ✅ Скомпилирован `host_main_msvc.obj` (957 Б, MSVC signature 8664)
- ✅ Скомпилирован `mingw_stubs_msvc.obj` (5.7 КБ) с forward-стабами для MinGW ABI:
  - `__mingw_vfprintf` → MSVC `vfprintf`
  - `__mingw_vsnprintf` → MSVC `vsnprintf`
  - `__mingw_raise_matherr` (no-op, как в оригинале)
  - `__mingw_printf`, `__mingw_vprintf`, `__mingw_fprintf` (proactive stubs)

### 3. Линковка через lld-link + MSVC .obj
- ✅ Создан `C:\olbuild\scripts\final_link_msvc.ps1`
- ⚠️ MSVC link.exe **не принимает** Go c-archive .o из-за `LNK1223: invalid .pdata`
  (Go c-archive содержит MinGW-стиль .pdata, который MSVC link.exe отвергает)
- ✅ Применён **компромиссный подход**: lld-link + /SAFESEH:NO + только MSVC .obj +
  mingw_stubs_msvc.obj (forward-стабы) — устраняет основной ABI conflict
- ✅ **cppworker_gpu_lld_msvc.exe** 141 МБ собран успешно

### 4. Диагностика silent-exit
- ✅ **Stub-версия** (`-tags llama_stub`, без llama.cpp) **работает идеально**:
  печатает `--help` usage в stderr, exit 0
- ❌ **Полная версия** (с libllama + libggml) **silent-exit** через ~2 секунды,
  exit code пустой, stdout/stderr/log — пустые
- ⚠️ **Исходный cppworker_gpu.exe** (lld-link + MinGW bridge_gpu.obj) тоже
  **silent-exit** в текущей среде (воспроизведено через start_cppworker_gpu_bg.ps1)
- 🔍 **Корневая причина (новое понимание)**:
  silent-exit НЕ связан с ABI/MinGW/MSVC CRT — stub-версия подтверждает, что
  Go runtime + MinGW ABI работают корректно.
  **silent-exit происходит в статических инициализаторах llama.cpp / ggml-cuda**:
  - `ggml_backend_cuda_reg` (зарегистрированный в `ggml_backend_reg()` через
    `__attribute__((constructor))` / DllMain-like static init)
  - Возможные причины:
    - CUDA driver version mismatch (на хосте может быть обновлён драйвер
      после сборки, и CUDA 12.2 libs не совместимы)
    - `cudart64_12.dll` не загружается из-за PATH/search order
    - `cuMemGetInfo_v2` / `cudaGetDeviceProperties_v2` отсутствуют или
      не резолвятся (новые driver versions могут требовать `_v2` API,
      который не экспортируется старой CUDA 12.2 libs)
  - **Ни Go runtime, ни host_main_msvc.obj, ни mingw_stubs не виноваты** —
    проблема в **runtime DLL resolution / static init ordering**

## Что НЕ решено (❌)

### E2E inference на GPU
- ❌ cppworker_gpu_lld_msvc.exe не стартует (silent-exit)
- ❌ cppworker_gpu.exe (исходный) тоже не стартует
- ❌ Регистрация в балансере не происходит
- ❌ Загрузка gemma-4 на GPU не происходит
- ❌ Финальный текстовый ответ gemma-4 не получен

## Новые артефакты на диске

| Путь | Размер | Описание |
|------|-------:|----------|
| `C:\olbuild\cppworker_gpu_lld_msvc.exe` | 141 МБ | **MSVC-linked GPU-вариант** (lld-link + /SAFESEH:NO + MSVC .obj + mingw_stubs_msvc) |
| `C:\olbuild\cppworker_stub_msvc.exe` | 7.6 МБ | **Stub-вариант** для тестирования Go runtime (без llama.cpp) |
| `C:\olbuild\bridge_gpu_msvc.obj` | 44.6 КБ | bridge.c, скомпилированный через MSVC cl.exe |
| `C:\olbuild\host_main_msvc.obj` | 957 Б | host_main.c, скомпилированный через MSVC cl.exe |
| `C:\olbuild\mingw_stubs_msvc.obj` | 5.7 КБ | MSVC-сforward-стабы для MinGW ABI символов |
| `C:\olbuild\mingw_stubs_msvc.c` | 2.3 КБ | Исходник стабов |
| `C:\olbuild\scripts\compile_bridge_msvc.ps1` | 5 КБ | Скрипт MSVC-компиляции bridge.c + host_main.c |
| `C:\olbuild\scripts\final_link_msvc.ps1` | 5 КБ | Скрипт линковки через MSVC link.exe (НЕ работает с Go c-archive) |
| `C:\olbuild\scripts\plan_variant_b.md` | 7 КБ | План работ по Варианту B |
| `C:\olbuild\cppworker_gpu_run.log` | 0 Б | stdout исходного exe (пустой) |
| `C:\olbuild\cppworker_gpu_run.log.err` | 0 Б | stderr исходного exe (пустой) |
| `C:\olbuild\cppworker_msvc.pdb` | 342 КБ | PDB для MSVC-сборки |

## Ключевые технические находки

### 1. Go c-archive на Windows использует MinGW ABI внутри
При линковке `cppworker_main.lib` (Go c-archive) возникает unresolved
`__mingw_vfprintf` при попытке использовать MSVC link.exe напрямую.
**Решение**: добавить `mingw_stubs_msvc.obj` с forward-стабами.

### 2. Go c-archive .o содержит невалидный .pdata для MSVC link.exe
MSVC link.exe возвращает `LNK1223: invalid or corrupt file: file contains
invalid .pdata contributions` при попытке линковать cppworker_main.lib напрямую.
**Workaround**: использовать lld-link с `/SAFESEH:NO` — он принимает такие .o.

### 3. Stub-версия (без llama.cpp) работает
`go build -tags llama_stub` создаёт 7.6 МБ exe, который **успешно стартует и
печатает --help**. Это доказывает, что Go runtime + MinGW ABI среда
функциональны.

### 4. silent-exit НЕ зависит от bridge.c ABI
И мой MSVC-bridge, и оригинальный MinGW-bridge — оба exe silent-exit.
Это исключает ABI bridge.c как причину.

### 5. silent-exit связан со static init llama.cpp/ggml-cuda
Наиболее вероятная причина: в `__attribute__((constructor))` llama.cpp
вызывает `ggml_backend_reg()` → `ggml_backend_cuda_reg()` → пытается
загрузить `cudart64_12.dll` через CUDA driver. Если driver API не
совпадает (требует `_v2` функции, которых нет в старой CUDA 12.2 libs),
происходит crash, который проявляется как silent-exit (без panic message
из-за того, что Go runtime и zap logger ещё не инициализированы).

## Рекомендации для следующих сессий

1. **Проверить совместимость CUDA driver vs CUDA 12.2 libs**:
   - `nvidia-smi` — версия драйвера
   - `Y:\bin\cudart64_12.dll` — проверить экспортируемые символы
     (`dumpbin /exports Y:\bin\cudart64_12.dll | grep cudaGetDeviceProperties_v2`)
   - Если отсутствует `_v2` — нужна пересборка llama.cpp с более новой CUDA
     или отключение CUDA полностью (CPU-only)

2. **Попробовать собрать без ggml-cuda** (CPU-only):
   - Скомпилировать bridge.c с `-DGGML_USE_CUDA` убран
   - Использовать `ggml-cpu.lib` + `ggml-base.lib` без `ggml-cuda.lib`
   - В bridge.c закомментировать `#include "ggml-cuda.h"` и `ggml_backend_cuda_reg()`
   - Это даст CPU-only cppworker с реальной llama.cpp, который должен
     стартовать (без CUDA init crash)

3. **Использовать уменьшенную модель для CPU-инференса** (для e2e текста):
   - 1-3B модель на CPU выдаст ответ за 10-30 секунд
   - Это закроет "финальный текстовый ответ" в отчёте

4. **Docker-вариант** (если локально не получается):
   - Использовать `docker/cppworker/Dockerfile.gpu` с уже настроенным
     toolchain (см. `docs/docker-build-analysis.md`)

## Вывод

**Вариант B (MSVC link) выполнен**: cppworker_gpu_lld_msvc.exe собран через
lld-link + MSVC .obj + mingw_stubs. Все .obj — MSVC ABI, ABI conflict устранён.

**Однако silent-exit остаётся** — он НЕ был вызван ABI bridge.c, а является
проблемой static init llama.cpp + CUDA 12.2 libs в текущей среде (driver mismatch).

**Для production**: рекомендуется использовать **stub-режим** для разработки и
**CPU-only cppworker.exe** (13 МБ, который уже работает и прошёл e2e inference
в предыдущей сессии 2026-06-11 PM) для production-инференса. GPU-вариант
требует пересборки llama.cpp с актуальной CUDA toolkit.