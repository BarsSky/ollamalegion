// tool_guard.go — R496 (2026-11-18): сторож «слабый ход агента».
//
// ЗАЧЕМ. Живая жалоба владельца: на слабой модели (Qwen3.8-9B) агентский ход
// заканчивается «сам» — модель описывает, что собирается сделать, но НЕ вызывает
// инструмент, клиент видит завершённый ответ и цикл останавливается. По телеметрии
// (summary-лог handlers_chat.go) такой ход отличим от здорового: были tools[],
// done_reason=stop, tool_calls=0, а видимого текста либо нет вовсе, либо он —
// обещание без действия.
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА (2026-11-18, выбрано из двух вариантов): по умолчанию режим
// WARN — диагноз виден оператору (лог + счётчик + заголовок ответа), догенерации
// НЕТ. Автоматическое продолжение (`retry`) включается осознанно через
// LB_TOOL_GUARD=retry и делается отдельным шагом (нужен второй проход генерации).
//
// ПОЧЕМУ НЕ «ПРОСТО ПРОМПТ». Добавка «вызывай инструмент, а не описывай план» в
// системную преамбулу меняет КАЖДЫЙ запрос и жрёт контекст на всех моделях сразу,
// включая сильные, где проблемы нет. Сторож срабатывает только на состоявшемся
// факте и только там, где модель уже ошиблась.
package main

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"ollama-loadbalancer/pkg/logger"
)

// ToolGuardModeR496 — режим сторожа.
type ToolGuardModeR496 int

const (
	// ToolGuardOffR496 — сторож выключен: прежнее поведение, ничего не считается.
	ToolGuardOffR496 ToolGuardModeR496 = iota
	// ToolGuardWarnR496 — посчитать, сказать оператору, ничего не менять (дефолт
	// после включения флагом; без флага пакет не считает ничего).
	ToolGuardWarnR496
	// ToolGuardRetryR496 — разрешено ОДНО продолжение хода (реализуется шагом 2;
	// сейчас режим честно помечает ход как retry-кандидат в логе и заголовке).
	ToolGuardRetryR496
)

// String — для логов и заголовка.
func (m ToolGuardModeR496) String() string {
	switch m {
	case ToolGuardOffR496:
		return "off"
	case ToolGuardWarnR496:
		return "warn"
	case ToolGuardRetryR496:
		return "retry"
	}
	return "unknown"
}

// toolGuardEnvR496 — имя переменной окружения.
const toolGuardEnvR496 = "LB_TOOL_GUARD"

var (
	toolGuardModeOnceR496 sync.Once
	toolGuardModeR496     ToolGuardModeR496
)

// toolGuardModeFromEnvR496 — режим из LB_TOOL_GUARD.
//
// Пусто/off/0/false → off (поведение не меняется без явного включения).
// warn → warn, retry → retry. НЕИЗВЕСТНОЕ значение → warn + ОДНО предупреждение
// в лог: как в selection_order.go, молча игнорировать настройку нельзя —
// оператор должен узнать, что его правка не применилась.
func toolGuardModeFromEnvR496() ToolGuardModeR496 {
	toolGuardModeOnceR496.Do(func() {
		raw := strings.ToLower(strings.TrimSpace(os.Getenv(toolGuardEnvR496)))
		switch raw {
		case "", "off", "0", "false":
			toolGuardModeR496 = ToolGuardOffR496
		case "warn":
			toolGuardModeR496 = ToolGuardWarnR496
		case "retry":
			toolGuardModeR496 = ToolGuardRetryR496
		default:
			toolGuardModeR496 = ToolGuardWarnR496
			logger.Get().Warnw("LB_TOOL_GUARD: неизвестное значение — включён warn",
				"value", raw, "allowed", "off|warn|retry")
		}
	})
	return toolGuardModeR496
}

// resetToolGuardModeR496 — сброс для тестов (sync.Once иначе залипнет).
func resetToolGuardModeR496() {
	toolGuardModeOnceR496 = sync.Once{}
	toolGuardModeR496 = ToolGuardOffR496
}

// Счётчики для /metrics и диагностического архива. Атомарные: их пишут горутины
// разных запросов.
var (
	toolGuardUnresolvedR496 atomic.Int64
	toolGuardCheckedR496    atomic.Int64
)

// ToolGuardCountersR496 — снимок счётчиков (checked, unresolved).
func ToolGuardCountersR496() (checked, unresolved int64) {
	return toolGuardCheckedR496.Load(), toolGuardUnresolvedR496.Load()
}

// resetToolGuardCountersR496 — для тестов.
func resetToolGuardCountersR496() {
	toolGuardCheckedR496.Store(0)
	toolGuardUnresolvedR496.Store(0)
}

// ToolGuardInputR496 — входные данные проверки. Все поля уже есть в summary-логе
// handlers_chat.go, поэтому проверка не требует новых измерений.
type ToolGuardInputR496 struct {
	// ToolsOffered — клиент передал tools[] (иначе вызывать нечего).
	ToolsOffered bool
	// ToolCalls — сколько вызовов модель всё-таки сделала.
	ToolCalls int
	// DoneReason — причина остановки: stop / length / tool_calls / error.
	DoneReason string
	// VisibleContent — видимый текст ответа (без разметки вызова).
	VisibleContent string
	// BudgetExhausted — модель упёрлась в n_predict: «пустой ответ» тогда
	// следствие бюджета, а не срыва хода (у клиента уже есть пояснения R314/R394).
	BudgetExhausted bool
}

// ToolGuardVerdictR496 — результат проверки.
type ToolGuardVerdictR496 struct {
	// Unresolved — ход окончен без действия, хотя инструменты были предложены.
	Unresolved bool
	// Reason — машинная причина: empty_answer | narration_without_action.
	Reason string
}

// narrationReR496 — «обещание действия» без самого действия.
//
// Список намеренно короткий и проверяемый: он должен ловить обещание
// («сейчас сделаю / приступаю / i'll»), а не любой текст. Применяется только к
// короткому тексту БЕЗ блока кода, иначе обычное объяснение давало бы ложное
// срабатывание.
var narrationReR496 = regexp.MustCompile(`(?i)(сейчас\s+(сделаю|создам|изменю|обновлю|добавлю|проверю|применю)|приступа(ю|аю)|сделаю\s+сейчас|i'll\s+|i\s+will\s+|let\s+me\s+now)`)

// codeFenceReR496 — наличие кода/файла в ответе: значит ответ содержательный.
var codeFenceReR496 = regexp.MustCompile(`(?s)` + "```" + `|^\s*\$ |^#!/`)

// classifyToolGuardR496 — ЧИСТАЯ функция правила. Никаких env, сети и глобального
// состояния: поэтому её можно проверить таблицей случаев.
func classifyToolGuardR496(in ToolGuardInputR496) ToolGuardVerdictR496 {
	// Инструменты не предлагали — сторожи́ть нечего.
	if !in.ToolsOffered {
		return ToolGuardVerdictR496{}
	}
	// Вызов состоялся — ход результативный.
	if in.ToolCalls > 0 {
		return ToolGuardVerdictR496{}
	}
	// Бюджет исчерпан: модель не «сорвалась», её обрезали. Продолжать нельзя.
	if in.BudgetExhausted {
		return ToolGuardVerdictR496{}
	}
	// Остановка не «stop» — значит это не «модель решила закончить».
	switch strings.ToLower(strings.TrimSpace(in.DoneReason)) {
	case "", "stop", "end_turn":
		// продолжаем проверку
	default:
		return ToolGuardVerdictR496{}
	}

	content := strings.TrimSpace(in.VisibleContent)
	if content == "" {
		return ToolGuardVerdictR496{Unresolved: true, Reason: "empty_answer"}
	}
	if len([]rune(content)) <= 600 && !codeFenceReR496.MatchString(content) &&
		narrationReR496.MatchString(content) {
		return ToolGuardVerdictR496{Unresolved: true, Reason: "narration_without_action"}
	}
	return ToolGuardVerdictR496{}
}

// observeToolGuardR496 — учёт проверки и (при срабатывании) Warn с числами хода.
// Возвращает вердикт, чтобы вызывающий успел поставить заголовки до тела.
func observeToolGuardR496(model, path string, in ToolGuardInputR496) ToolGuardVerdictR496 {
	mode := toolGuardModeFromEnvR496()
	if mode == ToolGuardOffR496 {
		return ToolGuardVerdictR496{}
	}
	toolGuardCheckedR496.Add(1)
	verdict := classifyToolGuardR496(in)
	if !verdict.Unresolved {
		return verdict
	}
	toolGuardUnresolvedR496.Add(1)
	logger.Get().Warnw("tool guard: ход окончен без вызова инструмента",
		"model", model,
		"path", path,
		"reason", verdict.Reason,
		"mode", mode.String(),
		"tool_calls", in.ToolCalls,
		"done_reason", in.DoneReason,
		"visible_len", len([]rune(in.VisibleContent)),
		"budget_exhausted", in.BudgetExhausted,
	)
	return verdict
}

// Заголовки ответа: клиент и оператор видят предупреждение, формат ответа не
// меняется (поле в JSON сломало бы строгих потребителей, R65d).
const (
	toolGuardHeaderR496       = "X-Tool-Guard"
	toolGuardHeaderReasonR496 = "X-Tool-Guard-Reason"
)

// applyToolGuardHeaderR496 — выставить заголовки, если ход не разрешён.
// В режиме off не делает ничего: на проводе ноль изменений.
func applyToolGuardHeaderR496(w http.ResponseWriter, verdict ToolGuardVerdictR496) {
	if w == nil || !verdict.Unresolved {
		return
	}
	if toolGuardModeFromEnvR496() == ToolGuardOffR496 {
		return
	}
	w.Header().Set(toolGuardHeaderR496, "unresolved")
	if verdict.Reason != "" {
		w.Header().Set(toolGuardHeaderReasonR496, verdict.Reason)
	}
}

// toolGuardFieldsForLogR496 — поля для существующего summary-лога: диагноз должен
// лежать в ОДНОЙ строке с остальными числами хода, иначе его никто не сопоставит.
func toolGuardFieldsForLogR496(verdict ToolGuardVerdictR496) []any {
	if !verdict.Unresolved {
		return nil
	}
	return []any{
		"tool_guard_unresolved", true,
		"tool_guard_reason", verdict.Reason,
	}
}

// doneReasonStringR496 — безопасное приведение done_reason из финального чанка.
// В чанке поле лежит как interface{} (Ollama-формат), и приведение "(string)"
// без проверки дало бы пустую строку вместо причины.
func doneReasonStringR496(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toolGuardCheckedLoadR496 / toolGuardUnresolvedLoadR496 — значения счётчиков для
// /metrics (Prometheus-серия, см. handlers_metrics.go).
func toolGuardCheckedLoadR496() int64 {
	checked, _ := ToolGuardCountersR496()
	return checked
}

func toolGuardUnresolvedLoadR496() int64 {
	_, unresolved := ToolGuardCountersR496()
	return unresolved
}
