package balancer

// request_plan_steward_r493_test.go — этап 1: страж «решения о запросе живут в
// плане, а не по месту».
//
// ЗАЧЕМ СТРАЖ. Инвентаризация (docs/architecture/request-plan-r493.md) показала:
// решение о стриме уже посчитано один раз (stream_flag_r484.go), но его
// перепроверяют напрямую 14 раз через isStreamingFromBody, а клиентское окно —
// 17 раз через ExtractNumCtxFromBody мимо parsedRequest.NumCtx. Пока миграция
// идёт пошагово (план собирается, потребители переключаются по одному), нужен
// механический запрет на РОСТ таких прямых решений: иначе каждая новая правка
// добавит ещё одну копию, и этап 1 не закроется никогда.
//
// ПРАВИЛО: allowlist (файл → число прямых вызовов) может только уменьшаться.
// Уменьшили — обновите базовую строку; увеличили — тест падает и объясняет, что
// решение должно прийти из плана.
//
// ПОЧЕМУ ПО ИСХОДНИКАМ, А НЕ ПОВЕДЕНИЕМ. Прямое решение о стриме и чтение плана
// дают один и тот же ответ клиенту — разница видна только в коде. Это тот же
// класс, что сторожа серии R493 по стриму.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requestDecisionAllowlistR493 — ЗАМОРОЖЕННАЯ база: файл → прямых решений.
// Уменьшать можно (и нужно, по ходу миграции), увеличивать — нельзя.
var requestDecisionAllowlistR493 = map[string]int{
	// База 2026-11-16 (фактический подсчёт, не догадка): 14 прямых решений
	// в 10 файлах. Уменьшать можно, увеличивать — нельзя.
	"admission_queue.go":             1,
	"image_tool_loop.go":             1,
	"image_tool_loop_ollama.go":      1,
	"llamacpp_handlers_inference.go": 3,
	"llamacpp_translate_req.go":      1,
	"llamacpp_transport.go":          1,
	"nctx_reload_handlers.go":        3,
	"preflight_stream_dialog.go":     1,
	"primary_model_policy.go":        1,
	"proxy_request.go":               1,
}

func TestR493_RequestDecisionsOnlyInPlan(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	seen := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		n := strings.Count(string(src), "isStreamingFromBody(")
		if n > 0 {
			seen[f] = n
		}
	}

	for f, got := range seen {
		allowed, ok := requestDecisionAllowlistR493[f]
		if !ok {
			t.Errorf("%s: %d прямых решений о стриме (isStreamingFromBody) — файла нет в allowlist. "+
				"Решение обязано приходить из requestPlanR493 (requestPlanOfR493); "+
				"если это временный шаг миграции — добавьте файл в базу осознанно", f, got)
			continue
		}
		if got > allowed {
			t.Errorf("%s: прямых решений %d, разрешено %d — allowlist может только уменьшаться. "+
				"Возьмите решение из плана, а не из тела", f, got, allowed)
		}
	}
	// Уменьшение — повод обновить базу, но не ошибка: фиксируем как факт.
	for f, allowed := range requestDecisionAllowlistR493 {
		if got, ok := seen[f]; !ok {
			t.Logf("baseline уменьшился: %s больше не решает напрямую (было %d) — "+
				"обновите requestDecisionAllowlistR493", f, allowed)
		} else if got < allowed {
			t.Logf("baseline уменьшился: %s (%d → %d) — обновите requestDecisionAllowlistR493", f, allowed, got)
		}
	}
}

// TestR493_PlanReachesContextForEveryParsedRequest — план собирается в
// parseRequestBody и обязан доехать до контекста: потребители handler-путей
// читают его через requestPlanOfR493, не переразбирая тело.
func TestR493_PlanReachesContextForEveryParsedRequest(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	if !strings.Contains(string(src), "withRequestPlanR493(r, parsed.Plan)") {
		t.Fatal("в ServeHTTP план не кладётся в контекст: handler-пути не смогут взять " +
			"решение без повторного разбора тела")
	}
}
