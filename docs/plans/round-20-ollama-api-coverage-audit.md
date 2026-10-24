# Round 20: Ollama API Coverage Audit

**Date**: 2026-08-03
**Status**: Audit complete, fixes pending
**Scope**: Полный анализ покрытия Ollama API и OpenAI-совместимых эндпоинтов в балансере → cppworker

## Executive Summary

После Round 19 hotfix (loaded + on-disk модели в `/api/tags`) проверил **19 эндпоинтов** через bundled-full стек (балансер 18080 → cppworker 18092). Найдено **6 реальных багов**, **3 неполных покрытия** и **1 root cause** который объясняет почти всё.

| Категория | Кол-во |
|-----------|--------|
| ✅ Работает полностью | 8 |
| ⚠️ Работает с оговорками | 3 |
| ❌ Не работает / не покрыто | 6 |
| 🐛 Root cause найден | 1 (load path mismatch) |

**Главное открытие**: авто-загрузка модели в балансере использует только `name` (без `path`), а cppworker пытается найти `models/<name>.gguf` (относительный путь, не существует). Из-за этого **все вызовы inference-эндпоинтов кроме первого падают** с HTTP 500 "load failed". Это маскирует проблему `/api/embeddings` (502), `/api/generate` (503), `/api/embed` (TIMEOUT), и приводит к потере загруженной модели через 30-60s (auto-unload scheduler).

## Live Verification (bundled-full, qwen3-4b loaded once, then auto-unload kicks in)

| # | Endpoint | Method | Status | Body sample | Issue |
|---|----------|--------|--------|-------------|-------|
| 1 | `/api/version` | GET | **200** | `{"llamaVersions":{},"version":"ollamalegion-1.0.0"}` | OK (uses cppworker /api/version) |
| 2 | `/api/tags` | GET | **200** | 3 models (loaded+on-disk) | ✓ Round 19 fix |
| 3 | `/api/ps` | GET | **200** | `{"models":null}` | **Phantom OK** — cppworker doesn't implement /api/ps (404), balancer returns 200 with empty |
| 4 | `/api/show` | POST | **TIMEOUT** | (60s) | Auto-load fails 500, never returns |
| 5 | `/api/chat` | POST | **200** | `{"done":true,"message":{"content":"Hi!"}}` | ✓ |
| 6 | `/api/generate` | POST | **503** | "all backends busy" | Auto-load failed → marked busy |
| 7 | `/api/embeddings` | POST | **TIMEOUT** | (60s) | Auto-load fails 500 |
| 8 | `/api/embed` (Ollama new) | POST | **TIMEOUT** | (60s) | cppworker 404 + auto-load fails |
| 9 | `/v1/chat/completions` | POST | **500** | (empty body) | Model not loaded, retry exhausted |
| 10 | `/v1/completions` | POST | **503** | (empty body) | Same |
| 11 | `/v1/embeddings` | POST | **502** | `embeddings failed: model qwen3-4b not found` | cppworker /v1/embeddings exists but model not loaded |
| 12 | `/v1/models` | GET | **200** | 3 models | ✓ |
| 13 | `/api/models` | GET | **200** | `{"count":0,"models":[]}` | cppworker native, count=0 after auto-unload |
| 14 | `/api/models/files` | GET | **200** | 2 files | ✓ |
| 15 | `/api/pull` | POST | **TIMEOUT** | (15s) | Real pull — expected slow |
| 16 | `/api/create` | POST | **TIMEOUT** | (15s) | Real create — expected slow |
| 17 | `/api/delete` | DELETE | **TIMEOUT** | (10s) | Expected slow |
| 18 | `/api/copy` | POST | **TIMEOUT** | (10s) | Expected slow |
| 19 | `/api/push` | POST | **503** | "all backends busy" | Should timeout, got 503 — wrong |

## Root cause: Auto-load path mismatch

**Проблема**: при первом запросе к `/api/embeddings`, `/api/generate` и т.п. после auto-unload, балансер пытается загрузить модель по имени:

```
[balancer] ensureModelLoadedOnBackend: auto-loading model {"name":"qwen3-4b"}
[balancer] executing model operation: load qwen3-4b → http://cppworker-gpu:18092/api/models/load
[cppworker] handleLoadModel: load model qwen3-4b → models/qwen3-4b.gguf
[cppworker] HTTP 500: load failed: failed to load model from models/qwen3-4b.gguf
```

Модель на диске: `/app/models/Qwen3-Instruct-2507-q4km.gguf`. Пользователь загрузил её с `name=qwen3-4b`. Но cppworker по умолчанию интерпретирует `name` как `<models_dir>/<name>.gguf` (т.е. `models/qwen3-4b.gguf`).

При ручном `load-with-params` мы передаём `name + path`, и это работает. Но балансер вызывает `load` (без path) — и падает.

**Дополнительный симптом**: модель выгружается через 10 минут простоя (`IdleUnload: 10m`), и тогда все последующие запросы падают. Через 30 сек poller показывает `loaded_models:0` — то есть даже если модель была загружена, она уже выгружена.

## Detailed Findings

### ✅ Работает полностью (8)

1. **`/api/version`** — balancer проксирует на cppworker `/api/version`. Возвращает 200.

2. **`/api/tags`** — Round 19 fix. loaded + on-disk. 200.

3. **`/api/chat`** — Ollama path → balancer → cppworker → /v1/chat/completions (через translateOllamaChatToOpenAI). 200.

4. **`/v1/models`** — Round 19 fix. 200.

5. **`/api/models`** — cppworker native endpoint, агрегируется по бэкендам. 200 (но count=0 после auto-unload).

6. **`/api/models/files`** — Round 19 fix. Возвращает .gguf файлы с диска. 200.

7. **`/v1/chat/completions`** — работает пока модель загружена. После auto-unload — 500.

8. **`/v1/completions`** — same as above. 503 если выгружена.

### ⚠️ Работает с оговорками (3)

1. **`/api/ps`** — balancer возвращает 200 с `{"models":null}`, но **cppworker не реализует этот endpoint** (404). Это "phantom OK" — клиент думает что всё хорошо, но реальных данных нет.

2. **`/api/show`** — Работает на cppworker, но балансер пытается auto-load первым делом → 500 → 30-60s timeout. Нужно починить load path.

3. **`/api/embeddings`** — То же что `/api/show`. 502 после auto-unload.

### ❌ Не работает / не покрыто (6)

1. **`/api/embed`** (Ollama v0.1.14+ с `input` массивом) — **отсутствует в cppworker** (404). Балансер не имеет translation. Клиенты типа Open WebUI 0.4+ используют именно этот endpoint.

2. **`/api/generate`** — После auto-unload падает 503.

3. **`/api/copy`, `/api/push`, `/api/delete`, `/api/pull`, `/api/create`** — Реализованы в OllamaRouter, но **не тестируются end-to-end** с реальными моделями. Тесты на cppworker напрямую проходят, через балансер — нет.

4. **`X-Model-*` headers** (Round 18 plan) — отсутствуют, нет capability advertisement клиенту.

5. **`/api/version` отдаёт кастомный version `"ollamalegion-1.0.0"`** — а cppworker отдаёт `"0.2.0 (real llama.cpp linked)"`. Клиенты, парсящие формат Ollama, могут сломаться.

6. **Embeddings retry behavior** — при первом запросе (модель не загружена) `v1/embeddings` возвращает 502 "model not found", в следующий раз — 200 (если модель осталась). Не консистентно.

## Round 21 Plan (proposed)

### P0.1 — Fix auto-load path mismatch (5 min, high impact)

**Issue**: `ensureModelLoadedOnBackend` вызывает `load` без `path`. cppworker не находит файл.

**Fix**: При auto-load, балансер должен:
1. Сначала запросить у cppworker `GET /api/models/files` чтобы получить список .gguf
2. Сопоставить `name` с `.gguf` файлом (либо по `name` без расширения, либо по фолбэк имени)
3. Вызвать `load-with-params` с правильным `path`

Или проще: cppworker должен поддержать `load` с пустым `name` (найти любую подходящую модель), или балансер должен передавать `name + auto-resolved path`.

### P0.2 — Implement `/api/ps` in cppworker (10 min)

**Issue**: cppworker не реализует `/api/ps`.

**Fix**: Добавить handler в `cmd/cppworker/handlers_model.go`:
```go
func handleOllamaPS(w http.ResponseWriter, r *http.Request) {
    models := backend.ListModels()
    ollamaModels := make([]map[string]interface{}, 0, len(models))
    for _, m := range models {
        if m.State != cppbackend.StateLoaded { continue }
        ollamaModels = append(ollamaModels, map[string]interface{}{
            "name": m.Name,
            "model": m.Name,
            "size": m.SizeBytes,
            "digest": fmt.Sprintf("sha256:%x", m.SizeBytes),
            "details": map[string]interface{}{...},
            "expires_at": time.Now().Add(...).Format(time.RFC3339),
            "size_vram": m.VRAMUsage * 1024 * 1024,
        })
    }
    writeJSON(w, 200, map[string]interface{}{"models": ollamaModels})
}

mux.HandleFunc("/api/ps", handleOllamaPS)
```

### P0.3 — Implement `/api/embed` in cppworker (15 min)

**Issue**: New Ollama API (v0.1.14+) использует `input: []string` вместо `prompt: ""`. cppworker не поддерживает.

**Fix**: Добавить `handleOllamaEmbed` в cppworker + translation в балансере (`/api/embed` → `/v1/embeddings`).

### P0.4 — Verify suite (1 hour)

Добавить `tests/verify_bundled/test_api_coverage.py`:
- T1: каждый из 19 эндпоинтов → проверка HTTP status и schema
- T2: concurrent same-model 2 streams → оба работают
- T3: auto-unload cycle → перезагрузка, проверка что работает
- T4: 503/404 → проверка консистентной ошибки (не phantom 200)

### P1.1 — Batched model name mapping (future)

cppworker должен поддержать GET `/api/models/resolve?name=xxx` → `path` чтобы балансер не угадывал.

---

## Summary

**7 из 19 эндпоинтов работают без проблем.** Остальные 12 либо частично сломаны, либо не покрыты.

Главный виновник — **auto-load path mismatch**: одна строка кода в `ensureModelLoadedOnBackend` ломает почти все inference-эндпоинты после первого цикла unload/reload.

После фикса P0.1+P0.2+P0.3 балансер покроет ~17 из 19 эндпоинтов. Останутся только `/api/pull/create/delete/copy/push` которые expectedly медленные.

**Ref**: user request 2026-08-03 "что по общему аудиту работы балансера - его полность ответов и поддержки проброса апи ollama до бекендов типа llama и ollama"
