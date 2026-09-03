# R59.1 — autosuggest apply endpoint (2026-09-03)

**Round:** R59.1
**Status:** Design approved
**Branch:** `centurion`
**Depends on:** R59 (autosuggest endpoint, commit `f6e9d6d`)

---

## 1. Goal

R59 дал read-only endpoint `GET /api/v1/admin/cluster/autosuggest` с
suggestions. R59.1 даёт operator endpoint для **apply** suggestions.

## 2. API

### POST /api/v1/admin/cluster/autosuggest/apply

**Request body**:
```json
{
  "suggestion_ids": ["sug-1", "sug-3"]
}
```

Или с explicit detail (для testing):
```json
{
  "suggestions": [
    {"from_backend": "b1", "to_backend": "b2", "model": "m1"}
  ]
}
```

**Response (200 OK)**:
```json
{
  "applied": 1,
  "failed": 1,
  "details": [
    {
      "suggestion_id": "sug-1",
      "from_backend": "b1",
      "to_backend": "b2",
      "model": "m1",
      "status": "applied",
      "actions": ["unloaded from b1", "loaded on b2"]
    },
    {
      "suggestion_id": "sug-3",
      "from_backend": "b1",
      "to_backend": "b2",
      "model": "m2",
      "status": "failed",
      "error": "model not loaded on b1"
    }
  ],
  "errors": ["sug-3: model not loaded on b1"]
}
```

**Errors**:
- 400 — `{"error": "suggestion_ids required"}`
- 400 — `{"error": "no valid suggestions to apply"}`
- 401 — auth failure (handled by middleware)
- 502 — backend unload/load failure (per-suggestion, not whole-request)

## 3. Algorithm

```
Parse request body
Extract list of {from, to, model} tuples
For each suggestion:
  Validate cross-type (re-check from.Type == to.Type)
  Validate model loaded on `from` (use metricsMgr)
  Try unload from `from`:
    POST /api/models/unload to from's cppworker
    If fails: mark status=failed, error=...
  Try load on `to`:
    POST /api/models/load to to's cppworker
    If fails: rollback (reload on `from`)
Return summary
```

Для **минимальной** R59.1 (этот коммит):
- Только suggestio_type="move" (load suggestions — R59.2)
- Backend использует существующий GgufApi.loadModel/unloadModel
- Rollback на failure (try reload на from если unload прошёл но load на to fail)
- Partial success: applied=N, failed=M (НЕ атомарно)

## 4. TDD

```go
// internal/balancer/apply_test.go

func TestApplySuggestions_Valid(t *testing.T) {
    // Mock backends + loaded models
    // Apply 1 suggestion
    // Expect: 1 applied, 0 failed
}

func TestApplySuggestions_InvalidID(t *testing.T) {
    // Apply non-existent suggestion_id
    // Expect: 0 applied, 1 failed
}

func TestApplySuggestions_ModelNotLoaded(t *testing.T) {
    // Suggestion says "move m1 from b1" but m1 not on b1
    // Expect: 0 applied, 1 failed
}

func TestApplySuggestions_CrossType(t *testing.T) {
    // Different types (ollama vs llama_cpp)
    // Expect: rejected before load attempt
}

func TestApplySuggestions_RollbackOnLoadFailure(t *testing.T) {
    // Unload from b1 succeeds, load on b2 fails
    // Expect: model back on b1, status=rollback_done
}
```

## 5. Out of scope (R59.2+)

- **R59.2**: Auto-mode loop (continuous autosuggest with circuit-breaker)
- **R59.3**: WebUI button + visual diff modal
- **R59.4**: VRAM-aware suggestions
- **R59.5**: atomic apply (all-or-nothing)
- **R59.6**: suggestion history log

## 6. Risk

| Risk | Mitigation |
|------|------------|
| Apply while in-flight request crashes | Best-effort: rollback attempts reload on source |
| Operator applies conflicting suggestions (overlapping backends) | Process sequentially, last-one-wins; document in R59.3 |
| Slow backend response (timeout during apply) | Use existing 30s ReadTimeout; user can re-apply |

## 7. Rollout

Land commit on `centurion`. TDD-verified. Existing tests must pass.
Deploy via Docker image rebuild (requires balancer image update).
