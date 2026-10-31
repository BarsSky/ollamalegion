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

// cancelUpstreamPayloadR360 — тело приказа об отмене.
//
// R360 (2026-10-31, аудит abort A5). ДО ЭТОГО уходило только {"model": ...}, а воркер
// на такой приказ вызывает CancelByModel("", model) — то есть гасит ВСЕ генерации этой
// модели на узле, включая ЧУЖИЕ. Живой сценарий: два клиента работают с одной моделью,
// у одного отвалился браузер — и второму обрывает ответ. Клиент видит «сервер сам
// закрыл поток» и повторяет запрос, нагрузка растёт.
//
// Теперь, когда балансер знает upstream request_id (воркер возвращает его заголовком
// X-Request-Id), отмена адресная: {"request_id": ...}, гасится ровно одна генерация.
// Если id не известен (старый воркер, нестандартный путь) — поведение прежнее, но это
// видно в логе по полю precise=false, а не молчаливая «тихая» отмена всех.
func cancelUpstreamPayloadR360(model, requestID string) (map[string]interface{}, bool) {
	payload := map[string]interface{}{}
	if requestID != "" {
		payload["request_id"] = requestID
		return payload, true
	}
	if model != "" {
		payload["model"] = model
	}
	return payload, false
}

// cancelUpstreamGeneration — попросить воркер прекратить генерацию.
//
// requestID — id, под которым генерация зарегистрирована на воркере (из заголовка
// X-Request-Id ответа). Пусто = точной отмены не будет, гасим по модели (R184-прежнее).
//
// Причина (reason) попадает в лог: «client_cancel» и «stream_timeout» — разные события,
// и по журналу должно быть видно, почему генерация прервана.
func (p *Proxy) cancelUpstreamGeneration(backendID, model, requestID, reason string) {
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
	payload, precise := cancelUpstreamPayloadR360(model, requestID)
	if !precise && logger.Get() != nil {
		// Не глушим молча: «отменили все генерации модели» — это побочный эффект,
		// который оператор обязан видеть, а не догадываться о нём по поведению клиентов.
		logger.Get().Warnw("cancel upstream: request_id неизвестен — отмена по модели",
			"backend", backendID, "model", model, "reason", reason,
			"hint", "точная отмена невозможна: воркер не вернул X-Request-Id; "+
				"будут отменены ВСЕ генерации этой модели на узле")
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
				"backend", backendID, "model", model, "request_id", requestID,
				"precise", precise, "reason", reason, "status", resp.StatusCode)
		}
	}()
}
