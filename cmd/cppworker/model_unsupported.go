// model_unsupported.go — R697 (2026-11-24): «сборка llama.cpp не знает эту архитектуру»
// как ОТДЕЛЬНЫЙ, внятный ответ воркера.
//
// ЖИВОЙ СЛУЧАЙ (стенд, cppworker gpu-r660, файл Inkling-2-Mini-UD-Q4_K_M.gguf):
//
//	заголовок GGUF: general.architecture="inkling2", 48 блоков, 734 тензора, 15.5 GiB;
//	POST /api/models/load → 500 «load failed: load model failed: failed to load model
//	from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf»;
//	llama.cpp в логе: unknown model architecture: 'inkling2'.
//
// То есть причина была, но клиент (WebUI, Cline, OpenWebUI) и оператор видели ПУТЬ к
// файлу в роли причины. Такой ответ заставляет проверять целостность файла и память —
// то есть искать не там: дело в таблице архитектур сборки (src/llama-arch.cpp,
// LLM_ARCH_NAMES), которая обновляется только вместе с сабмодулем c/llama.cpp.
//
// ПОЧЕМУ КОД ОТДЕЛЬНЫЙ, А НЕ gguf_incompatible. gguf_incompatible уже значит «файл не
// принят» и на практике указывает на битый/чужой файл. Здесь файл исправен и свежее
// движка: лечится обновлением сабмодуля и пересборкой образа, а не проверкой файла.
//
// ОБРАЗЕЦ — вердикт совместимости imageworker (R696, cmd/sdworker/handlers_model.go):
// отказ по СОДЕРЖИМОМУ файла приходит сразу, с машинным кодом, текстом причины и
// подсказкой, что делать. Форму ответа читают балансер (он проксирует статус и тело
// как есть, internal/api/gguf_backend_proxy.go) и WebUI (gguf-api.js показывает поле
// error), поэтому новых полей протокола запросов не появляется.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// modelUnsupportedCodeR697 — машинный код ответа. Совпадает с кодом причины
// (LoadFailureModelUnsupported), чтобы клиенту не приходилось держать два словаря.
const modelUnsupportedCodeR697 = LoadFailureModelUnsupported

// modelUnsupportedMarkersR697 — как llama.cpp сообщает «такой архитектуры нет».
//
// Обе формулировки живут в c/llama.cpp: `unknown model architecture: '<arch>'`
// (src/llama-model.cpp, llama_model_create — архитектура не нашлась в LLM_ARCH_NAMES)
// и `unsupported model architecture: '<arch>'` (там же, ветка default в
// llama_model_mapping — когда LLM_ARCH_* есть, а реализации нет). Для оператора это
// одно и то же: сборка не умеет эту архитектуру.
var modelUnsupportedMarkersR697 = []string{
	"unknown model architecture",
	"unsupported model architecture",
}

// isModelUnsupportedText — текст ошибки это вердикт «архитектура не поддерживается».
func isModelUnsupportedText(text string) bool {
	low := strings.ToLower(text)
	for _, marker := range modelUnsupportedMarkersR697 {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// modelUnsupportedArchFromText — имя архитектуры из строки llama.cpp.
//
// Формулировка движка стабильна (`... architecture: 'inkling2'`), поэтому разбираем
// её, а не гадаем по имени файла: имя файла — намерение автора модели, архитектура в
// заголовке — факт.
func modelUnsupportedArchFromText(text string) string {
	low := strings.ToLower(text)
	for _, marker := range modelUnsupportedMarkersR697 {
		idx := strings.Index(low, marker)
		if idx < 0 {
			continue
		}
		rest := text[idx+len(marker):]
		// Между маркером и именем стоит ": '" (движок), но допускаем и пробелы.
		if q := strings.IndexAny(rest, "'\""); q >= 0 {
			rest = rest[q+1:]
			if end := strings.IndexAny(rest, "'\""); end > 0 {
				return strings.TrimSpace(rest[:end])
			}
			continue
		}
		if colon := strings.Index(rest, ":"); colon >= 0 {
			name := strings.TrimSpace(strings.Trim(rest[colon+1:], " \t\r\n"))
			if name != "" {
				return name
			}
		}
	}
	return ""
}

// knownArchSupportR697 — когда архитектура появилась в upstream llama.cpp (номер
// сборки), если это известно.
//
// ЗАЧЕМ. Подсказка «обновите сабмодуль» без номера версии заставляет оператора
// искать её самому, а «обновите до b12642» — конкретное действие. Таблица заполняется
// вручную и только по факту релиза upstream: пустое значение означает «неизвестно», и
// подсказка остаётся общей. Inkling 2 (`inkling2`, `inkling2_loop`) появился в
// llama.cpp b12642 (PR #32187/#32217, looped-архитектура — #32695); в пине
// b1b770d90 (deps: .env CPPWORKER_LLAMA_BUILD) его ещё нет.
var knownArchSupportR697 = map[string]string{
	"inkling2":      "b12642",
	"inkling2_loop": "b12642",
}

// modelUnsupportedBuildR697 — сборка llama.cpp этого воркера (факт из бинаря).
// Пустая строка — старый образ без запечённого коммита: не выдумываем версию.
func modelUnsupportedBuildR697() string {
	return strings.TrimSpace(currentLlamaBuildFacts().Binary)
}

// modelUnsupportedBuildLabelR697 — как назвать сборку в тексте.
func modelUnsupportedBuildLabelR697(build string) string {
	if build == "" {
		return i18n.T("cppworker.load_failure.build_unknown")
	}
	return build
}

// modelUnsupportedMessageR697 — человеческая причина для клиента.
func modelUnsupportedMessageR697(arch, build string) string {
	if arch == "" {
		arch = i18n.T("cppworker.load_failure.arch_unknown")
	}
	return i18n.T("cppworker.load_failure.model_unsupported",
		arch, modelUnsupportedBuildLabelR697(build))
}

// modelUnsupportedHintR697 — что делать оператору.
//
// Подсказка НЕ обещает, что «обновить» = «заработает»: обновление сабмодуля тянет за
// собой мост (c/bridge) и раскладку RPC, поэтому в подсказке назван и второй шаг —
// пересборка образа.
func modelUnsupportedHintR697(arch string) string {
	hint := i18n.T("cppworker.load_failure.model_unsupported_hint", arch)
	if since := knownArchSupportR697[strings.ToLower(strings.TrimSpace(arch))]; since != "" {
		hint += " " + i18n.T("cppworker.load_failure.model_unsupported_since", since)
	}
	return hint
}

// modelUnsupportedDiagnosticsR697 — числа/факты для разбора (уходят в load_failure
// и в /api/models/load/progress).
func modelUnsupportedDiagnosticsR697(arch, build string) map[string]interface{} {
	diag := map[string]interface{}{
		"decision": "model_unsupported",
		"hint":     modelUnsupportedHintR697(arch),
	}
	if arch != "" {
		diag["arch"] = arch
	}
	if build != "" {
		diag["llama_build"] = build
	}
	if since := knownArchSupportR697[strings.ToLower(strings.TrimSpace(arch))]; since != "" {
		diag["arch_supported_since"] = since
	}
	return diag
}

// writeModelUnsupportedResponseR697 — ответ 409 с причиной и подсказкой.
//
// СТАТУС 409, а не 500. 5xx значит «воркер сломался, повтори» — и клиенты (Cline,
// openai-python, OpenWebUI) уходят в повторы, а оператор ищет поломку воркера. Это
// отказ по СОДЕРЖИМОМУ файла: повтор не поможет, нужен новый образ. Тот же выбор
// сделан у imageworker (R696, sdworker/handlers_model.go: bundle_unsupported → 409).
//
// Тело повторяет остальные отказы воркера: error — человеческий текст (его показывают
// клиенты), code/reason — машинный код, model/arch/llama_build — факты, hint — что
// делать, raw_error — исходный текст вместе со строкой llama.cpp.
func writeModelUnsupportedResponseR697(w http.ResponseWriter, modelName string, loadErr error, arch string) {
	build := modelUnsupportedBuildR697()
	body := map[string]interface{}{
		"error":       modelUnsupportedMessageR697(arch, build),
		"code":        modelUnsupportedCodeR697,
		"reason":      modelUnsupportedCodeR697,
		"model":       modelName,
		"decision":    "model_unsupported",
		"hint":        modelUnsupportedHintR697(arch),
		"llama_build": build,
	}
	if arch != "" {
		body["arch"] = arch
	}
	if loadErr != nil {
		body["raw_error"] = loadErr.Error()
	}
	writeJSON(w, http.StatusConflict, body)
}

// recordLoadFailureR697 — записать провал загрузки с причиной и диагностикой.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ВХОД. Прежний loadFailures.record(name, err) классифицировал только
// по тексту, поэтому /api/models (поле load_failure → agent → балансер → WebUI)
// отдавал причину без имени архитектуры и без сборки. Для model_unsupported эти два
// факта и есть ответ, поэтому они кладутся в diagnostics.
func recordLoadFailureR697(modelName string, loadErr error) {
	if loadErr == nil {
		return
	}
	reason := classifyLoadFailureReason(loadErr, loadErr.Error())
	var diag map[string]interface{}
	if reason == LoadFailureModelUnsupported {
		arch := modelUnsupportedArchFromText(loadErr.Error())
		diag = modelUnsupportedDiagnosticsR697(arch, modelUnsupportedBuildR697())
	}
	loadFailures.recordDetailed(modelName, reason, loadErr, diag)
}

// respondModelUnsupportedR697 — единая точка для путей загрузки: если движок сказал
// «такой архитектуры нет», ответ уже записан (true).
//
// Вызывается ПОСЛЕ обычной записи в лог и ДО общего 500: остальные провалы загрузки
// обязаны остаться 500 с прежним текстом (их разбор — не предмет этой правки).
func respondModelUnsupportedR697(w http.ResponseWriter, modelName string, loadErr error) bool {
	if loadErr == nil || !isModelUnsupportedText(loadErr.Error()) {
		return false
	}
	arch := modelUnsupportedArchFromText(loadErr.Error())
	build := modelUnsupportedBuildR697()
	recordLoadFailureR697(modelName, loadErr)
	logger.Get().Errorw("load refused: llama.cpp does not know the model architecture",
		"name", modelName, "arch", arch, "llama_build", build,
		"summary", modelUnsupportedSummaryR697(modelName, arch, build))
	writeModelUnsupportedResponseR697(w, modelName, loadErr, arch)
	return true
}

// loadFailureHintR697 — подсказка из записи реестра для поверхностей, которые показывают
// причину словами (progress, /api/models). Пустая строка = подсказки нет.
func loadFailureHintR697(reason string, diag map[string]interface{}) string {
	if reason != LoadFailureModelUnsupported || diag == nil {
		return ""
	}
	if hint, ok := diag["hint"].(string); ok {
		return hint
	}
	return ""
}

// modelUnsupportedSummaryR697 — одна строка для лога воркера: модель, архитектура,
// сборка. Лог пишется даже когда клиент уже ушёл.
func modelUnsupportedSummaryR697(modelName, arch, build string) string {
	return fmt.Sprintf("model %q: architecture %q is not supported by llama.cpp build %q",
		modelName, arch, modelUnsupportedBuildLabelR697(build))
}

// failedLoadProgressR697 — тело ответа /api/models/load/progress для провалившейся
// загрузки: прежние поля (name/state/error/failedAt) плюс код причины и подсказка.
//
// ЗАЧЕМ. Поллер (WebUI через балансер) видел state="failed" и текст, но не код: чтобы
// показать «архитектура не поддерживается», ему пришлось бы разбирать строку
// llama.cpp. Код и подсказка едут отдельными полями — форма ответа расширяется, старые
// потребители продолжают читать error/failedAt.
func failedLoadProgressR697(name string, fe loadFailureEntry) map[string]interface{} {
	out := map[string]interface{}{
		"name":     name,
		"state":    "failed",
		"error":    fe.Err,
		"failedAt": fe.At.UTC().Format(time.RFC3339Nano),
	}
	if fe.Reason != "" {
		out["reason"] = fe.Reason
	}
	if hint := loadFailureHintR697(fe.Reason, fe.Diagnostics); hint != "" {
		out["hint"] = hint
	}
	return out
}
