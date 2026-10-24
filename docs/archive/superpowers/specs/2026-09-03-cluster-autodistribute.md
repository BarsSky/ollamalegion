# Phase 4: Cluster AutoDistribute (R59) design

**Date:** 2026-09-03
**Author:** Mavis (Phase 4 from 2026-09-03 user roadmap)
**Status:** Design approved (R59 implementation)
**Branch:** `centurion`
**Depends on:** All R56-R58.3 work
**Reference:** user pain — «попытка автоматизации и упрощения для работы пользователя провалилась. Все запросы на автоматическое распредление нагрузки на машину (загрузка в RAM и VRAM) балансером завершалась ошибкой»

---

## 1. Background

Пользователь хочет чтобы balancer САМ распределял модели между бэкендами
на основе:
- **VRAM/RAM usage** каждого бэкенда
- **Active requests** per backend
- **Model-affinity** (модель уже загружена → не выгружать)
- **Backend weight** (operator-defined)

Раньше эта функциональность либо не существовала, либо была
с нестабильным поведением. Нужно сделать простой, детерминированный
алгоритм с 3 режимами:
- `manual` — оператор сам решает
- `suggest` — balancer показывает recommendation, оператор apply
- `auto` — balancer сам apply (с circuit-breaker)

## 2. Goal

Реализовать 3 уровня автоматизации (от безопасного к рискованному):
1. **`GET /api/v1/admin/cluster/autosuggest`** — возвращает текущее состояние +
   рекомендации (model moves, weight adjustments) БЕЗ применения.
2. **`POST /api/v1/admin/cluster/autosuggest/apply`** — apply suggestions
   (operator reviews + confirms).
3. **`POST /api/v1/admin/cluster/auto-tuner` (already exists in R54.8)** —
   продолжает работать, не трогаем.

R59 scope: **только #1 (autosuggest endpoint + UI button)**. #2 и auto-mode
(continuous autosuggest loop) — следующие раунды.

## 3. Algorithm (autosuggest, минимальная версия)

```
Input: cluster state (all backends + their status, activeRequests, models)
Output: array of suggestions (JSON)

Step 1. Build backend utilization map:
   for each backend b:
     busyScore = b.ActiveRequests / b.MaxConcurrentReqs  // 0..1+
     modelCount = loadedModels[b].length / b.MaxModels     // 0..1+
     unhealthy = b.Status != healthy

Step 2. Identify overloaded backends (busyScore > 0.8 OR modelCount > 0.9)
         and underloaded backends (busyScore < 0.3 AND modelCount < 0.5)

Step 3. For each overloaded backend:
     for each loaded model m on it:
       if m is "system model" (small, used for many): skip
       find underloaded backend b2 with:
         - Same Type (ollama/llama_cpp) — required (cross-type moves break load format)
         - Status == healthy
         - Different from b
         - Not already loaded (model-affinity bonus: prefer keeping on same backend)
       if found: suggest move (b, m) → (b2, m)

Step 4. For each underloaded backend with 0 models:
     if cluster has unloaded models (in pending queue):
       suggest load: load <first-pending-model> on b
```

Возвращаем массив suggestions с метаданными:
- `from` / `to` (backend IDs)
- `model` (name)
- `reason` (text)
- `priority` (1=high, 2=medium, 3=low)
- `estimated_vram_savings_mb` (if available)

## 4. API contract

### GET /api/v1/admin/cluster/autosuggest

**Response (200)**:
```json
{
  "timestamp": "2026-09-03T13:00:00Z",
  "backends": [
    {
      "id": "ollama-1",
      "status": "healthy",
      "active_requests": 5,
      "max_concurrent": 10,
      "busy_score": 0.5,
      "loaded_models": ["qwen3-8b"],
      "max_models": 4,
      "model_count_score": 0.25,
      "is_overloaded": false,
      "is_underloaded": true,
      "unhealthy": false
    },
    ...
  ],
  "suggestions": [
    {
      "id": "sug-1",
      "type": "move",
      "from_backend": "ollama-2",
      "to_backend": "ollama-3",
      "model": "gemma-4-9b",
      "reason": "ollama-2 has busy_score 0.95; ollama-3 underloaded (busy_score 0.1)",
      "priority": 1
    },
    ...
  ],
  "summary": {
    "total_backends": 3,
    "overloaded_count": 1,
    "underloaded_count": 1,
    "suggestion_count": 1
  }
}
```

### POST /api/v1/admin/cluster/autosuggest/apply (R59.1 — out of scope для этого раунда)

Apply specific suggestions. Body: `{"suggestion_ids": ["sug-1", "sug-2"]}`.
Operator must explicitly approve. Returns `{"applied": 2, "failed": 0, "details": [...]}`.

## 5. TDD / acceptance

- Unit test `TestComputeSuggestions_NoOverload` — 2 backends, 0 loaded, 0 suggestions
- Unit test `TestComputeSuggestions_OneOverloaded` — 1 backend with 8/10 active, suggests move
- Unit test `TestComputeSuggestions_DifferentType` — cppworker on b1, ollama on b2 — НЕ предлагает move (cross-type)
- Unit test `TestComputeSuggestions_UnhealthySkipped` — unhealthy backend skipped
- Integration: `GET /api/v1/admin/cluster/autosuggest` returns 200 with valid JSON

## 6. Out of scope (future rounds)

- **R59.1**: POST `/apply` endpoint (operator approval flow)
- **R59.2**: Auto-mode (continuous autosuggest loop with circuit-breaker)
- **R59.3**: WebUI — "Suggest Distribution" button + visual diff (current vs suggested)
- **R59.4**: VRAM-aware suggestions (using `llamaMetrics` GPU memory data, R58)
- **R59.5**: Per-model load predictions (based on `llamaMetrics` request rate)

## 7. UI for R59 (minimum)

- Add button "Suggest Distribution" in WebUI Settings page
- Click → fetches /autosuggest → displays modal with table of suggestions
- "Apply" button (placeholder, не делает ничего до R59.1)
- Close button
- Loading state during fetch

UI will be added in R59.3 (this round focuses на backend + tests).

## 8. Risk

| Risk | Mitigation |
|------|------------|
| Suggestion engine даёт sub-optimal advice (operator applies blindly) | TDD cases + clear "reason" field + R59.3 visual diff |
| Cross-type move breaks load | Algorithm explicitly checks `b.Type == b2.Type` |
| Apply endpoint abused (auto-apply loop) | R59.2 circuit-breaker (3 fails → manual mode) |
| Slow endpoint on large clusters (50+ backends) | O(N) algorithm, O(1) per backend — fast enough |
