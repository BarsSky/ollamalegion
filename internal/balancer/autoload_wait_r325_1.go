// autoload_wait_r325_1.go — R325.1 (2026-10-30): таймаут загрузки модели —
// не отказ.
//
// НАЙДЕНО ЖИВОЙ МАТРИЦЕЙ API (tests/api_contract). `/api/chat` на только что
// выгруженной модели получал 503 через 180.6 с с телом
//   {"error":"model \"gemma-…\" is not loaded and auto-load failed: model load failed: "}
// — при том что в логе балансера через ту же секунду было
//   "executeLlamaCppLoad: model loaded successfully (recovered from HTTP timeout), elapsed 68.5".
//
// ПРИЧИНА. Гейт автозагрузки (R325) вызывает ensureModelLoadedOnBackend, а тот
// внутри вызывает executeLlamaCppLoad с бюджетом ожидания загрузки 180 с. Модель
// на этом стенде поднимается 140–260 с, то есть бюджет исчерпывается РАНЬШЕ
// готовности, и наружу уходит ошибка «model load failed» без причины.
//
// ПРАВИЛО. Ошибка загрузки не означает «модели нет»: пока воркер держит модель в
// состоянии loading, клиент должен ждать — ровно как на handler-путях
// (LB_AUTO_LOAD_WAIT_SEC, по умолчанию 900 с). Отказ отдаём только если модель
// так и не стала ready за бюджет.
package balancer

import (
	"context"
	"strings"
	"time"
)

// modelReadyPollIntervalR325_1 — как часто спрашиваем воркер о готовности.
// 2 с: /api/models на воркере дешёвый, а спешить некуда — это ожидание загрузки.
const modelReadyPollIntervalR325_1 = 2 * time.Second

// waitModelReadyForAutoLoad — ждать готовности модели в пределах штатного
// бюджета автозагрузки.
//
// Возвращает true, если модель стала ready. false — бюджет исчерпан, отменён
// клиент или модели так и нет.
func (p *Proxy) waitModelReadyForAutoLoad(ctx context.Context, backendID, model string) bool {
	if p == nil || p.llamaCppRouter == nil || backendID == "" || model == "" {
		return false
	}
	budget := p.autoLoadWaitTimeoutForModel(model)
	if budget <= 0 {
		// Явно отключённое ожидание — не наше дело удлинять его.
		return p.llamaCppRouter.isModelReadyOnBackend(backendID, model)
	}
	deadline := time.Now().Add(budget)
	for {
		if p.llamaCppRouter.isModelReadyOnBackend(backendID, model) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(modelReadyPollIntervalR325_1):
		}
	}
}

// isModelMissingLoadError — R325.2: ошибка загрузки означает «модели нет», а не
// «подожди». Воркер отвечает 404 и текстом model not found; ждать такую модель
// бессмысленно (живой факт: 900 с ожидания перед 503 при мгновенном 404 воркера).
func isModelMissingLoadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"404", "model not found", "not found", "model file not found",
		"no such file", "не найдена", "не найден",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}