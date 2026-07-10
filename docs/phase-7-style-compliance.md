# Phase 7 — Style Compliance & Docs Sync (2026-07-10)

> **Подготовлено:** 2026-07-10 22:20 MSK
> **Branch:** `centurion` (от текущего HEAD `0b2bbbc`)
> **Период:** 1 сессия (~2-3 ч)
> **Приоритет:** 🟡 P2 (low-priority cleanup, но regression-важно)
> **Скоуп:** рефакторинг + мелкие исправления из `docs/issues/2026-06-29-big-model-20gb-fix.md` секция "Known Issues / TODO"

---

## Контекст

В сессиях 17 (CSS audit) и F.4 (i18n balance) были закрыты крупные style-проблемы:
- 178 → 123 hardcoded colors (-31%)
- 1019 = 1019 i18n keys (0 diff)

Однако в `docs/issues/2026-06-29-big-model-20gb-fix.md` секция "Known Issues / TODO" остались
2 нерешённых пункта + 1 пункт с устаревшим статусом:

| # | Известная проблема | Фактический статус |
|---|---|---|
| 1 | `POST /api/v1/cppworker/reset-reload-counter` is still TODO | ✅ **DONE** (handlers_reset_reload.go + 5 тестов) — doc устарел |
| 2 | i18n helper scripts (fix_i18n_syntax.py, etc.) | ✅ kept as safety net — by design |
| 3 | Em-dash compliance: 7 violations in CSS comments + 2 in i18n headers | ❌ **TODO** |
| 4 | rgba() compliance: ~120 rgba() в CSS | ⚠️ partial — can show pattern with 2-3 examples |
| 5 | Ollama-only messages возвращают HTTP 501 | ✅ by design |

**Production-ready план** P.1 (rpc_coordinator) / P.2 (virtual_router) — multi-day, отдельные сессии.

---

## Цель

Закрыть **em-dash + rgba + doc-sync** как чистый refactor-коммит с regression-тестом, чтобы:

1. **Все em-dash в CSS/JS коде заменены на `-`** (механическая правка 9 мест)
2. **Шаблон `color-mix()`** применён к 2-3 конкретным rgba() в `themes.css` как reference (с комментарием)
3. **Regression-тест** `lint_css_i18n_test.go` падает, если кто-то снова вставит em-dash в webui
4. **Issue doc обновлён** (TODO-3 статус с "still TODO" → "✅ DONE 2026-07-06")

---

## Acceptance criteria

1. `Select-String em-dash webui/css/*.css webui/js/i18n/*.js` → 0 matches.
2. `go test -tags llama_stub -count=1 ./internal/...` — все проходят (включая новый `TestLintCSSAndI18nNoEmDash`).
3. `node -c webui/js/i18n/{en,ru}.js` — exit 0.
4. CSS всё ещё валиден (Chrome DevTools "Computed Styles" на любой странице не показывает parse errors).
5. 2-3 примера `color-mix()` в `themes.css` с inline-комментарием `# color-mix(in srgb, var(--accent) X%, transparent)` вместо `rgba(...)`.
6. `docs/issues/2026-06-29-big-model-20gb-fix.md` секция "Known Issues / TODO" обновлена: #1 = ✅ DONE (с датой + commit ref), #3 = ✅ FIXED (этот PR).

---

## Файлы

### Изменяются
- `webui/css/components.css` (строки 78, 80, 90, 91) — 4 em-dash → `-`
- `webui/css/data.css` (строка 170) — 1 em-dash → `-`
- `webui/css/layout.css` (строки 81, 83) — 2 em-dash → `-`
- `webui/js/i18n/en.js` (строка 1) — 1 em-dash → `-`
- `webui/js/i18n/ru.js` (строка 1) — 1 em-dash → `-`
- `webui/css/themes.css` — 2-3 примера `color-mix()` + комментарий
- `docs/issues/2026-06-29-big-model-20gb-fix.md` — Known Issues sync

### Создаются
- `internal/api/lint_css_i18n_test.go` (новый, ~80 LOC) — Go тест сканирует webui/css + webui/js/i18n на em-dash
- `docs/phase-7-style-compliance.md` (новый) — описание Phase 7 (этот файл)

### Тесты
- `internal/api/lint_css_i18n_test.go:TestLintCSSAndI18nNoEmDash` — fail при em-dash в webui

---

## Оценка

- Phase 7.1 em-dash fix: 30 мин (механические правки + CSS parse check)
- Phase 7.2 color-mix examples: 30 мин (2-3 примера + комментарий)
- Phase 7.3 regression test: 45 мин (новый Go тест)
- Phase 7.4 docs sync: 15 мин

**Итого: ~2-2.5 ч.**

---

## План работы (чеклист)

### Под-сессия 7.1 (30 мин): em-dash fix
- [ ] Заменить em-dash на `-` в 4 местах `webui/css/components.css`
- [ ] Заменить em-dash на `-` в 1 месте `webui/css/data.css`
- [ ] Заменить em-dash на `-` в 2 местах `webui/css/layout.css`
- [ ] Заменить em-dash на `-` в строках 1 файлов `webui/js/i18n/{en,ru}.js`
- [ ] Verify: `Select-String em-dash webui/css/*.css webui/js/i18n/*.js` → 0
- [ ] Verify: `node -c webui/js/i18n/{en,ru}.js` exit 0
- [ ] Verify: Chrome browser reload (manual) — pages parse OK

### Под-сессия 7.2 (30 мин): color-mix examples
- [ ] Прочитать `webui/css/themes.css` полностью (~100 строк)
- [ ] Найти 2-3 rgba() с типичными use cases (badge bg, shadow, gradient)
- [ ] Заменить на `color-mix(in srgb, var(--token) X%, transparent)` (или `color-mix(in srgb, var(--token1), var(--token2) X%)`)
- [ ] Добавить inline-комментарий с правилом: "Use color-mix() when blending theme tokens with alpha; reserve rgba() для чистых цветов с фиксированной alpha (не зависящих от темы)"
- [ ] Verify: Chrome browser reload — визуально идентично

### Под-сессия 7.3 (45 мин): regression test
- [ ] Создать `internal/api/lint_css_i18n_test.go`
- [ ] Test скан `webui/css/*.css` на em-dash (читает файлы, regex)
- [ ] Test скан `webui/js/i18n/{en,ru}.js` line 1 (header comment) на em-dash
- [ ] `t.Errorf` с конкретным file:line при нарушении
- [ ] `t.Skip` если webui директория не найдена (для CI на Linux runner без webui)
- [ ] Verify: `go test -tags llama_stub ./internal/api/ -run TestLint` PASS
- [ ] Verify: добавить временно em-dash → test FAIL (regression check)

### Под-сессия 7.4 (15 мин): docs sync
- [ ] Открыть `docs/issues/2026-06-29-big-model-20gb-fix.md` секция "Known Issues / TODO"
- [ ] Изменить #1: "still TODO" → "✅ DONE 2026-07-06 (commit `0e1f3a2` — handlers_reset_reload.go + 5 тестов)"
- [ ] Изменить #3: "treated as low priority" → "✅ FIXED 2026-07-10 (Phase 7.1, commit `XXX`)"

### Commit + push
- [ ] `git add webui/css/ webui/js/i18n/ internal/api/lint_css_i18n_test.go docs/issues/`
- [ ] `git commit -m "refactor: Phase 7 — replace em-dash + color-mix pattern + regression test"`
- [ ] (push не делаем без явного запроса)

---

## Зависимости

- **7.1** — независим (механические правки)
- **7.2** — независим от 7.1
- **7.3** — независим (новый файл)
- **7.4** — независим

Все 4 под-сессии можно делать последовательно или параллельно.

---

## Связанные документы

- [`docs/issues/2026-06-29-big-model-20gb-fix.md`](../docs/issues/2026-06-29-big-model-20gb-fix.md) — источник Known Issues / TODO
- [`docs/fixes/2026-07-06-bug13-nkvh-from-cbridge.md`](../docs/fixes/2026-07-06-bug13-nkvh-from-cbridge.md) — side findings #1+#2 уже исправлены
- [`plans/2026-q3-production-ready-plan.md`](../plans/2026-q3-production-ready-plan.md) — следующий этап (P.1 + P.2)
- [`.clinerules`](../.clinerules) — стиль кода (русские комментарии)

---

**Подготовлено:** 2026-07-10 22:20 MSK (Phase 7 — Style Compliance & Docs Sync)
**После Phase 7:** готово к Phase 8 (P.1 rpc_coordinator, 5-8 дней).
