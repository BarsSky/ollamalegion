package balancer

// stream_abort_r296_test.go — R296 (2026-10-29).
//
// Два живых дефекта:
//
//  1. ПРИЧИНА ОБРЫВА ПОДМЕНЯЛАСЬ ПОМЕТКОЙ. При `docker kill` cppworker посреди
//     стрима клиент получил «stream stopped because the balancer reloaded the
//     model for another request; please retry»: перезагрузки не было, воркер
//     умер, а совет «retry» был обречён. Причина обязана определяться фактом
//     («модель реально сняли»), а не тем, что когда-то ставилась пометка.
//
//  2. ПРИЧИНА И ТЕРМИНАТОР ЕХАЛИ ОДНИМ КАДРОМ (`done:true` + `error`), из-за
//     чего клиент закрывал ход раньше, чем разбирал причину. Контракт: сначала
//     кадр-причина (ход НЕ закрыт), затем ровно один терминатор.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newR296Proxy() *Proxy { return &Proxy{} }

// TestR296_AttributionRequiresRealUnload — пометка «начали» сама по себе не
// обвиняет балансер: без подтверждённой выгрузки обрыв остаётся upstream-причиной.
func TestR296_AttributionRequiresRealUnload(t *testing.T) {
	p := newR296Proxy()

	p.noteDestructiveOpR271("bk1", "m1", destructiveReloadR271)
	if kind, ok := p.recentDestructiveOpR271("bk1", "m1", time.Minute); ok {
		t.Fatalf("обвинили балансер без реальной выгрузки: kind=%q", kind)
	}
	// Ровно та же картина, что на живом стенде: пометка есть, воркер убит,
	// классификатор должен назвать upstream-причину.
	if got := streamAbortReasonR259(errors.New("unexpected EOF"), nil); got != streamAbortUpstreamError {
		t.Fatalf("причина = %q, want %q", got, streamAbortUpstreamError)
	}

	// Модель реально сняли — теперь обрыв можно объяснять нашей перезагрузкой.
	p.confirmDestructiveOpUnloadedR271("bk1", "m1")
	kind, ok := p.recentDestructiveOpR271("bk1", "m1", time.Minute)
	if !ok || kind != destructiveReloadR271 {
		t.Fatalf("после реальной выгрузки причина не признана: ok=%v kind=%q", ok, kind)
	}

	// Снятие пометки (операция не состоялась) возвращает честную upstream-причину.
	p.clearDestructiveOpR271("bk1", "m1")
	if _, ok := p.recentDestructiveOpR271("bk1", "m1", time.Minute); ok {
		t.Fatal("пометка не снята — снова ложная причина")
	}
}

// TestR296_AttributionWindowExpires — подтверждённая выгрузка «протухает»:
// через окно атрибуции обрыв снова считается upstream-причиной.
func TestR296_AttributionWindowExpires(t *testing.T) {
	p := newR296Proxy()
	p.noteDestructiveOpR271("bk1", "m1", destructiveReloadR271)
	p.confirmDestructiveOpUnloadedR271("bk1", "m1")

	// Сдвигаем момент выгрузки в прошлое (окно атрибуции считается от него).
	p.streamsR271.mu.Lock()
	key := streamRefKeyR271("bk1", "m1")
	op := p.streamsR271.destructive[key]
	op.at = time.Now().Add(-2 * time.Minute)
	p.streamsR271.destructive[key] = op
	p.streamsR271.mu.Unlock()

	if _, ok := p.recentDestructiveOpR271("bk1", "m1", time.Minute); ok {
		t.Fatal("окно атрибуции не истекло — старая выгрузка снова обвиняет балансер")
	}
	if _, ok := p.recentDestructiveOpR271("bk1", "m1", 3*time.Minute); !ok {
		t.Fatal("в пределах окна причина должна признаваться")
	}
}

// TestR296_NDJSONReasonThenSingleTerminator — для /api/chat: причина идёт
// ОТДЕЛЬНЫМ кадром с done:false, затем ровно один терминатор.
func TestR296_NDJSONReasonThenSingleTerminator(t *testing.T) {
	frames := abortFramesR296("/api/chat", "m1", streamAbortUpstreamError)
	if len(frames) != 2 {
		t.Fatalf("кадров = %d, want 2 (причина + терминатор)", len(frames))
	}

	var reason map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(frames[0]))), &reason); err != nil {
		t.Fatalf("кадр-причина не JSON: %v", err)
	}
	if done, _ := reason["done"].(bool); done {
		t.Error("кадр-причина закрывает ход (done:true) — клиент не успеет прочитать причину")
	}
	if msg, _ := reason["error"].(string); !strings.Contains(msg, "upstream") && !strings.Contains(msg, "backend") {
		t.Errorf("причина не названа: %q", msg)
	}

	var terminal map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(frames[1]))), &terminal); err != nil {
		t.Fatalf("терминатор не JSON: %v", err)
	}
	if done, _ := terminal["done"].(bool); !done {
		t.Error("терминатор не закрывает ход")
	}
	if _, hasErr := terminal["error"]; hasErr {
		t.Error("терминатор не должен нести причину — она уже в предыдущем кадре")
	}
}

// TestR296_SSEReasonWithoutFinish — для /v1: кадр-причина БЕЗ finish_reason,
// затем ровно один терминатор с finish_reason.
func TestR296_SSEReasonWithoutFinish(t *testing.T) {
	frames := abortFramesR296("/v1/chat/completions", "m1", streamAbortBalancerReload)
	if len(frames) != 2 {
		t.Fatalf("кадров = %d, want 2", len(frames))
	}

	payload := func(i int) map[string]interface{} {
		line := strings.TrimSpace(string(frames[i]))
		line = strings.TrimPrefix(line, "data: ")
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("кадр %d не JSON: %v", i, err)
		}
		return m
	}

	reason := payload(0)
	if _, hasErr := reason["error"]; !hasErr {
		t.Error("в первом кадре нет причины")
	}
	choices, _ := reason["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]interface{})
	if fr, hasFR := choice["finish_reason"]; hasFR && fr != nil {
		t.Errorf("кадр-причина закрывает ход: finish_reason=%v", fr)
	}

	terminal := payload(1)
	choices, _ = terminal["choices"].([]interface{})
	choice, _ = choices[0].(map[string]interface{})
	if fr, _ := choice["finish_reason"].(string); fr == "" {
		t.Error("в терминаторе нет finish_reason")
	}
	if _, hasErr := terminal["error"]; hasErr {
		t.Error("терминатор не должен нести причину")
	}
}

// TestR296_OurLimitHasNoClientReason — наш лимит буфера: клиенту не сообщаем
// «обрыв upstream», только корректный терминатор (как в R259).
func TestR296_OurLimitHasNoClientReason(t *testing.T) {
	for _, path := range []string{"/api/chat", "/v1/chat/completions"} {
		frames := abortFramesR296(path, "m1", streamAbortOurLimit)
		if len(frames) != 1 {
			t.Fatalf("%s: кадров = %d, want 1 (только терминатор)", path, len(frames))
		}
		if strings.Contains(string(frames[0]), "upstream") {
			t.Errorf("%s: наш лимит обвиняет upstream: %s", path, frames[0])
		}
	}
}
