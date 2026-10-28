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

// agentRequiredWindow — необходимое окно НА КЛИЕНТА для агентского хода.
//
// Budget: клиентский n_predict, если он его назвал (агентские клиенты ставят
// max_tokens на длинные правки файлов), иначе дефолт оператора.
func agentRequiredWindow(meta *RequestMeta) int {
	if meta == nil || meta.EstimatedPromptTokens <= 0 {
		return 0
	}
	budget := meta.RequestedNPredict
	if budget <= 0 {
		budget = agentOutputReserveTokens()
	}
	return meta.EstimatedPromptTokens + budget + agentWindowSlackTokens
}
