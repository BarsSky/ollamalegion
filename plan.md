# План реализации: Интеллектуальная загрузка моделей с распределением GPU/CPU

> **Дата обновления:** 2026-06-28
> **Статус:** 🔶 **ЧАСТИЧНО РЕАЛИЗОВАН** — см. актуальный статус в [plans/2026-q3-roadmap.md](2026-q3-roadmap.md) и [plans/README.md](README.md)
>
> **NB:** Этот документ — исторический план от 2026-05-13. Этапы 1-3 были **заменены** на трёхступенчатый каскад `calculateLazyLoadOpts` (`cmd/cppworker/lazyload_calc.go`, Session 2), а этап 4 — полностью реализован в Session 4 (см. CHANGELOG.md, `[Unreleased — 2026-06-26]`). Новые планы на следующие этапы — в [Session F](plans/2026-q3-session-f-quick-wins.md) и [production-ready](plans/2026-q3-production-ready-plan.md).

## Этап 1: CalculateOptimalGPULayers (backend.go)
- [ ] Добавить метод CalculateOptimalGPULayers в Backend
- [ ] Учитывает доступную VRAM + RAM
- [ ] Бинарный поиск оптимальных GPU-слоёв
- [ ] Возвращает ошибку с диагностикой

**Статус (2026-06-28):** 🟡 Заменён на `calculateLazyLoadOpts` в `cmd/cppworker/lazyload_calc.go` (Session 2, 17 unit-тестов).
Stage 1 (`exact_fit`): все weights + KV-cache влезают в VRAM → opts как есть.
Stage 2 (`partial_offload`): уменьшаем `gpu_layers`, остальные веса через `mmap` в RAM.
Stage 3 (`reduced_nctx`): `gpu_layers=0` (CPU-only через mmap) + n_ctx = `maxViableNCtx`.

## Этап 2: Модификация checkVRAMForModel
- [ ] Вместо жёсткой проверки использовать CalculateOptimalGPULayers
- [ ] Автоматически подбирать gpuLayers если не влезает

**Статус (2026-06-28):** 🟡 AutoTuneNCtx покрывает 90% сценариев через трёхступенчатый каскад. Production-конфиг `config.bundled.json:114` использует `numCtx=32768` как default, `LB_NCTX_RELOAD_MAX_N_CTX=65536` для auto-reload.

## Этап 3: Расширение tryRamFallbackReload
- [ ] Учитывать GPU OOM тоже для fallback
- [ ] Использовать пересчёт слоёв вместо фиксированного gpuLayers

**Статус (2026-06-28):** ✅ **DONE**. В `cmd/cppworker/inference.go:491` реализован 3-stage каскад (VRAM → partial offload → CPU-only + max_viable). Каскад активируется автоматически при `ErrNCtxNeedsReload`. Production-флаг `CPPWORKER_RAM_FALLBACK_N_CTX=true` (по умолчанию false, opt-in).

## Этап 4: Context-aware загрузка через API
- [x] Новый endpoint /api/models/load-with-params — **DONE 2026-06-26 (Session 4, P-1)**
  - `internal/cppbackend/backend.go` — `LoadModelOpts` расширен полями `NThreads`, `Parallel`, `KVCacheType`, `SplitMode`, `OverrideTensor`.
  - `cmd/cppworker/types.go` — структура `loadWithParamsRequest` со всеми 15 полями (базовые + расширенные) в виде `*int`/`*string` указателей.
  - `cmd/cppworker/handlers_model.go:handleLoadWithParams` (~150 LOC) — POST, JSON, валидация name, race-condition handling (TryLockLoad/WaitForLoad), LoadModelWithOpts, JSON response с `appliedOpts`.
  - `cmd/cppworker/router.go` — `mux.HandleFunc("/api/models/load-with-params", handleLoadWithParams)`.
  - 11 unit-тестов в `cmd/cppworker/handlers_model_loadwithparams_test.go` — все зелёные.
  - Подробности: `CHANGELOG.md` → `[Unreleased — 2026-06-26] → Features (Session 4 — P-1)`.
- [x] Прокси через балансировщик — **DONE 2026-06-26**
  - Generic proxy `internal/api/gguf_backend_proxy_handlers.go` уже корректно маршрутизирует все пути под `/api/v1/gguf/backends/{id}/proxy/`. Endpoint документация добавлена в комментарий handler'а.
  - Пример: `POST /api/v1/gguf/backends/cppworker-gpu-bundled/proxy/api/models/load-with-params` (требуется `X-API-Token`).

**Расширения после Session 4:**
- [x] Session 16 (2026-06-27) — Per-Model Profiles: добавлены поля `parallel` (n_parallel в llama.cpp) и `kv_cache_type` (f16/q8_0/q4_0) с полной цепочкой backend → C-bridge → cppworker → WebUI. 4 новых теста PASS.
  - `pkg/types/balancing.go` — `Parallel int`, `KVCacheType string` в `LlamaCppModelProfile`.
  - `internal/cppbackend/backend.go` — `LoadModelOpts.KVCacheType` (string вместо int).
  - `c/bridge/bridge.{h,c,go,stub.go}` — `ModelConfig.n_parallel`, `ModelConfig.kv_cache_type`, switch на `ctx_params.type_k/type_v`.
  - `cmd/cppworker/handlers_model.go` — `reloadModelRequest.KVCacheType *string`, валидация через `isValidKVCacheType`.
  - `webui/js/modules/cppworker-params.js` — 2 поля в advanced секции wizard.

---

## Связанные документы

- [plans/README.md](plans/README.md) — главный roadmap, статус R-1..R-7.
- [plans/2026-q3-roadmap.md](plans/2026-q3-roadmap.md) — Q3 2026 roadmap (Models tab gaps ЗАКРЫТ, остались UI/UX + production-ready).
- [plans/2026-q3-session-f-quick-wins.md](plans/2026-q3-session-f-quick-wins.md) — следующий этап (UI/UX quick wins, июль 2026).
- [plans/2026-q3-production-ready-plan.md](plans/2026-q3-production-ready-plan.md) — production-ready режимы (август-сентябрь 2026, перед 1.0 release).
- [docs/cppworker-model-params.md](docs/cppworker-model-params.md) — Per-Model Profiles + load-with-params endpoint.
- [docs/troubleshooting.md](docs/troubleshooting.md) — n_ctx reload debug runbook.

---

**Этот файл оставлен для истории** — для актуального состояния см. [plans/README.md](plans/README.md) и связанные roadmap-документы.