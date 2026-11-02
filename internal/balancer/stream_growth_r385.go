// stream_growth_r385.go — R385 (2026-11-01): СТРИМИНГОВЫЙ запрос с большим
// окном ПОДНИМАЕТ модель, если это возможно. Прежнее правило «стриминг окно не
// растит» отменено как противоречащее требованию оператора.
//
// ЖИВАЯ ЖАЛОБА. Клиент пришёл с `options.num_ctx=131072`, модель загружена с
// 32768, VRAM/RAM хватает, доноры свободны — а балансер обслужил запрос с
// урезанным окном (или отказал «поднимите окно в карточке модели»). Оператор:
// «правило — если запрос приходит с большим окном, модель надо перезагрузить
// под новое окно, если она его поддерживает и нет ограничений в железе или
// параллельности».
//
// ЧЕМ ЭТО БЫЛО. R301 сознательно запрещал рост на стриминге: перезагрузка под
// живым стримом рвёт стрим (R271), поэтому для streaming оставались только
// «обслужить с загруженным окном» (Served) и «отказать с числами» (Defer).
// Тесты R301 это и закрепляли — то есть правило в тестах было, а
// операторского правила не было.
//
// ЧТО ТЕПЕРЬ. Рост разрешён, но только когда он НИКОМУ не вредит:
//   - нет активных стримов и нет уже идущей перезагрузки (growthBlockersR288);
//   - модель не «primary» (у неё окно зафиксировано администратором — это его
//     решение, и перебивать его клиентским num_ctx нельзя);
//   - бэкенд не донор раскладки (своих копий он не держит, R304);
//   - окно физически возможно: GGUF-потолок, заявленное окно профиля и
//     ContextLengthMax (WindowFeasibility), плюс потолок авто-роста.
//
// Если хоть одно не выполнено — поведение прежнее: обслужить с загруженным
// окном, если промпт влезает, иначе отказ с числами и причиной.
//
// СЕМАНТИКА ОТВЕТА. Перезагрузка асинхронная (LB_NCTX_RELOAD_ASYNC), поэтому
// клиент получает 503 с Retry-After и числами «нужно/загружено», а следующий
// его запрос обслуживается уже с новым окном. Тот же контракт, что у
// не-стримингового пути: рост окна — это перезагрузка, а не мгновенное действие.
package balancer

import (
	"fmt"
	"os"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// streamGrowthEnabledR385 — выключатель роста окна на стриминге (по умолчанию
// включён: именно это и просил оператор).
func streamGrowthEnabledR385() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_NCTX_GROW_FOR_STREAM"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// streamGrowthCheckR385 — можно ли поднять модель под нужное окно ради
// стримингового запроса. Возвращает (можно, причина_отказа).
func (p *Proxy) streamGrowthCheckR385(backendID, model string, needed int) (bool, string) {
	if p == nil || model == "" {
		return false, "модель не определена"
	}
	if !streamGrowthEnabledR385() {
		return false, "рост окна на стриминге выключен (LB_NCTX_GROW_FOR_STREAM=0)"
	}
	if needed <= 0 {
		return false, "нужное окно неизвестно"
	}
	if p.isPrimaryModel(model) {
		return false, "окно модели зафиксировано администратором (primary)"
	}
	if backendID == "" {
		return false, "бэкенд не определён"
	}
	if p.cannotHostModelR304(backendID) {
		return false, "бэкенд — донор раскладки, своих копий он не держит"
	}
	// Физика и заявленное операторское окно: та же проверка, что перед ручной
	// перезагрузкой (R297), — чтобы не выгрузить модель в никуда.
	if ok, ceiling, why := p.WindowFeasibility(backendID, model, needed); !ok {
		return false, fmt.Sprintf("окно %d физически невозможно: %s (потолок %d)", needed, why, ceiling)
	}
	if ceiling := p.physicalNumCtxCeiling(model, backendID); ceiling > 0 && needed > ceiling {
		return false, fmt.Sprintf("нужно %d, потолок авто-роста %d", needed, ceiling)
	}
	return true, ""
}

// triggerStreamGrowthR385 — запустить перезагрузку модели под нужное окно.
func (p *Proxy) triggerStreamGrowthR385(backendID, model string, needed int) {
	if p == nil || model == "" || needed <= 0 {
		return
	}
	backend := p.backendByIDR383(backendID)
	if backend == nil {
		logger.Get().Warnw("R385: рост окна не запущен — запись бэкенда не найдена",
			"backend", backendID, "model", model, "needed_tokens", needed)
		return
	}
	// Адрес воркера обязателен: перезагрузка — это HTTP-вызов ему. Без проверки
	// «неполная» запись бэкенда (тест/полу-зарегистрированный узел) роняет
	// процесс паникой в http.Client, а не отвечает клиенту отказом.
	if strings.TrimSpace(backend.Host) == "" || backend.CppWorkerPort <= 0 {
		logger.Get().Warnw("R385: рост окна не запущен — у бэкенда нет адреса воркера",
			"backend", backendID, "model", model, "host", backend.Host,
			"port", backend.CppWorkerPort, "needed_tokens", needed)
		return
	}
	logger.Get().Infow("R385: стриминговый запрос требует большего окна — запускаем перезагрузку модели",
		"backend", backendID, "model", model, "needed_tokens", needed)
	p.executeAsyncReload(backendID, model, needed, backend)
}

// growthStartsMessageR385 — текст ответа клиенту: что происходит и когда
// повторить. Числа обязательны (R291/R299): «retry later» без них оператор не
// может отличить рост от вечного отказа.
func growthStartsMessageR385(model string, v growthVerdictR301) string {
	return fmt.Sprintf("model %q needs %d tokens of context; the loaded window is %d — "+
		"the balancer has started reloading the model to the requested window, retry in %d seconds",
		model, v.Needed, v.Loaded, streamGrowthRetryAfterSecR385)
}

// streamGrowthRetryAfterSecR385 — сколько подождать до повтора. Загрузка
// 27B-модели на живой машине занимает десятки секунд; 60 — верхняя граница
// обычного случая, повтор раньше просто снова получит 503 с числом.
const streamGrowthRetryAfterSecR385 = 60
