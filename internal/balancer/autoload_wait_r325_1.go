// autoload_wait_r325_1.go — R325.1 (2026-10-30): таймаут загрузки модели —
// не отказ.
//
// НАЙДЕНО ЖИВОЙ МАТРИЦЕЙ API (tests/api_contract). `/api/chat` на только что
// выгруженной модели получал 503 через 180.6 с с телом
//
//	{"error":"model \"gemma-…\" is not loaded and auto-load failed: model load failed: "}
//
// — при том что в логе балансера через ту же секунду было
//
//	"executeLlamaCppLoad: model loaded successfully (recovered from HTTP timeout), elapsed 68.5".
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

// isBackendUnreachableLoadError — R325.3: ошибка загрузки из-за недоступности
// воркера. Ждать в этом случае бессмысленно: воркер не «грузится», его нет.
//
// ЖИВОЙ ФАКТ (тесты контракта): гейт ждал 180 с, чтобы вернуть
// "model is not loaded and auto-load failed: backend unreachable: … (cppworker
// did not answer /api/version within 4s)". Клиент всё это время висел на
// запросе, который был обречён с первой секунды.
func isBackendUnreachableLoadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "backend unreachable") ||
		strings.Contains(msg, "did not answer /api/version")
}

// isModelLoadFailedError — R421 (2026-11-03): загрузка вернула КОНКРЕТНУЮ
// причину отказа (битый файл, неподдерживаемая модель). Ждать «до готовности»
// нечего: воркер не в состоянии loading, модель не появится от ожидания.
//
// ЖИВОЙ ФАКТ (CI, tests/cppworker_lazy_load): мок отвечал 500
// «model file corrupted: bad magic number», а балансер ждал полный бюджет
// автозагрузки — тест шёл 180 с и получал 503 в конце. Клиент всё это время
// висел, хотя причина была известна на первой секунде.
//
// ВАЖНО: ошибка «model load failed: » с ПУСТОЙ причиной остаётся ожидаемой
// (R325.1: это таймаут executeLlamaCppLoad, модель может ещё грузиться).
func isModelLoadFailedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	for _, marker := range []string{
		"corrupted", "bad magic", "invalid model", "unsupported model",
		"unsupported architecture", "failed to load model", "failed to load the model",
		"cannot load model", "can't load model", "model file is damaged",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
