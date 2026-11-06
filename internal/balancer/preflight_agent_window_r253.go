package balancer

// preflight_agent_window_r253.go — R253 (2026-10-28): окно n_ctx растёт под
// АГЕНТСКИЙ ход, даже если клиент не прислал num_ctx.
//
// ЖИВОЙ ДЕФЕКТ. Окно росло только по запросу клиента: DecidePreflight на пустом
// meta.RequestedNCtxOverride отвечал NoOp («клиент не сообщил окно — не гадаем»).
// Cline num_ctx не присылает вовсе, поэтому окно не росло НИКОГДА, а cppworker в
// ответ режет вывод по фактическому промпту (cmd/cppworker/nctx_clamp.go):
// prompt ≥ n_ctx → n_predict=0 (пустой ответ), иначе clamp до n_ctx-prompt-64.
// Снаружи это «сначала быстро, а как упёрлось — срыв сессии».
//
// РЕШЕНИЕ. Если клиент окно не назвал, но балансер УЖЕ знает размер промпта
// (meta.EstimatedPromptTokens считается тем же путём, что и для клиентского окна),
// выводим необходимое окно НА КЛИЕНТА: промпт + бюджет вывода + slack. Дальше это
// значение проходит те же проверки, что и клиентское: физический потолок модели и
// операторский AutoReloadMaxNCtx (переиспользуем ветки (3)-(5) DecidePreflight).
//
// ГРАНИЦЫ. Никаких перезагрузок «на всякий случай»: если выведенное окно не
// больше загруженного на слот — поведение прежнее (NoOp). Всё под существующим
// LB_NCTX_PREFLIGHT_ENABLED; отдельный LB_NCTX_GROW_FOR_AGENTS (default ON) гасит
// именно новое поведение одной переменной.

import (
	"os"
	"strconv"
	"strings"
)

const (
	// agentOutputReserveDefaultTokens — сколько токенов вывода закладывать в окно,
	// если клиент не задал бюджет (Cline max_tokens не шлёт).
	agentOutputReserveDefaultTokens = 4096
	// agentOutputReserveMinTokens — нижняя граница бюджета: меньше 256 токенов на
	// ход агента бессмысленно, и «окно» из одного промпта не имело бы смысла.
	agentOutputReserveMinTokens = 256
	// agentOutputReserveMaxTokens — верхняя граница: защита от мусора в env.
	agentOutputReserveMaxTokens = 131072
	// agentWindowSlackTokens — запас на служебные токены шаблона/BOS (как в
	// cppworker: promptTokens + n_predict + slack ≤ n_ctx).
	agentWindowSlackTokens = 64
)

// nctxGrowForAgentsEnabled — R253: включено ли авто-растаскивание окна под
// агентский ход. Default ON; LB_NCTX_GROW_FOR_AGENTS=0/false/no выключает
// (возвращает прежнее поведение «растём только по запросу клиента»).
func nctxGrowForAgentsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_NCTX_GROW_FOR_AGENTS")))
	switch v {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// agentOutputReserveTokens — бюджет вывода для расчёта окна.
func agentOutputReserveTokens() int {
	raw := strings.TrimSpace(os.Getenv("LB_AGENT_OUTPUT_RESERVE_TOKENS"))
	if raw == "" {
		return agentOutputReserveDefaultTokens
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return agentOutputReserveDefaultTokens
	}
	if n < agentOutputReserveMinTokens {
		return agentOutputReserveMinTokens
	}
	if n > agentOutputReserveMaxTokens {
		return agentOutputReserveMaxTokens
	}
	return n
}

// agentOutputBudget — сколько токенов вывода закладывать в окно.
//
// R294 (2026-10-29): ЖИВОЙ ДЕФЕКТ. Бюджет брался как сырое значение
// meta.RequestedNPredict, и если клиент прислал маленький num_predict (например
// 8) — им же и ограничивался расчёт окна. Хуже: при нулевом/неразобранном
// значении в лог уходил output_budget=0, то есть окно считалось «промпт + 0»,
// и балансер мог перезагрузить модель в окно МЕНЬШЕ загруженного. Здесь бюджет
// всегда осмысленный: клиентский n_predict, но не меньше минимального (ход
// агента не бывает из 8 токенов) — иначе окно сжимается.
func agentOutputBudget(meta *RequestMeta) int {
	budget := 0
	if meta != nil {
		budget = meta.RequestedNPredict
	}
	if budget <= 0 {
		// Клиент бюджета не назвал (Cline живёт на max_output_tokens) — берём
		// дефолт оператора: ход агента без бюджета вывода бессмыслен.
		return agentOutputReserveTokens()
	}
	if budget < agentOutputReserveMinTokens {
		// Названный, но игрушечный бюджет (num_predict=8) поднимаем до
		// минимума, а не до дефолта: клиент явно ограничил ответ, и раздувать
		// окно на 4096 из-за этого нельзя (иначе лишний reload на короткий вопрос).
		budget = agentOutputReserveMinTokens
	}
	if budget > agentOutputReserveMaxTokens {
		budget = agentOutputReserveMaxTokens
	}
	return budget
}

// agentRequiredWindow — необходимое окно НА КЛИЕНТА для агентского хода.
func agentRequiredWindow(meta *RequestMeta) int {
	return agentRequiredWindowAtLeast(meta, 0)
}

// agentWindowBudgetMode — как учитывать бюджет ВЫВОДА в «нужном окне».
//
// R451 (2026-11-06): РАЗМЕР окна и БЮДЖЕТ вывода клиента — разные вещи.
//
// ЖИВОЙ ДЕФЕКТ (стенд, Cline на 9B). Первый ход прошёл на окне 32768. Второй
// (продолжение) пришёл с `max_tokens=15393`, и балансер посчитал
// «нужное окно» = оценка промпта 24677 + бюджет клиента 15393 + 64 = 40134,
// то есть на 7.3k больше загруженного окна 32768. Итог — unload+load модели на
// 243 с прямо посреди хода агента, хотя ПРОМПТ в окно влезал (24677 из 32768,
// свободно ~8k под ответ): воркер в таком случае просто урезает n_predict по
// свободному месту и отвечает.
//
// Что означает бюджет клиента: «сколько я готов принять», а не «сколько модели
// нужно места». Из-за этой подмены Cline сам себе устраивал перезагрузку:
// max_tokens он считает из объявленного окна, а окно балансер увеличивал по
// этому же max_tokens — обратная связь.
//
// minimal (default) — в окно закладывается только нижняя граница осмысленного
// хода (agentOutputReserveMinTokens, 256). Растим окно, когда не влезает САМ
// промпт (или клиент явно попросил больше, или под ответ не остаётся места) —
// то же правило, что в R447 для стримингового вердикта.
//
// client — прежнее поведение (промпт + весь бюджет клиента): нужно, если
// оператор хочет гарантированно получать полный max_tokens без обрезки и готов
// платить за это перезагрузками.
func agentWindowBudgetMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AGENT_WINDOW_BUDGET")))
	switch v {
	case "client", "full", "1", "true", "on":
		return "client"
	default:
		return "minimal"
	}
}

// agentWindowReserveTokens — сколько токенов вывода закладывать в «нужное окно».
func agentWindowReserveTokens(meta *RequestMeta) int {
	if agentWindowBudgetMode() == "client" {
		return agentOutputBudget(meta)
	}
	return agentOutputReserveMinTokens
}

// agentRequiredWindowAtLeast — то же, но выведенное окно НИКОГДА не меньше
// запрошенного клиентом и не меньше переданного «пола» (floor).
//
// R294: нижняя граница — прямое требование корректности. Клиент, попросивший
// num_ctx=16384, не должен получить перезагрузку в 4165 только потому, что
// оценка промпта оказалась маленькой. Так же и загруженное окно: рост не может
// быть «ростом в меньшую сторону» — уменьшение окна под живым клиентом рвёт его
// ход (cppworker делает unload+load).
//
// R451: слагаемое бюджета вывода — agentWindowReserveTokens (по умолчанию 256,
// а не весь max_tokens клиента; см. agentWindowBudgetMode).
func agentRequiredWindowAtLeast(meta *RequestMeta, floor int) int {
	if meta == nil || meta.EstimatedPromptTokens <= 0 {
		return floor
	}
	need := meta.EstimatedPromptTokens + agentWindowReserveTokens(meta) + agentWindowSlackTokens
	if meta.RequestedNCtxOverride > need {
		need = meta.RequestedNCtxOverride
	}
	if floor > need {
		need = floor
	}
	return need
}
