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
	"fmt"
	"net/http"
	"strconv"
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
func writeContextGrowthDeferred(w http.ResponseWriter, model string, needed, loadedPerSeq int) {
	retry := 60
	body := fmt.Sprintf(
		`{"error":"context window is smaller than the request needs",`+
			`"reason":"context_growth_pending",`+
			`"model":%q,"requiredTokens":%d,"loadedWindowTokens":%d,`+
			`"retryAfterSec":%d,`+
			`"hint":"raise the model context window (WebUI → model card) or shorten the history"}`,
		model, needed, loadedPerSeq, retry)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(body))
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
	estimated := meta.EstimatedPromptTokens
	if estimated <= 0 {
		return false, 0, 0
	}
	loaded := p.loadedPerSeqNCtx(backendID, model)
	if loaded <= 0 {
		return false, 0, 0
	}
	budget := meta.RequestedNPredict
	if budget <= 0 {
		budget = defaultOutputBudgetForGrowth
	}
	needed := estimated + budget
	if !promptNeedsMoreContext(estimated, budget, loaded) {
		return false, 0, 0
	}
	// Рост возможен только без активных стримов (guard R271): если стримы есть,
	// перезагрузку отложат, а запрос в очереди простоял бы впустую.
	if p.ActiveStreamsOnBackend(backendID) == 0 {
		return false, needed, loaded
	}
	logger.Get().Infow("context growth deferred instead of admission queue (R288)",
		"backend", backendID, "model", model,
		"estimated_prompt_tokens", estimated, "output_budget", budget,
		"needed_tokens", needed, "loaded_window_tokens", loaded,
		"active_streams", p.ActiveStreamsOnBackend(backendID),
		"retry_after_sec", 60)
	return true, needed, loaded
}

var _ = time.Second
