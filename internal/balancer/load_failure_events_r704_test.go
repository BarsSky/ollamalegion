//go:build llama_stub

// load_failure_events_r704_test.go — R704 (2026-11-24).
//
// ПРЕДМЕТ. Уведомление оператору (bell-меню WebUI) и HTTP-статус на async-пути
// загрузки для причины model_schema_unsupported — «архитектуру сборка знает, а
// схему файла нет» (loop-развёрнутый GGUF: 24 физических блока при
// block_count=48).
//
// Без этой правки оператор увидел бы «файл модели не принят llama.cpp
// (формат/архитектура)» — то есть снова пошёл бы проверять файл, хотя файл на
// месте и исправен, а на async-пути балансер отдал бы 500 «повтори» вместо
// терминального 409.
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

// modelSchemaUnsupportedFailureR704 — запись провала в том виде, в каком её
// строит cppworker (diagnostics model_schema_unsupported.go). Числа специально
// float64, как после JSON-раундтрипа cppworker → agent → балансер: приведение
// `.(int)` потеряло бы их, и это ловится тестом.
func modelSchemaUnsupportedFailureR704(model string) *types.LoadFailureInfo {
	return &types.LoadFailureInfo{
		Model:    model,
		Reason:   "model_schema_unsupported",
		Error:    "load model " + model + ": check_tensor_dims: tensor 'blk.24.attn_q.weight' not found",
		Severity: "error",
		Diagnostics: map[string]interface{}{
			"arch":            "inkling2",
			"llama_build":     "b12642",
			"block_count":     float64(48),
			"physical_blocks": float64(24),
			"tensor":          "blk.24.attn_q.weight",
			"hint":            "нужен ре-экспорт GGUF как inkling2_loop с block_count=24 или более новый загрузчик llama.cpp",
		},
	}
}

// TestR704_LoadFailureEvent_NamesSchemaNumbersAndAction — что видит оператор:
// архитектуру, числа (block_count=48 / физических блоков=24) и действие; ни
// «модель не найдена», ни прежнего общего «формат/архитектура».
func TestR704_LoadFailureEvent_NamesSchemaNumbersAndAction(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	p.PublishLoadFailureTransition("b1", modelSchemaUnsupportedFailureR704("Inkling-2-Mini-UD-Q4_K_M"))

	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("событий %d, want 1", len(events))
	}
	ev := events[0]
	if reason, _ := ev.Data["reason"].(string); reason != "model_schema_unsupported" {
		t.Errorf("Data.reason = %v, want model_schema_unsupported", ev.Data["reason"])
	}
	if ev.Severity != types.SeverityError {
		t.Errorf("Severity = %q, want error", ev.Severity)
	}
	for _, want := range []string{"inkling2", "block_count=48", "24", "inkling2_loop", "Inkling-2-Mini-UD-Q4_K_M"} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("Message не содержит %q: %q", want, ev.Message)
		}
	}
	for _, unwanted := range []string{"формат/архитектура", "Модель не найдена"} {
		if strings.Contains(ev.Message, unwanted) {
			t.Errorf("Message остался прежним общим текстом (%q): %q", unwanted, ev.Message)
		}
	}
}

// TestR704_LoadFailureEvent_WorksWithoutDiagnostics — запись без чисел (воркер не
// смог прочитать заголовок) причину всё равно объясняет словами.
func TestR704_LoadFailureEvent_WorksWithoutDiagnostics(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	p.PublishLoadFailureTransition("b1", &types.LoadFailureInfo{
		Model: "m", Reason: "model_schema_unsupported", Severity: "error",
	})
	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("событий %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Message, "не поддерживается загрузчиком llama.cpp") {
		t.Errorf("Message без диагностики не объясняет причину: %q", events[0].Message)
	}
	if !strings.Contains(events[0].Message, "схеме тензоров") {
		t.Errorf("Message не говорит, что дело в схеме: %q", events[0].Message)
	}
}

// TestR704_SchemaFailureIsTerminalOnAsyncPath — reason из /api/models/load/progress
// попадает в модель-менеджер без потерь: по нему HTTP-слой выбирает 409, а не 500.
//
// Проверяется путь «воркер ответил 202, загрузка провалилась» — именно так R704 и
// проявился на стенде (async runAsyncLoad).
func TestR704_SchemaFailureIsTerminalOnAsyncPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Inkling-2-Mini","state":"failed",` +
			`"reason":"model_schema_unsupported",` +
			`"error":"check_tensor_dims: tensor 'blk.24.attn_q.weight' not found"}`))
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

	mm := &ModelManager{}
	state, _, reason, known := mm.fetchLoadProgressWithReason(
		srv.Client(), u.Hostname(), port, "b1", "Inkling-2-Mini")
	if state != "failed" || !known {
		t.Fatalf("state = %q known=%v, want failed/true", state, known)
	}
	if reason != "model_schema_unsupported" {
		t.Fatalf("reason = %q, want model_schema_unsupported", reason)
	}
	// То же правило, которым управляет HTTP-слой: терминальный отказ → 409.
	if !isTerminalLoadRefusal(reason) {
		t.Error("код причины не признан терминальным — балансер отдаст 500 «повтори»")
	}
	if !isTerminalLoadRefusal("model_unsupported") {
		t.Error("R697 потерян: model_unsupported больше не терминальный отказ")
	}
	// Обратная сторона: транзиентный/рядовой провал не должен глушить разбор выше.
	if isTerminalLoadRefusal("model_not_found") {
		t.Error("model_not_found признан терминальным отказом по содержимому")
	}
	if isTerminalLoadRefusal("") {
		t.Error("пустой код причины признан терминальным отказом")
	}
}
