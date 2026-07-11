# Phase 7 — Style Compliance & Docs Sync (2026-07-10)

> **Prepared:** 2026-07-10 22:20 MSK
> **Branch:** `centurion` (from current HEAD `0b2bbbc`)
> **Period:** 1 session (~2-3 h)
> **Priority:** 🟡 P2 (low-priority cleanup, but regression-important)
> **Scope:** refactoring + minor fixes from `docs/issues/2026-06-29-big-model-20gb-fix.md` "Known Issues / TODO" section

---

## Context

In sessions 17 (CSS audit) and F.4 (i18n balance) the major style issues were closed:
- 178 → 123 hardcoded colors (-31%)
- 1019 = 1019 i18n keys (0 diff)

However, in `docs/issues/2026-06-29-big-model-20gb-fix.md` "Known Issues / TODO" section, 2 unresolved items + 1 item with an outdated status remained:

| # | Known issue | Actual status |
|---|---|---|
| 1 | `POST /api/v1/cppworker/reset-reload-counter` is still TODO | ✅ **DONE** (handlers_reset_reload.go + 5 tests) — doc is outdated |
| 2 | i18n helper scripts (fix_i18n_syntax.py, etc.) | ✅ kept as safety net — by design |
| 3 | Em-dash compliance: 7 violations in CSS comments + 2 in i18n headers | ❌ **TODO** |
| 4 | rgba() compliance: ~120 rgba() in CSS | ⚠️ partial — can show pattern with 2-3 examples |
| 5 | Ollama-only messages return HTTP 501 | ✅ by design |

**Production-ready plan** P.1 (rpc_coordinator) / P.2 (virtual_router) — multi-day, separate sessions.

---

## Goal

Close **em-dash + rgba + doc-sync** as a clean refactor commit with a regression test, so that:

1. **All em-dashes in CSS/JS code are replaced with `-`** (mechanical fix at 9 places)
2. **`color-mix()` pattern** applied to 2-3 specific rgba() in `themes.css` as a reference (with a comment)
3. **Regression test** `lint_css_i18n_test.go` fails if someone re-introduces an em-dash in webui
4. **Issue doc updated** (TODO-3 status from "still TODO" → "✅ DONE 2026-07-06")

---

## Acceptance criteria

1. `Select-String em-dash webui/css/*.css webui/js/i18n/*.js` → 0 matches.
2. `go test -tags llama_stub -count=1 ./internal/...` — all pass (including the new `TestLintCSSAndI18nNoEmDash`).
3. `node -c webui/js/i18n/{en,ru}.js` — exit 0.
4. CSS is still valid (Chrome DevTools "Computed Styles" on any page does not show parse errors).
5. 2-3 `color-mix()` examples in `themes.css` with inline comment `# color-mix(in srgb, var(--accent) X%, transparent)` instead of `rgba(...)`.
6. `docs/issues/2026-06-29-big-model-20gb-fix.md` "Known Issues / TODO" section updated: #1 = ✅ DONE (with date + commit ref), #3 = ✅ FIXED (this PR).

---

## Files

### Modified
- `webui/css/components.css` (lines 78, 80, 90, 91) — 4 em-dashes → `-`
- `webui/css/data.css` (line 170) — 1 em-dash → `-`
- `webui/css/layout.css` (lines 81, 83) — 2 em-dashes → `-`
- `webui/js/i18n/en.js` (line 1) — 1 em-dash → `-`
- `webui/js/i18n/ru.js` (line 1) — 1 em-dash → `-`
- `webui/css/themes.css` — 2-3 `color-mix()` examples + comment
- `docs/issues/2026-06-29-big-model-20gb-fix.md` — Known Issues sync

### Created
- `internal/api/lint_css_i18n_test.go` (new, ~80 LOC) — Go test scans webui/css + webui/js/i18n for em-dash
- `docs/phase-7-style-compliance.md` (new) — Phase 7 description (this file)

### Tests
- `internal/api/lint_css_i18n_test.go:TestLintCSSAndI18nNoEmDash` — fails on em-dash in webui

---

## Estimate

- Phase 7.1 em-dash fix: 30 min (mechanical edits + CSS parse check)
- Phase 7.2 color-mix examples: 30 min (2-3 examples + comment)
- Phase 7.3 regression test: 45 min (new Go test)
- Phase 7.4 docs sync: 15 min

**Total: ~2-2.5 h.**

---

## Work plan (checklist)

### Sub-session 7.1 (30 min): em-dash fix
- [ ] Replace em-dash with `-` in 4 places in `webui/css/components.css`
- [ ] Replace em-dash with `-` in 1 place in `webui/css/data.css`
- [ ] Replace em-dash with `-` in 2 places in `webui/css/layout.css`
- [ ] Replace em-dash with `-` in line 1 of `webui/js/i18n/{en,ru}.js`
- [ ] Verify: `Select-String em-dash webui/css/*.css webui/js/i18n/*.js` → 0
- [ ] Verify: `node -c webui/js/i18n/{en,ru}.js` exit 0
- [ ] Verify: Chrome browser reload (manual) — pages parse OK

### Sub-session 7.2 (30 min): color-mix examples
- [ ] Read `webui/css/themes.css` fully (~100 lines)
- [ ] Find 2-3 rgba() with typical use cases (badge bg, shadow, gradient)
- [ ] Replace with `color-mix(in srgb, var(--token) X%, transparent)` (or `color-mix(in srgb, var(--token1), var(--token2) X%)`)
- [ ] Add inline comment with the rule: "Use color-mix() when blending theme tokens with alpha; reserve rgba() for pure colors with fixed alpha (not theme-dependent)"
- [ ] Verify: Chrome browser reload — visually identical

### Sub-session 7.3 (45 min): regression test
- [ ] Create `internal/api/lint_css_i18n_test.go`
- [ ] Test scans `webui/css/*.css` for em-dash (reads files, regex)
- [ ] Test scans `webui/js/i18n/{en,ru}.js` line 1 (header comment) for em-dash
- [ ] `t.Errorf` with specific file:line on violation
- [ ] `t.Skip` if webui directory not found (for CI on a Linux runner without webui)
- [ ] Verify: `go test -tags llama_stub ./internal/api/ -run TestLint` PASS
- [ ] Verify: temporarily add an em-dash → test FAILS (regression check)

### Sub-session 7.4 (15 min): docs sync
- [ ] Open `docs/issues/2026-06-29-big-model-20gb-fix.md` "Known Issues / TODO" section
- [ ] Change #1: "still TODO" → "✅ DONE 2026-07-06 (commit `0e1f3a2` — handlers_reset_reload.go + 5 tests)"
- [ ] Change #3: "treated as low priority" → "✅ FIXED 2026-07-10 (Phase 7.1, commit `XXX`)"

### Commit + push
- [ ] `git add webui/css/ webui/js/i18n/ internal/api/lint_css_i18n_test.go docs/issues/`
- [ ] `git commit -m "refactor: Phase 7 — replace em-dash + color-mix pattern + regression test"`
- [ ] (do not push without an explicit request)

---

## Dependencies

- **7.1** — independent (mechanical edits)
- **7.2** — independent of 7.1
- **7.3** — independent (new file)
- **7.4** — independent

All 4 sub-sessions can be done sequentially or in parallel.

---

## Related documents

- [`docs/issues/2026-06-29-big-model-20gb-fix.md`](../docs/issues/2026-06-29-big-model-20gb-fix.md) — source of Known Issues / TODO
- [`docs/fixes/2026-07-06-bug13-nkvh-from-cbridge.md`](../docs/fixes/2026-07-06-bug13-nkvh-from-cbridge.md) — side findings #1+#2 already fixed
- [`plans/2026-q3-production-ready-plan.md`](../plans/2026-q3-production-ready-plan.md) — next stage (P.1 + P.2)
- [`.clinerules`](../.clinerules) — code style (Russian comments)

---

**Prepared:** 2026-07-10 22:20 MSK (Phase 7 — Style Compliance & Docs Sync)
**After Phase 7:** ready for Phase 8 (P.1 rpc_coordinator, 5-8 days).
