//go:build llama_stub

// nctx_silent_client_r83_test.go — R83-fix (2026-09-30).
//
// ТРЕБОВАНИЕ ЭКСПЛУАТАЦИИ: «если клиент не прислал, с каким окном загружать, то
// если модель загружена — перезагружать не надо; если не загружена — грузить
// дефолтными значениями из настроек».
//
// ДЕФЕКТ. При body_num_ctx=0 preflight подставлял contextLength профиля (это
// HINT) и, если загруженный n_ctx ему не равнялся, запускал async reload. На
// живом стенде: модель загружена с ctx=8192, первый же клиентский запрос без
// num_ctx перезагружал её в 16384 (в логе: chosen_source=profile, «detected
// n_ctx mismatch, scheduling async reload»). Оператор видел это как «настройки
// сбиваются сами».
package balancer

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newProxyForSilentNCtx — прокси с ИНИЦИАЛИЗИРОВАННЫМ координатором reload.
//
// Важно: preflightNCtxReloadIfNeeded при p.nctxReload == nil возвращает OK сразу,
// поэтому тест «reload не случился» проходил бы тривиально и ничего не проверял.
func newProxyForSilentNCtx(t *testing.T, backendID, modelName string, loadedNCtx int) *Proxy {
	t.Helper()
	p := newProxyForPreflightSync(t, backendID, modelName, loadedNCtx)
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	// Запланированный async reload уходит в отдельную горутину и делает HTTP на
	// backend host:port. В тесте там никто не слушает — это нормально (запрос
	// упадёт с connection refused и залогируется), но клиент обязан быть
	// не-nil, иначе горутина паникует уже после завершения теста.
	p.client = &http.Client{Timeout: time.Second}
	return p
}

// loadedNCtxOf — что preflight считает загруженным n_ctx (кэш метрик).
func loadedNCtxOf(p *Proxy, backendID string) int {
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil {
		for _, m := range lm.LoadedModels {
			if m.ContextLength > 0 {
				return m.ContextLength
			}
		}
	}
	return 0
}

// TestR83_SilentClient_LoadedModel_NoReload — клиент без num_ctx, модель
// загружена с МЕНЬШИМ контекстом, чем подсказка профиля: перезагрузки быть не
// должно, настройки загруженной модели остаются прежними.
func TestR83_SilentClient_LoadedModel_NoReload(t *testing.T) {
	t.Parallel()
	const backendID = "r83-silent-backend"
	const modelName = "qwen2.5.gguf"

	p := newProxyForSilentNCtx(t, backendID, modelName, 8192)
	// Профиль-подсказка «просит» 16384 — именно он раньше и запускал reload.
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		modelName: {ContextLength: 16384},
	}
	stub := &stubMetricsDoer{loadedNCtx: 16384}
	p.metricsHTTPDoer = stub

	body := []byte(`{"model":"` + modelName + `"}`) // клиент не задал num_ctx
	modifiedBody, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, modelName, body, "/api/chat")

	if !needsProxy || status != http.StatusOK {
		t.Fatalf("ожидался пропуск запроса (OK); got needsProxy=%v status=%d msg=%q",
			needsProxy, status, msg)
	}
	if got := loadedNCtxOf(p, backendID); got != 8192 {
		t.Errorf("n_ctx загруженной модели изменился на %d — preflight перезагрузил "+
			"модель, хотя клиент окно не запрашивал (должно остаться 8192)", got)
	}
	if string(modifiedBody) != string(body) {
		t.Errorf("тело запроса изменено: %s", string(modifiedBody))
	}
	if int(stub.mu.Load()) != 0 {
		t.Errorf("heartbeat/reload-инфраструктура вызывалась (%d раз) — перезагрузки быть не должно",
			stub.mu.Load())
	}
}

// TestR83_SilentClient_NotLoadedModel_KeepsDefaultsPath — если модель НЕ
// загружена, правило не применяется: preflight, как и раньше, выбирает
// источник «профиль → дефолт бэкенда», то есть загрузка пойдёт значениями из
// настроек (а не «ничего не делаем»).
func TestR83_SilentClient_NotLoadedModel_KeepsDefaultsPath(t *testing.T) {
	t.Parallel()
	const backendID = "r83-silent-backend-2"
	const modelName = "qwen2.5.gguf"

	// loadedNCtx = 0 → модель не загружена.
	p := newProxyForSilentNCtx(t, backendID, modelName, 0)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		modelName: {ContextLength: 16384},
	}
	stub := &stubMetricsDoer{loadedNCtx: 0}
	p.metricsHTTPDoer = stub

	body := []byte(`{"model":"` + modelName + `"}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, modelName, body, "/api/chat")

	// Главное: путь НЕ обрезан новым правилом — загрузка запускается со
	// значениями из настроек (профиль-подсказка 16384), а не «ничего не делаем».
	// Клиент получает 503 + Retry-After — это штатное «модель грузится».
	if needsProxy {
		t.Fatalf("ожидалось, что загрузка будет запланирована (needsProxy=false), "+
			"получено needsProxy=true msg=%q status=%d", msg, status)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (модель грузится, клиенту Retry-After)", status)
	}
	if got := loadedNCtxOf(p, backendID); got != 16384 {
		t.Errorf("к загрузке принят n_ctx=%d, want 16384 — значение из настроек "+
			"(профиль-подсказка) не доехало", got)
	}
}

// TestR83_ExplicitClientNCtx_StillReloads — обратная совместимость: если клиент
// ЯВНО попросил больше, чем загружено, и prompt действительно не влезает —
// перезагрузка по-прежнему происходит (новое правило касается только молчащего
// клиента).
func TestR83_ExplicitClientNCtx_StillReloads(t *testing.T) {
	t.Parallel()
	const backendID = "r83-silent-backend-3"
	const modelName = "qwen2.5.gguf"

	p := newProxyForSilentNCtx(t, backendID, modelName, 4096)
	stub := &stubMetricsDoer{loadedNCtx: 16384}
	p.metricsHTTPDoer = stub

	// Большой prompt + num_ctx=16384 при загруженных 4096 → нужен reload.
	big := make([]byte, 0, 20000)
	big = append(big, `{"model":"`+modelName+`","options":{"num_ctx":16384},"messages":[{"role":"user","content":"`...)
	for i := 0; i < 4000; i++ {
		big = append(big, 'w')
	}
	big = append(big, `"}]}`...)

	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, modelName, big, "/api/chat")

	// Явный запрос клиента обрабатывается как раньше: reload запланирован,
	// клиенту 503 + Retry-After (или 200, если включён wait-режим и reload успел).
	if needsProxy && status == http.StatusOK {
		// Допустимо: wait-режим дождался перезагрузки и обслужил запрос.
		if got := loadedNCtxOf(p, backendID); got <= 4096 {
			t.Errorf("запрос обслужен, но n_ctx остался %d — перезагрузки не было", got)
		}
		return
	}
	if needsProxy {
		t.Fatalf("неожидаемое состояние: needsProxy=true status=%d msg=%q", status, msg)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 после планирования reload", status)
	}
	if got := loadedNCtxOf(p, backendID); got <= 4096 {
		t.Errorf("ожидалась запланированная перезагрузка на больший n_ctx, а загруженный "+
			"n_ctx остался %d — явный запрос клиента перестал работать", got)
	}
}
