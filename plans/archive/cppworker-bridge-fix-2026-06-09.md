# cppworker: диагностика crash и фикс c/bridge/bridge.c (ОБНОВЛЕНО)

**Дата:** 2026-06-09 (обновление 23:50 MSK)
**Автор:** Cline
**Контекст:** контейнер `ollama-legion/cppworker:fix-d6-v2` запускался, но cppworker падал silent без логов.

---

## TL;DR

`c/bridge/bridge.c` написан под **старую** llama.cpp API, а в репозитории (и в Docker build context) лежит **новая** llama.cpp (b4500+). Несовместимые объявления (`flash_attn_type` enum, `llama_new_context_with_model`, `llama_memory_clear`) приводят к ошибкам компиляции/линковки либо silent crash при GPU init.

**Исправлено 4 точки несовместимости**, нативно собран CPU-only `cppworker.exe` (21 МБ), `/health` отвечает 200 OK. GPU-сборка в Docker запущена, но **Docker daemon упал** на стадии 22% CUDA-компиляции — требуется перезапуск инфраструктуры.

---

## Фикс в `c/bridge/bridge.c` (применён, проверен)

| Было (старая llama.cpp) | Стало (новая llama.cpp b4500+) |
|---|---|
| `ctx_params.flash_attn_type = config->flash_attn_type >= 0 ? (==0 ? LLAMA_FLASH_ATTN_TYPE_DISABLED : LLAMA_FLASH_ATTN_TYPE_ENABLED) : LLAMA_FLASH_ATTN_TYPE_AUTO;` | `if (config->flash_attn_type >= 0) { ctx_params.flash_attn = (config->flash_attn_type != 0); }` |
| `struct llama_context *context = llama_new_context_with_model(model, ctx_params);` | `struct llama_context *context = llama_init_from_model(model, ctx_params);` |
| `llama_memory_t mem = llama_get_memory(im->context); if (mem != NULL) { llama_memory_clear(mem, true); }` | `llama_kv_self_clear(im->context);` |
| `struct llama_vocab *vocab = llama_model_get_vocab(model);` (warning: discards const) | `const struct llama_vocab *vocab = llama_model_get_vocab(model);` + cast при записи в struct: `im->vocab = (struct llama_vocab *)vocab;` |

---

## Нативная сборка (CPU-only) — ✅ УСПЕШНО

Среда: Windows 11, MinGW gcc 13.2.0, CMake 4.2.0-rc2, Go 1.25.4.

```powershell
# 1. llama.cpp CPU-only
cmake -S C:\olbuild\c\llama.cpp -B C:\olbuild\c\llama.cpp\build-cpu -G "MinGW Makefiles" \
  -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=OFF -DGGML_VULKAN=OFF -DGGML_OPENBLAS=OFF \
  -DGGML_NATIVE=OFF -DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF \
  -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF -DLLAMA_CURL=OFF
cmake --build C:\olbuild\c\llama.cpp\build-cpu -j 4    # 2 мин
# → libllama.a, libggml.a, libggml-cpu.a, libggml-base.a

# 2. bridge
cmake -S C:\olbuild\c\bridge -B C:\olbuild\c\bridge\build -G "MinGW Makefiles"
cmake --build C:\olbuild\c\bridge\build -j 4
# → libollamalegion_bridge.a (17 КБ)

# 3. cppworker.exe (Go + CGo)
$env:CGO_CFLAGS = "-IC:\olbuild\c\llama.cpp\include -IC:\olbuild\c\llama.cpp\ggml\include -IC:\olbuild\c\bridge -fopenmp"
$env:CGO_LDFLAGS = "-LC:\olbuild\c\llama.cpp\build\src -LC:\olbuild\c\llama.cpp\build\ggml\src -LC:\olbuild\c\bridge\build -lollamalegion_bridge -lllama -lggml -lggml-cpu -lggml-base -lws2_32 -lpthread -lstdc++ -lsupc++ -lgomp -static-libgcc -static-libstdc++"
go build -tags "llama_real" -o C:\olbuild\cppworker.exe ./cmd/cppworker
# → cppworker.exe (21 МБ)
```

**Проверка:**
```
GET /health     → 200 OK  {"status":"ok","version":"0.2.0 (real llama.cpp linked)"}
GET /api/models → 200 OK  {"count":0,"models":[]}
```

cppworker нативно запускается на Windows 11, отвечает на HTTP. Это значит, что **bridge.Init()** и **bridge.GetGPUCount()** отрабатывают без crash (для CPU-only GetGPUCount возвращает 0).

---

## GPU-сборка в Docker — ⚠️ ЗАБЛОКИРОВАНО

Запущена сборка `ollama-legion/cppworker:fix-d6-v3` с исправленным bridge.c и CUDA 12.2:
```bash
docker build -t ollama-legion/cppworker:fix-d6-v3 \
  -f docker/cppworker/Dockerfile.gpu --target runtime .
```

**Прогресс:** Stage 1 (llama-builder) шёл ~30 мин, дошёл до 22% ggml-cuda (компилировал tsembd.cu.o), затем BuildKit завис (CPU активен, но лог не обновляется 1+ час). При попытке принудительно убить процесс BuildKit (PID 28644) Docker daemon упал, сейчас:
- `com.docker.service` — Stopped, не запускается (`Cannot open com.docker.service service`)
- WSL2 `docker-desktop` — Running, но `docker version` → 500 Internal Server Error
- API Docker недоступен

**Восстановление требует:**
- Перезагрузка Windows ИЛИ
- Docker Desktop → Troubleshoot → Restart / Reset to factory defaults ИЛИ
- Полный uninstall + reinstall Docker Desktop

После восстановления команда для продолжения GPU-сборки:
```bash
docker build -t ollama-legion/cppworker:fix-d6-v3 \
  -f docker/cppworker/Dockerfile.gpu \
  --build-arg CUDA_ARCH=86 \
  --target runtime .
```

---

## Альтернативный GPU-путь (для нативной Windows)

MinGW gcc на Windows **не совместим** с официальной `cudart.lib` (ABI Microsoft x64). Для GPU-сборки нативно нужны:
1. **MSVC 2019 BuildTools** — есть в `C:\Program Files (x86)\Microsoft Visual Studio\2019\BuildTools\VC\Tools\MSVC\14.29.30133\bin\Hostx64\x64\cl.exe`
2. **CUDA Toolkit 12.x** — installer был запущен, но не оставил артефактов в `C:\Program Files\NVIDIA GPU Computing Toolkit` (видимо, требует ручного подтверждения setup.exe с правами админа)
3. **vcvars64.bat** для активации MSVC environment
4. **Сборка через cmake + MSVC** (вместо MinGW)

Команда (после восстановления CUDA и docker):
```powershell
& 'C:\Program Files (x86)\Microsoft Visual Studio\2019\BuildTools\VC\Auxiliary\Build\vcvars64.bat'
cmake -S C:\olbuild\c\llama.cpp -B C:\olbuild\c\llama.cpp\build-gpu -G "Ninja" \
  -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=ON \
  -DCMAKE_CUDA_ARCHITECTURES=86 \
  -DCUDAToolkit_ROOT="C:/Program Files/NVIDIA GPU Computing Toolkit/CUDA/v12.4"
cmake --build C:\olbuild\c\llama.cpp\build-gpu -j 4
```

Но! Go cgo по умолчанию использует MinGW gcc. Чтобы Go бинарь линковался с MSVC-собранной `libllama.lib`, нужно:
- либо собрать Go из исходников с `CC=cl.exe`
- либо использовать `go build -buildmode=c-archive` (создаст .lib/.a, но потребует MSVC для линковки)
- либо собрать Go через `gccgo` (который умеет c-archive)

---

## Изменённые файлы

- `C:\Ollama\ollamalegion\c\bridge\bridge.c` — 4 исправления API ✅
- `C:\olbuild\c\bridge\bridge.c` — копия для сборки ✅
- `C:\olbuild\cppworker.exe` (21 МБ) — рабочий native CPU-only бинарь ✅
- `C:\olbuild\c\llama.cpp\build\*.a` — статические библиотеки ✅
- `C:\olbuild\c\bridge\build\libollamalegion_bridge.a` ✅

---

## Что нужно сделать для завершения GPU-сборки

1. **Перезагрузить Windows** (или `Reset Docker Desktop to factory defaults` через Troubleshoot меню)
2. После восстановления Docker:
   ```bash
   cd C:\Ollama\ollamalegion
   docker build -t ollama-legion/cppworker:fix-d6-v3 \
     -f docker/cppworker/Dockerfile.gpu --target runtime .
   ```
3. Заменить образ в `deployments/docker-compose.cppworker-bundled.yml`:
   ```yaml
   image: ollama-legion/cppworker:fix-d6-v3
   ```
4. Перезапустить контейнер:
   ```bash
   docker compose -f deployments/docker-compose.cppworker-bundled.yml up -d cppworker
   ```
5. Проверить:
   ```bash
   docker logs ol-bundled-cppworker-gpu
   docker exec ol-bundled-cppworker-gpu nvidia-smi
   curl http://localhost:18091/health
   ```

Ожидаемый результат: `cppworker` стартует, регистрируется в балансировщике, GPU обнаружен (`RTX 3070, sm_86`), `/health` → 200 OK.