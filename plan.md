# План реализации: Интеллектуальная загрузка моделей с распределением GPU/CPU

## Этап 1: CalculateOptimalGPULayers (backend.go)
- [ ] Добавить метод CalculateOptimalGPULayers в Backend
- [ ] Учитывает доступную VRAM + RAM
- [ ] Бинарный поиск оптимальных GPU-слоёв
- [ ] Возвращает ошибку с диагностикой

## Этап 2: Модификация checkVRAMForModel
- [ ] Вместо жёсткой проверки использовать CalculateOptimalGPULayers
- [ ] Автоматически подбирать gpuLayers если не влезает

## Этап 3: Расширение tryRamFallbackReload
- [ ] Учитывать GPU OOM тоже для fallback
- [ ] Использовать пересчёт слоёв вместо фиксированного gpuLayers

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
