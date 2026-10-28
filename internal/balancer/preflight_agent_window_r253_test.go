// preflight_agent_window_r253_test.go — R253 (2026-10-28): окно n_ctx растёт под
// агентский ход, а preflight-диалог ДООБСЛУЖИВАЕТ запрос вместо пустого [DONE].
//
// ЖИВОЙ ДЕФЕКТ (Cline, длинная серия tool-call'ов). Cline не присылает num_ctx,
// поэтому окно модели не росло никогда: cppworker резал вывод по фактическому
// промпту (prompt ≥ n_ctx → n_predict=0, иначе clamp до n_ctx-prompt-64), а
// preflight-диалог на async reload отвечал keepalive'ами и `data: [DONE]` БЕЗ
// ответа модели. Снаружи: «сначала быстро, как упёрлось — срыв сессии».
//
// Тесты фиксируют:
//   - промпт больше загруженного окна и num_ctx нет → preflight ТРЕБУЕТ рост;
//   - LB_NCTX_GROW_FOR_AGENTS=0 возвращает прежнее поведение (NoOp);
//   - промпт влезает → NoOp (никаких перезагрузок «на всякий случай»);
//   - после reload клиент получает РЕАЛЬНЫЕ кадры ответа, а не только [DONE];
//   - ошибка дообслуживания → error-кадр + ровно один [DONE].
package balancer

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// r253State — состояние бэкенда: окно слота 16384, потолки большие.
func r253State() *NCtxBackendState {
	return &NCtxBackendState{
		BackendID:            "cppworker-1",
		CurrentNCtx:          32768,
		CurrentContextPerSeq: 16384,
		CurrentSlots:         2,
		ModelMaxContext:      131072,
		PhysicalMaxContext:   131072,
		GGUFMaxContext:       131072,
		AutoReloadMaxNCtx:    131072,
	}
}

// TestR253_AgentPromptBeyondWindowGrows — (а) Cline без num_ctx, промпт больше
// окна → preflight требует рост (не NoOp).
func TestR253_AgentPromptBeyondWindowGrows(t *testing.T) {
	// 20000 + reserve 4096 + 64 = 24160 > 16384 (окно слота) → нужен рост.
	meta := &RequestMeta{EstimatedPromptTokens: 20000}

	res := DecidePreflight(meta, r253State(), DefaultNCtxReloadConfig())
	if res.Decision == PreflightNoOp {
		t.Fatalf("промпт 20000 не влезает в окно слота 16384, а preflight дал NoOp — "+
			"cppworker обрежет вывод (живой дефект): %+v", res)
	}
	if res.Decision != PreflightReload {
		t.Fatalf("ожидался PreflightReload, получено %v: %s", res.Decision, res.RejectBody)
	}
	if !res.AgentDerivedWindow {
		t.Error("окно выведено из промпта, но AgentDerivedWindow=false — оператор не увидит причину в логе")
	}
	if res.TargetNCtx < 20000 {
		t.Errorf("TargetNCtx=%d меньше самого промпта (20000): рост не решит проблему", res.TargetNCtx)
	}
}

// TestR253_GrowForAgentsDisabledIsBackwardCompatible — (а, обратная совместимость)
// одной переменной поведение возвращается к прежнему: NoOp.
func TestR253_GrowForAgentsDisabledIsBackwardCompatible(t *testing.T) {
	t.Setenv("LB_NCTX_GROW_FOR_AGENTS", "0")

	meta := &RequestMeta{EstimatedPromptTokens: 20000}
	res := DecidePreflight(meta, r253State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("LB_NCTX_GROW_FOR_AGENTS=0 должен давать прежнее NoOp, получено %v: %s",
			res.Decision, res.RejectBody)
	}
}

// TestR253_AgentPromptFitsWindowNoReload — (б) промпт меньше окна → NoOp.
func TestR253_AgentPromptFitsWindowNoReload(t *testing.T) {
	// 8000 + 4096 + 64 = 12160 ≤ 16384.
	meta := &RequestMeta{EstimatedPromptTokens: 8000}
	res := DecidePreflight(meta, r253State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("промпт влезает в окно, ожидался NoOp (никаких лишних reload), получено %v: %s",
			res.Decision, res.RejectBody)
	}
}

// TestR253_ClientWindowStillWins — клиентское окно по-прежнему в приоритете:
// агентская эвристика не подменяет явный запрос.
func TestR253_ClientWindowStillWins(t *testing.T) {
	meta := &RequestMeta{RequestedNCtxOverride: 4096, EstimatedPromptTokens: 20000}
	res := DecidePreflight(meta, r253State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("клиент просит 4096 (меньше загруженных 16384) — ожидался NoOp, получено %v: %s",
			res.Decision, res.RejectBody)
	}
	if res.AgentDerivedWindow {
		t.Error("клиентское окно не должно помечаться как выведенное из промпта")
	}
}

// newR253SelfProxyServer — заглушка собственной OpenAI-поверхности, которая
// отдаёт два содержательных кадра и НЕ присылает [DONE] (проверяем гарантию).
func newR253SelfProxyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(preflightDialogServedHeader) == "" {
			t.Errorf("self-proxy не поставил маркер %s — возможен цикл диалога", preflightDialogServedHeader)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestR253_AfterReloadClientGetsRealFrames — (в) после reload клиент получает
// настоящие кадры ответа, а не только [DONE].
func TestR253_AfterReloadClientGetsRealFrames(t *testing.T) {
	srv := newR253SelfProxyServer(t)
	t.Setenv("LB_SELF_BASE_URL", srv.URL)

	incoming := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true}`))
	incoming.Header.Set("Content-Type", "application/json")

	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamSelfProxyChunked(ctx, incoming, bufrw, []byte(`{"model":"m1","stream":true}`), 32768)

	got := buf.String()
	if !strings.Contains(got, "hel") || !strings.Contains(got, "lo") {
		t.Fatalf("клиент не получил кадры ответа модели (только маркер?): %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("кадров [DONE] %d, want 1 (поток обязан закрыться ровно одним): %q", n, got)
	}
	if !strings.Contains(got, ": ") && strings.Contains(got, "keepalive") {
		t.Log("keepalive-строки в ответе допустимы")
	}
}

// TestR253_SelfProxyFailureGetsErrorAndDone — (г) ошибка дообслуживания → error-кадр
// и ровно один [DONE] (клиент видит причину, а не оборванный поток).
func TestR253_SelfProxyFailureGetsErrorAndDone(t *testing.T) {
	// Закрытый порт: self-proxy обязан вернуть ошибку соединения.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	t.Setenv("LB_SELF_BASE_URL", deadURL)

	incoming := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true}`))
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamSelfProxyChunked(ctx, incoming, bufrw, []byte(`{"model":"m1"}`), 32768)

	got := buf.String()
	if !strings.Contains(got, "error") {
		t.Fatalf("нет error-кадра после провала дообслуживания: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("кадров [DONE] %d, want 1 (поток обязан завершиться): %q", n, got)
	}
}

// TestR253_DialogKeepaliveIsComment — R253: keepalive'ы в диалоге — SSE-комментарии,
// а не JSON-чанки с пустыми choices (строгие OpenAI-клиенты считают их мусором).
func TestR253_DialogKeepaliveIsComment(t *testing.T) {
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamDialogKeepaliveLoop(bufrw, ctx, "b1", "m1", 32768, streamDialogConfig{
			keepalivePeriod: 5 * time.Millisecond,
		})
	}()
	time.Sleep(25 * time.Millisecond) // несколько тиков
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive-цикл не остановился по отмене контекста")
	}
	if !strings.Contains(buf.String(), ": keepalive") {
		t.Fatalf("keepalive не SSE-комментарий: %q", buf.String())
	}
	if strings.Contains(buf.String(), "chat.completion.chunk") {
		t.Fatalf("keepalive всё ещё JSON-чанк: %q", buf.String())
	}
}
