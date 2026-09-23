// autoload_wait.go — R67a (2026-09-23): дождаться авто-загрузки модели вместо
// немедленного 503.
//
// ЖАЛОБА: «если модель не загружена, клиенту сразу прилетает ошибка что будет
// она загружена через 30-180 секунд, и только повторный запрос проходит».
//
// Механизм: ensureModelLoadedOnBackend в async-режиме (LB_AUTO_LOAD_ASYNC,
// default) запускал load в goroutine и СРАЗУ возвращал ошибку
// «model X load started, waiting for cppworker to finish (~30-180s)», которую
// HTTP-хендлер превращал в 503 + Retry-After. Клиент (OpenWebUI) на 503 либо
// показывает ошибку пользователю, либо повторяет запрос — то есть первый запрос
// никогда не обслуживается.
//
// Почему async был введён: sync-режим блокировал handler до 3 минут и клиенты с
// таймаутом 60-120s отваливались. Но на нормальном железе (A10 24GB) загрузка
// обычно укладывается в 30-120 секунд, поэтому разумный компромисс — ЖДАТЬ
// ограниченное время (LB_AUTO_LOAD_WAIT_SEC, default 180), и отдавать 503 только
// если модель реально не успела. Состояние опрашивается напрямую у cppworker
// (/api/models/load/progress), а не через метрики балансера — те обновляются раз
// в 30 секунд и дали бы лишние 30s задержки.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// autoLoadWaitDefaultSec — сколько ждать завершения авто-загрузки перед тем, как
// вернуть клиенту 503+Retry-After. 0 = прежнее поведение (сразу 503).
const autoLoadWaitDefaultSec = 180

// autoLoadPollInterval — период опроса состояния загрузки.
const autoLoadPollInterval = 2 * time.Second

// AutoLoadWaitTimeout — R67a: сколько ждать авто-загрузку модели.
//
//	LB_AUTO_LOAD_WAIT_SEC=N  — N секунд (0 = не ждать, прежнее поведение;
//	                           отрицательное = ждать до отмены клиентом).
//	По умолчанию 180s. Значение имеет смысл только в async-режиме
//	(LB_AUTO_LOAD_ASYNC=1, default): в sync-режиме handler и так блокируется на
//	загрузке.
func AutoLoadWaitTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_AUTO_LOAD_WAIT_SEC"))
	if v == "" {
		return autoLoadWaitDefaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return autoLoadWaitDefaultSec * time.Second
	}
	if n == 0 {
		return 0
	}
	if n < 0 {
		// «Ждать сколько потребуется» — ограничиваем разумным максимумом, чтобы
		// не держать соединение вечно (клиент всё равно отвалится раньше).
		return 30 * time.Minute
	}
	return time.Duration(n) * time.Second
}

// modelLoadState — состояние загрузки модели на конкретном бэкенде.
type modelLoadState struct {
	State string
	Error string
	Known bool
}

// fetchModelLoadState — один опрос /api/models/load/progress у cppworker.
//
// 404 = записи о загрузке нет: либо модель уже загружена (load завершился и
// запись снята), либо загрузка вообще не начиналась — различаем по /api/models.
func (lr *LlamaCppRouter) fetchModelLoadState(
	ctx context.Context, client *http.Client, baseURL, modelName string,
) modelLoadState {
	url := fmt.Sprintf("%s/api/models/load/progress?model=%s", baseURL, urlPathEscape(modelName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return modelLoadState{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return modelLoadState{}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return modelLoadState{State: "unknown"}
	}
	if resp.StatusCode != http.StatusOK {
		return modelLoadState{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return modelLoadState{}
	}
	var parsed struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return modelLoadState{}
	}
	return modelLoadState{State: strings.ToLower(parsed.State), Error: parsed.Error, Known: true}
}

// isModelPresentOnBackend — модель присутствует в /api/models cppworker'а
// (в состоянии loaded или loading). Используется как второй признак готовности:
// cppworker может снять запись прогресса, оставив модель загруженной.
func (lr *LlamaCppRouter) isModelPresentOnBackend(
	ctx context.Context, client *http.Client, baseURL, modelName string,
) (loaded bool, present bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/models", nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, false
	}
	var parsed struct {
		Models []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, false
	}
	for _, m := range parsed.Models {
		if !modelNameMatches(m.Name, modelName) {
			continue
		}
		if strings.EqualFold(m.State, "loaded") {
			return true, true
		}
		return false, true
	}
	return false, false
}

// waitForModelLoad — ждёт готовности модели после запущенной авто-загрузки.
//
// Возвращает nil, когда модель загружена. Если cppworker сообщил об ошибке
// загрузки — возвращает её текст (клиент получит actionable причину вместо
// общего 503). Если истёк timeout/контекст — возвращает соответствующую ошибку,
// и вызывающий код отдаёт прежний 503+Retry-After (модель при этом продолжает
// грузиться, повторный запрос пройдёт).
func (lr *LlamaCppRouter) waitForModelLoad(
	ctx context.Context, backendID, modelName string, timeout time.Duration,
) error {
	if lr == nil || lr.proxy == nil || timeout <= 0 {
		return fmt.Errorf("wait disabled")
	}
	baseURL := lr.proxy.backendHTTPAddrByID(backendID)
	if baseURL == "" {
		return fmt.Errorf("unknown backend %q", backendID)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("client canceled while waiting for model load: %w", err)
		}

		st := lr.fetchModelLoadState(ctx, client, baseURL, modelName)
		switch st.State {
		case "loaded":
			return nil
		case "error", "failed":
			msg := st.Error
			if msg == "" {
				msg = "cppworker reported load failure"
			}
			return fmt.Errorf("model load failed: %s", msg)
		case "loading":
			// продолжаем ждать
		default:
			// Записи прогресса нет — проверяем список моделей: возможно, модель
			// уже загружена (запись снята) или загружена под каноническим именем.
			loaded, present := lr.isModelPresentOnBackend(ctx, client, baseURL, modelName)
			if loaded {
				return nil
			}
			if !present && time.Now().After(deadline) {
				return fmt.Errorf("model did not appear on backend within %s", timeout)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("model load did not finish within %s", timeout)
		}
		// Спим не дольше, чем осталось до дедлайна: иначе таймаут «переезжает»
		// на целый интервал опроса (клиент ждёт лишние 2 секунды).
		nap := autoLoadPollInterval
		if remaining := time.Until(deadline); remaining < nap {
			nap = remaining
		}
		if nap <= 0 {
			return fmt.Errorf("model load did not finish within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("client canceled while waiting for model load: %w", ctx.Err())
		case <-time.After(nap):
		}
	}
}

// waitForBackendNCtx — R68 (2026-09-23): дождаться, пока cppworker сообщит
// context_size >= want для модели (после reload'а n_ctx).
//
// Нужен transport-level пути preflightNCtxReloadIfNeeded: раньше он всегда
// отвечал клиенту 503 «being reloaded, retry in 30s», и Cline/OpenWebUI
// показывали ошибку, хотя reload занимал десятки секунд — то есть
// «проходил только повторный запрос». Теперь запрос ждёт готовности модели
// (как R67a делает для авто-загрузки) и обслуживается сразу.
//
// Возвращает true, если модель загружена с нужным n_ctx; false при таймауте,
// отмене или ошибке опроса.
func (p *Proxy) waitForBackendNCtx(
	ctx context.Context, backendID, modelName string, want int, timeout time.Duration,
) bool {
	if p == nil || timeout <= 0 || want <= 0 {
		return false
	}
	baseURL := p.backendHTTPAddrByID(backendID)
	if baseURL == "" {
		return false
	}
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		if nctx, loaded := p.fetchBackendModelNCtx(ctx, client, baseURL, modelName); loaded && nctx >= want {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		nap := autoLoadPollInterval
		if remaining < nap {
			nap = remaining
		}
		select {
		case <-time.After(nap):
		case <-ctxDone(ctx):
			return false
		}
	}
}

// ctxDone — nil-safe ctx.Done() (ctx может быть nil в вызовах без контекста).
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// fetchBackendModelNCtx — один опрос /api/models: возвращает (context_size, найден).
func (p *Proxy) fetchBackendModelNCtx(
	ctx context.Context, client *http.Client, baseURL, modelName string,
) (int, bool) {
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, baseURL+"/api/models", nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	type modelEntry struct {
		Name          string `json:"name"`
		ID            string `json:"id"`
		State         string `json:"state"`
		ContextSize   int    `json:"context_size"`
		ContextLength int    `json:"contextLength"`
	}
	var parsed struct {
		Models []modelEntry `json:"models"`
		// loaded_models — ключ старых сборок cppworker (и тестовых моков).
		LoadedModels []modelEntry `json:"loaded_models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	entries := append(parsed.Models, parsed.LoadedModels...)
	for _, m := range entries {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		if !modelNameMatches(name, modelName) {
			continue
		}
		if !strings.EqualFold(m.State, "loaded") {
			return 0, true // найдена, но ещё грузится
		}
		// Живой cppworker отдаёт snake_case context_size, старые сборки/моки —
		// camelCase contextLength: поддерживаем оба.
		if m.ContextSize > 0 {
			return m.ContextSize, true
		}
		return m.ContextLength, true
	}
	return 0, false
}

// ctxOrBackground — nil-safe context.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// RequestSessionKey — R67a: идентификатор пользователя/сессии клиента.
// Нужен, чтобы различать разных пользователей OpenWebUI (одна модель может
// обслуживать нескольких человек одновременно) в логах, метриках и в
// admission-очереди (R67b). Порядок источников:
//  1. X-User-Id / X-OpenWebUI-User-Id / X-User-Email — явный пользователь;
//  2. X-Session-Id / X-Chat-Id — идентификатор сессии/чата;
//  3. поле "user"/"user_id" в теле запроса (стандарт OpenAI) или "chat_id"/
//     "session_id" (Ollama), в том числе внутри metadata (OpenWebUI);
//  4. "" — анонимный клиент (Cline и подобные не передают ни того, ни другого).
//
// Значение не аутентифицирует запрос — это только ключ группировки/справедливости.
func RequestSessionKey(r *http.Request, body []byte) string {
	if r != nil {
		for _, h := range []string{"X-User-Id", "X-OpenWebUI-User-Id", "X-User-Email"} {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return h + ":" + v
			}
		}
		for _, h := range []string{"X-Session-Id", "X-Chat-Id", "X-Conversation-Id"} {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return h + ":" + v
			}
		}
	}
	if len(body) > 0 {
		var parsed struct {
			User      string `json:"user"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
			ChatID    string `json:"chat_id"`
			Metadata  struct {
				UserID    string `json:"user_id"`
				ChatID    string `json:"chat_id"`
				SessionID string `json:"session_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			for _, v := range []string{parsed.User, parsed.UserID, parsed.Metadata.UserID} {
				if v = strings.TrimSpace(v); v != "" {
					return "user:" + v
				}
			}
			for _, v := range []string{parsed.SessionID, parsed.Metadata.SessionID} {
				if v = strings.TrimSpace(v); v != "" {
					return "session:" + v
				}
			}
			for _, v := range []string{parsed.ChatID, parsed.Metadata.ChatID} {
				if v = strings.TrimSpace(v); v != "" {
					return "chat:" + v
				}
			}
		}
	}
	return ""
}
