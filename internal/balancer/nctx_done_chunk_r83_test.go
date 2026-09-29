// nctx_done_chunk_r83_test.go — R83 (2026-09-29).
//
// ДЕФЕКТ. Cppworker отдаёт n_ctx-ошибку НЕ как HTTP 4xx, а как HTTP 200 с полем
// "error" в финальном (done) чанке нативного NDJSON:
//
//	{"done":true,"error":"stream inference failed with code 2: requested
//	 n_ctx=128000 exceeds model's effective n_ctx=32768 ..."}
//
// Балансер такую ошибку только логировал (llamacpp_transport.go:819) и отдавал
// клиенту, а авто-reload не запускал: ветка `resp.StatusCode >= 400` не срабатывала,
// а ParseCppWorkerError намеренно возвращает nil для status < 400.
//
// Следствие — дедлок на живом стенде (лог 2026-09-29): балансер держал в
// LastKnownNCtx 128000, cppworker после рестарта поднял модель с 32768, preflight
// видел «128000 >= required» → NoOp → reload не запускался, и КАЖДЫЙ запрос Cline
// получал done-чанк с ошибкой.
//
// Здесь проверяется распознавание (isNCtxDoneChunkError) и сборка ошибки для
// handleNCtxReload (newNCtxErrorFromDoneChunk).
package balancer

import (
	"errors"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestR83_IsNCtxDoneChunkError_RealLogMessage — точный текст из живого лога
// (log_docker.txt, 2026-09-29) обязан распознаваться: иначе авто-reload не
// запустится и клиент снова получит ошибку на каждом запросе.
func TestR83_IsNCtxDoneChunkError_RealLogMessage(t *testing.T) {
	realWorld := "stream inference failed with code 2: requested n_ctx=128000 " +
		"exceeds model's effective n_ctx=32768. Auto-reload may be possible if VRAM " +
		"allows (max_vram_n_ctx=9250). Otherwise save a model profile with " +
		"n_ctx=128000 and reload the model (or send a smaller n_ctx in " +
		"options.num_ctx) (current_n_ctx=32768, required_n_ctx=128000, " +
		"max_vram_n_ctx=9250): n_ctx exceeds loaded model; auto-reload may be possible"

	if !isNCtxDoneChunkError(realWorld) {
		t.Fatalf("текст из живого лога не распознан как n_ctx-ошибка — авто-reload "+
			"не запустится, клиент получит ошибку: %q", realWorld)
	}

	// Prompt-too-long (code 3) — тоже повод для reload.
	if !isNCtxDoneChunkError("stream inference failed with code 3: prompt too long") {
		t.Error("code 3 (prompt too long) должен распознаваться как n_ctx-ошибка")
	}

	// Текстовые формулировки без кода — на случай смены формата cppworker.
	for _, text := range []string{
		"requested n_ctx=128000 exceeds model's effective n_ctx=32768",
		"n_ctx exceeds loaded model",
		"prompt exceeds context",
	} {
		if !isNCtxDoneChunkError(text) {
			t.Errorf("формулировка %q не распознана", text)
		}
	}
}

// TestR83_IsNCtxDoneChunkError_IgnoresUnrelated — чужие ошибки (модель не найдена,
// внутренние коды) НЕ должны запускать reload: иначе балансер начнёт перезагружать
// модель в ответ на любую ошибку.
func TestR83_IsNCtxDoneChunkError_IgnoresUnrelated(t *testing.T) {
	for _, text := range []string{
		"",
		"model not found",
		"stream inference failed with code 7: model \"x\" not found",
		"context canceled",
		"connection reset by peer",
	} {
		if isNCtxDoneChunkError(text) {
			t.Errorf("нерелевантная ошибка %q ошибочно распознана как n_ctx-ошибка "+
				"(приведёт к лишнему reload)", text)
		}
	}
}

// TestR83_NewNCtxErrorFromDoneChunk_WiresReloadPath — собранная ошибка обязана
// быть NCtxError с кодом NCtxNeedsReload: именно по нему handleNCtxReload идёт
// тем же путём, что и для HTTP-варианта.
func TestR83_NewNCtxErrorFromDoneChunk_WiresReloadPath(t *testing.T) {
	err := newNCtxErrorFromDoneChunk("stream inference failed with code 2: n_ctx exceeds loaded model", "backend-1")
	if err == nil {
		t.Fatal("newNCtxErrorFromDoneChunk вернул nil")
	}

	var nctxErr *NCtxError
	if !errors.As(err, &nctxErr) {
		t.Fatalf("ожидался *NCtxError, получено %T", err)
	}
	if nctxErr.BackendID != "backend-1" {
		t.Errorf("BackendID = %q, want backend-1", nctxErr.BackendID)
	}
	if nctxErr.BridgeInfo == nil {
		t.Fatal("BridgeInfo == nil: handleNCtxReload не увидит код ошибки")
	}
	if nctxErr.BridgeInfo.Code != bridge.ErrCodeNCtxNeedsReload {
		t.Errorf("BridgeInfo.Code = %d, want %d (NCtxNeedsReload)",
			nctxErr.BridgeInfo.Code, bridge.ErrCodeNCtxNeedsReload)
	}
	// HTTPStatus=200 — не опечатка: cppworker именно так и отвечает, ошибка в чанке.
	if nctxErr.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200 (ошибка приходит внутри done-чанка)",
			nctxErr.HTTPStatus)
	}
}
