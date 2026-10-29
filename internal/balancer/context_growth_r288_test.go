// context_growth_r288_test.go — R288 (2026-10-29): запрос, которому нужно
// большее окно, получает понятный отказ вместо 900-секундной тишины в
// admission-очереди.
//
// ЖИВОЙ ДЕФЕКТ (стенд r282): модель загружена с окном 8192, клиент попросил
// num_ctx=32768 во время чужого стрима. Рост окна был отложен guard'ом R271,
// и запрос встал в очередь с wait_max_sec=900 — для клиента это «балансер
// ничего не отвечает», хотя решение уже было известно.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR288_PromptNeedsMoreContext — таблица правила «влезает ли промпт в слот».
func TestR288_PromptNeedsMoreContext(t *testing.T) {
	cases := []struct {
		name      string
		estimated int
		budget    int
		loaded    int
		want      bool
	}{
		{"влезает с запасом", 1000, 4096, 8192, false},
		{"ровно в притык — влезает", 4096, 4096, 8192, false},
		{"не влезает на токен", 4097, 4096, 8192, true},
		{"ЖИВОЙ СЛУЧАЙ: 32768 при окне 8192", 20000, 8192, 8192, true},
		{"окно неизвестно — не отказываем", 20000, 8192, 0, false},
		{"оценки нет — не отказываем", 0, 8192, 8192, false},
		{"отрицательный бюджет не ломает правило", 9000, -10, 8192, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := promptNeedsMoreContext(c.estimated, c.budget, c.loaded); got != c.want {
				t.Fatalf("promptNeedsMoreContext(%d,%d,%d) = %v, want %v",
					c.estimated, c.budget, c.loaded, got, c.want)
			}
		})
	}
}

// TestR288_DeferredResponseIsHonest — ответ несёт причину, числа и Retry-After.
func TestR288_DeferredResponseIsHonest(t *testing.T) {
	rec := httptest.NewRecorder()
	writeContextGrowthDeferred(rec, "gemma-4-E4B-it-Q4_K_M", 24096, 8192, 1, true)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код = %d, want 503 (запрос корректный, окно мало)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("нет Retry-After: клиент не знает, когда повторять")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", ct)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if body["reason"] != "context_growth_pending" {
		t.Fatalf("reason = %v, want context_growth_pending (стабильное имя для клиента)", body["reason"])
	}
	if body["requiredTokens"] != float64(24096) || body["loadedWindowTokens"] != float64(8192) {
		t.Fatalf("в ответе должны быть ЧИСЛА (сколько нужно и сколько есть): %v", body)
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Fatal("нет подсказки, что делать")
	}
	// R299: два РАЗНЫХ факта в теле. «active streams=0» при идущей перезагрузке —
	// не противоречие, а честная картина; клиенту нужны оба поля.
	if body["activeStreams"] != float64(1) {
		t.Fatalf("activeStreams = %v, want 1 (живой счётчик, не «по умолчанию 0»)", body["activeStreams"])
	}
	if body["reloadPending"] != true {
		t.Fatalf("reloadPending = %v, want true (перезагрузка идёт — причина ожидания)", body["reloadPending"])
	}
	if body["deferredReason"] != "context_growth_pending" || body["deferred"] != true {
		t.Fatalf("нет единого формата «отложено» (R291): %v", body)
	}
}

// TestR288_NoBlockWithoutActiveStreams — если стримов нет, обычный путь (и
// очередь) должен работать как раньше: preflight/reload может успеть.
func TestR288_NoBlockWithoutActiveStreams(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8192}
	if blocked, _, _ := p.contextGrowthBlocker("bk-1", "m", meta); blocked {
		t.Fatal("без активных стримов отказывать нельзя: рост окна ещё возможен")
	}
}

// TestR288_BlockWhenStreamOccupiesBackend — при живом стриме отказываем сразу,
// а не ставим в очередь на 900 с.
func TestR288_BlockWhenStreamOccupiesBackend(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	// Кладём «загруженное окно» так, как его видит loadedPerSeqNCtx.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: 8192, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()

	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8192}

	// Без стрима — не блокируем (рост ещё может случиться).
	if blocked, _, _ := p.contextGrowthBlocker("bk-1", "m", meta); blocked {
		t.Fatal("без стрима блокировать нельзя")
	}

	// С активным стримом на этом бэкенде — блокируем и называем числа.
	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()
	blocked, needed, loaded := p.contextGrowthBlocker("bk-1", "m", meta)
	if !blocked {
		t.Fatal("под живым стримом рост окна отложен — очередь бессмысленна")
	}
	if needed != 28192 && needed != 20000+8192 {
		t.Fatalf("needed = %d, want %d", needed, 20000+8192)
	}
	if loaded != 8192 {
		t.Fatalf("loaded = %d, want 8192", loaded)
	}
}
