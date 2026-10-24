// cancel_upstream_r184.go — R184 (2026-10-19): ОТМЕНА ГЕНЕРАЦИИ ПРИ РАЗРЫВЕ КЛИЕНТА.
//
// ЗАЧЕМ. Матрица ошибок R180 прошла 16/16, но ЗАМЕР времени показал дефект:
// запрос, отправленный сразу после обрыва клиента, занял 100 429 мс (в другом
// прогоне — 548 мс). Механизм: балансер при обрыве закрывает СВОЁ чтение upstream
// (resp.Body.Close()), но воркер об этом не узнаёт и продолжает генерировать —
// модель держит единственный слот, и следующий клиент ждёт её конца.
//
// У воркера есть штатная отмена (POST /api/cancel: request_id | user_id | model),
// и в коде воркера прямо записано, что дочерние генерации НЕ видят ctx.Done() —
// значит отмену надо ПРИСЛАТЬ, а не полагаться на закрытое соединение.
//
// Здесь: балансер при client cancel / stream timeout отправляет воркеру
// {"model": ...}. Вызов асинхронный и с коротким таймаутом: путь ответа клиенту
// он задерживать не должен, а его неудача не отменяет уже принятого решения.
package balancer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// cancelUpstreamTimeout — сколько ждём ответа на отмену. Отмена — служебный
// вызов: если воркер занят, ответа может не быть, но сам приказ уже в сокете.
const cancelUpstreamTimeout = 3 * time.Second

// cancelUpstreamGeneration — попросить воркер прекратить генерацию модели.
//
// Причина (reason) попадает в лог: «client_cancel» и «timeout» — разные события,
// и по журналу должно быть видно, почему генерация прервана.
func (p *Proxy) cancelUpstreamGeneration(backendID, model, reason string) {
	if p == nil || backendID == "" {
		return
	}
	addr := p.backendHTTPAddrByID(backendID)
	if addr == "" {
		return
	}
	token := ""
	if b := p.GetBackend(backendID); b != nil {
		token = b.CppWorkerApiToken
	}
	payload := map[string]interface{}{}
	if model != "" {
		payload["model"] = model
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	// Асинхронно: путь ответа клиенту не должен ждать воркер.
	go func() {
		url := addr + "/api/cancel"
		req, reqErr := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-API-Token", token)
		}
		client := &http.Client{Timeout: cancelUpstreamTimeout}
		resp, doErr := client.Do(req)
		if doErr != nil {
			if logger.Get() != nil {
				logger.Get().Warnw(i18n.T("balancer.cancel_upstream.not_accepted_warn"),
					"backend", backendID, "model", model, "reason", reason, "error", doErr)
			}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if logger.Get() != nil {
			logger.Get().Infow(i18n.T("balancer.cancel_upstream.cancelled_info"),
				"backend", backendID, "model", model, "reason", reason, "status", resp.StatusCode)
		}
	}()
}
