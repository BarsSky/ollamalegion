//go:build llama_stub

// load_failure_events_r697_test.go — R697 (2026-11-24).
//
// ПРЕДМЕТ. Уведомление оператору — единственная поверхность, где WebUI показывает
// причину провала загрузки словами (bell-меню рисует ev.message как есть,
// webui/js/modules/notifications.js). Живой случай: модель Inkling-2-Mini
// (general.architecture="inkling2") не поднялась, и в панель приходило «файл модели
// не принят llama.cpp (формат/архитектура)» — то есть намёк проверять файл, хотя
// лечится обновлением сабмодуля c/llama.cpp и образом воркера.
//
// Здесь закреплено: текст называет архитектуру, сборку и действие; подсказка едет из
// диагностики cppworker, а не выдумывается балансером.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// modelUnsupportedFailureR697 — запись провала в том виде, в каком её строит cppworker
// (/api/models → load_failure: reason + diagnostics).
func modelUnsupportedFailureR697(model string) *types.LoadFailureInfo {
	return &types.LoadFailureInfo{
		Model:    model,
		Reason:   "model_unsupported",
		Error:    "load model " + model + ": failed to load model from /app/models/" + model + ".gguf",
		Severity: "error",
		Diagnostics: map[string]interface{}{
			"arch":                 "inkling2",
			"llama_build":          "b1b770d90",
			"arch_supported_since": "b12642",
			"hint":                 "обновите сабмодуль c/llama.cpp до коммита, где эта архитектура есть, и пересоберите образ воркера",
		},
	}
}

// TestR697_LoadFailureEvent_NamesArchBuildAndAction — что именно видит оператор.
func TestR697_LoadFailureEvent_NamesArchBuildAndAction(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	p.PublishLoadFailureTransition("b1", modelUnsupportedFailureR697("Inkling-2-Mini-UD-Q4_K_M"))

	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("событий %d, want 1", len(events))
	}
	ev := events[0]
	// Машинный код обязан дойти до WebUI/SSE без изменений.
	if reason, _ := ev.Data["reason"].(string); reason != "model_unsupported" {
		t.Errorf("Data.reason = %v, want model_unsupported", ev.Data["reason"])
	}
	if ev.Severity != types.SeverityError {
		t.Errorf("Severity = %q, want error", ev.Severity)
	}

	// Текст: архитектура + сборка + действие (не «проверьте файл»).
	for _, want := range []string{"inkling2", "b1b770d90", "c/llama.cpp", "Inkling-2-Mini-UD-Q4_K_M"} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("Message не содержит %q: %q", want, ev.Message)
		}
	}
	if strings.Contains(ev.Message, "формат/архитектура") {
		t.Errorf("Message остался прежним общим текстом gguf_incompatible: %q", ev.Message)
	}
}

// TestR697_LoadFailureEvent_WorksWithoutDiagnostics — старая запись (или воркер до
// R697) причину всё равно объясняет словами, а не падает на пустых полях.
func TestR697_LoadFailureEvent_WorksWithoutDiagnostics(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	p.PublishLoadFailureTransition("b1", &types.LoadFailureInfo{
		Model: "m", Reason: "model_unsupported", Severity: "error",
	})
	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("событий %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Message, "не поддерживается сборкой llama.cpp") {
		t.Errorf("Message без диагностики не объясняет причину: %q", events[0].Message)
	}
}

// TestR697_FetchLoadProgressCarriesReason — async-путь (WebUI грузит без wait) узнаёт
// причину из /api/models/load/progress. Без кода причины провал по архитектуре
// неотличим от транзиентного сбоя, и HTTP-слой отвечает 500 «повтори».
func TestR697_FetchLoadProgressCarriesReason(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		status     int
		wantState  string
		wantReason string
		wantKnown  bool
	}{
		{
			name:       "провал по архитектуре",
			status:     http.StatusOK,
			body:       `{"name":"Inkling-2-Mini","state":"failed","reason":"model_unsupported","error":"unknown model architecture: 'inkling2'"}`,
			wantState:  "failed",
			wantReason: "model_unsupported",
			wantKnown:  true,
		},
		{
			name:      "воркер не знает модели",
			status:    http.StatusNotFound,
			body:      `{"error":"model not found and not loading"}`,
			wantState: "",
			wantKnown: false,
		},
		{
			// Воркер до R697: поля reason нет — поведение обязано остаться прежним.
			name:      "старый воркер без reason",
			status:    http.StatusOK,
			body:      `{"name":"m","state":"failed","error":"failed to load model"}`,
			wantState: "failed",
			wantKnown: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse test server url: %v", err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatalf("test server port: %v", err)
			}

			// fetchLoadProgressWithReason не зависит от состояния ModelManager.
			mm := &ModelManager{}
			state, _, reason, known := mm.fetchLoadProgressWithReason(
				srv.Client(), u.Hostname(), port, "b1", "Inkling-2-Mini")

			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if known != tc.wantKnown {
				t.Errorf("known = %v, want %v", known, tc.wantKnown)
			}
		})
	}
}
