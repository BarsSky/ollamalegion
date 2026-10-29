// context_growth_r288.go — R288 (2026-10-29): запрос, которому нужно БОЛЬШЕЕ
// окно, не должен молча стоять в admission-очереди 900 секунд.
//
// ЖИВОЙ ДЕФЕКТ (стенд r282). Модель загружена с окном 8192. Клиент попросил
// `num_ctx=32768` во время чужого стрима: preflight не смог вырастить окно
// (модель занята), и запрос ушёл в admission-очередь с `wait_max_sec=900`.
// Для клиента это ровно «балансер ничего не отвечает»: он ждёт пустоту, хотя
// решение уже известно — окно сейчас не вырастет, ждать бессмысленно.
//
// ЧТО ЗДЕСЬ. Одна проверка ПЕРЕД ожиданием слота: если промпт запроса вместе с
// бюджетом вывода не влезает в окно ОДНОГО СЛОТА, то ожидание слота не поможет
// (cppworker всё равно урежет n_predict или ответит 413). Такому клиенту честно
// говорим «нужно больше контекста, повторите» — 503 + Retry-After, а не тишина.
//
// Чего проверка НЕ делает: не решает, можно ли вырасти (это preflight/reload),
// не трогает слоты и не перезагружает модель. Её единственная задача — не
// ставить в очередь запрос, который в этой очереди заведомо не обслужат.
package balancer

import (
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// defaultOutputBudgetForGrowth — сколько токенов вывода считать «нужными»,
// когда клиент не задал n_predict. Значение совпадает с дефолтом бюджета
// агентского роста окна (LB_AGENT_OUTPUT_RESERVE_TOKENS=4096), чтобы проверка
// и рост окна считали одно и то же: иначе балансер мог бы отказать запросу,
// который сам же и собирался обслужить.
const defaultOutputBudgetForGrowth = 4096

// promptNeedsMoreContext — влезает ли промпт в окно слота.
//
// Чистая функция, чтобы правило было одно и проверялось таблицей:
//
//	loadedPerSeq <= 0                  → false (окно неизвестно: не отказываем)
//	estimated <= 0                    → false (оценки нет: не отказываем)
//	estimated + outputBudget <= window → false (влезает, пусть стоит в очереди)
//	иначе                             → true  (ждать слот бессмысленно)
func promptNeedsMoreContext(estimatedPromptTokens, outputBudget, loadedPerSeq int) bool {
	if loadedPerSeq <= 0 || estimatedPromptTokens <= 0 {
		return false
	}
	if outputBudget < 0 {
		outputBudget = 0
	}
	return estimatedPromptTokens+outputBudget > loadedPerSeq
}

// writeContextGrowthDeferred — понятный отказ вместо 900-секундного молчания.
//
// Код 503 (а не 400/413): запрос КОРРЕКТНЫЙ, просто сейчас окно меньше нужного
// — оператор или следующий запрос после reload'а его обслужит. Retry-After
// говорит, через сколько повторять, поля actual/limit — сколько именно не
// хватает (иначе «увеличьте окно» — совет без числа).
//
// R291 (2026-10-29): формат приведён к ОДНОМУ виду «отложено» (status/deferred/
// deferredReason), общему с ручным reload и n_ctx-preflight. Живой факт: те же
// сценарии отвечали тремя разными телами, и клиент не мог их обработать одним
// правилом.
func writeContextGrowthDeferred(w http.ResponseWriter, model string, needed, loadedPerSeq int) {
	writeDeferredJSON(w, deferredBodyR291{
		DeferredReason: "context_growth_pending",
		Model:          model,
		Extra: map[string]interface{}{
			"error":              "context window is smaller than the request needs",
			"requiredTokens":     needed,
			"loadedWindowTokens": loadedPerSeq,
			"hint":               "raise the model context window (WebUI → model card) or shorten the history",
		},
	})
}

// contextGrowthBlocker — решает, надо ли отказать ДО ожидания слота.
//
// Возвращает (true, needed, loaded) если:
//   - у запроса есть оценка промпта,
//   - она с бюджетом вывода не влезает в окно слота загруженной модели,
//   - и рост окна сейчас не может быть выполнен (есть активные стримы на
//     бэкенде — reload отложен guard'ом R271, ждать слот бессмысленно).
//
// Если активных стримов нет — не вмешиваемся: preflight/reload может успеть
// вырастить окно, и обычный путь (в т.ч. очередь) должен работать как раньше.
func (p *Proxy) contextGrowthBlocker(backendID, model string, meta *RequestMeta) (bool, int, int) {
	if p == nil || meta == nil || backendID == "" {
		return false, 0, 0
	}
	loaded := p.loadedPerSeqNCtx(backendID, model)
	if loaded <= 0 {
		return false, 0, 0
	}
	budget := agentOutputBudget(meta)
	estimated := meta.EstimatedPromptTokens
	needed := 0
	// R294 (2026-10-29): ЯВНО ЗАПРОШЕННОЕ ОКНО — тоже причина «нужно больше».
	//
	// Живой дефект: клиент прислал num_ctx=16384 при загруженных 8192, но промпт
	// был коротким, поэтому проверка «промпт + бюджет не влезает» не срабатывала:
	// запрос уходил в воркер, тот отвечал кадром ошибки внутри стрима
	// (`requested n_ctx=16384 exceeds model's effective n_ctx=8192`), а на /v1
	// поток после такого кадра вообще не завершался. Сравнение с явным запросом
	// закрывает оба случая до старта стрима.
	requested := meta.RequestedNCtxOverride
	if requested > loaded {
		needed = requested
	} else if estimated > 0 && promptNeedsMoreContext(estimated, budget, loaded) {
		needed = estimated + budget
	}
	if needed == 0 {
		return false, 0, 0
	}
	if needed < requested {
		needed = requested
	}
	// Рост возможен только без активных стримов (guard R271): если стримы есть,
	// перезагрузку отложат, а запрос в очереди простоял бы впустую.
	//
	// R297 (2026-10-29): ТАКЖЕ блокируем, если перезагрузка уже идёт. Живой факт:
	// при активном стриме второй запрос ждал 40.9 с и получал 503 с
	// `active streams=0` — счётчик стримов на момент проверки уже обнулился
	// (стрим завершился/не был учтён под этим ключом), а reload был в процессе,
	// поэтому окно вырасти не могло. «Активных стримов нет» ≠ «рост возможен»:
	// возможен он только когда нет ни стримов, ни идущей перезагрузки.
	active := p.ActiveStreamsOnBackend(backendID)
	reloadPending := false
	if p.nctxReload != nil {
		reloadPending = p.nctxReload.IsReloadPending(backendID, model)
	}
	if active == 0 && !reloadPending {
		return false, needed, loaded
	}
	logger.Get().Infow("context growth deferred instead of admission queue (R288)",
		"backend", backendID, "model", model,
		"estimated_prompt_tokens", estimated,
		"requested_by_client", requested,
		"output_budget", budget,
		"needed_tokens", needed, "loaded_window_tokens", loaded,
		"active_streams", active,
		"reload_pending", reloadPending,
		"retry_after_sec", 60)
	return true, needed, loaded
}

var _ = time.Second
