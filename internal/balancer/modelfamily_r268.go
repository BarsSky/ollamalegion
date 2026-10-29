// modelfamily_r268.go — R268 (2026-10-28): подключение реестра семейств к
// тракту балансера.
//
// ЧТО ЗДЕСЬ. Тонкая склейка, без бизнес-логики: определить семейство запроса,
// решить, можно ли конвертировать ответ в Ollama API, и — если нельзя — отдать
// ОДИН понятный отказ вместо испорченного ответа.
//
// ПОЧЕМУ ОТКАЗ ТОЛЬКО НА ПУТЯХ КОНВЕРТАЦИИ. На `/v1/*` балансер ничего не
// преобразует: ответ llama.cpp идёт клиенту как есть, и «неподдержанная версия»
// там не мешает (клиент сам разбирает формат). Отказ на passthrough-порту был бы
// запретом на исправную работу — поэтому решение принимается по пути запроса,
// а не «вообще».
package balancer

import (
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/modelfamily"
	"ollama-loadbalancer/pkg/logger"
)

// defaultOpenAIPassthroughPort — порт OpenAI-совместимого passthrough, который
// называется оператору в отказе, если в конфиге он не задан (0).
//
// Число совпадает с дефолтом поставки (LB_OPENAI_PORT / config: openAIPort).
const defaultOpenAIPassthroughPort = 18079

// isOllamaConversionPath — конвертирует ли балансер ответ на этом пути в Ollama
// API (NDJSON). Именно там диалект семейства обязан быть понят, иначе метки
// шаблона и вызовы уедут клиенту текстом (живой дефект Cline).
func isOllamaConversionPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}

// openAIPassthroughPort — порт для текста отказа.
func (p *Proxy) openAIPassthroughPort() int {
	if p != nil && p.config != nil && p.config.LoadBalancer.OpenAIPort > 0 {
		return p.config.LoadBalancer.OpenAIPort
	}
	return defaultOpenAIPassthroughPort
}

// resolveModelFamily — опознать семейство для запроса.
//
// meta (tools/endpoint) передаётся потому, что поведение одного и того же файла
// зависит от контекста: с tools[] детектор обязан быть включён, без них он только
// мешает. Сейчас семейства это игнорируют, но контракт уже есть — иначе через
// месяц пришлось бы менять интерфейс.
func (p *Proxy) resolveModelFamily(model string, hasTools bool, endpoint string) modelfamily.Resolution {
	return modelfamily.Resolve(model, modelfamily.Meta{HasTools: hasTools, Endpoint: endpoint})
}

// refuseUnsupportedModelFamily — ранний отказ на пути КОНВЕРТАЦИИ.
//
// converting=false означает passthrough: ответ отдаётся как есть (нативный
// Ollama-путь или /v1/*), и «неподдержанная версия» там ничему не мешает —
// отказывать на исправной работе нельзя.
//
// Возвращает true, если запрос ОТКЛОНЁН (и ответ уже записан). Отказ ставится
// ДО обращения к модели: гонять многосекундную генерацию ради ответа, который мы
// всё равно не сможем преобразовать, бессмысленно — и оператор ждал бы впустую.
func (p *Proxy) refuseUnsupportedModelFamily(w http.ResponseWriter, originalPath, model string, hasTools, converting bool) bool {
	if !converting {
		return false
	}
	res := p.resolveModelFamily(model, hasTools, originalPath)
	if res.Status != modelfamily.UnsupportedVersion {
		return false
	}

	text := modelfamily.RefusalText(model, res, p.openAIPassthroughPort())
	// Одна строка на отказ: в ней есть модель, семейство, версия, диапазон и
	// порт. Без этого следующий разбор снова начинался бы с догадок.
	logger.Get().Warnw("model family not supported for Ollama conversion",
		"backend_path", originalPath,
		"model", model,
		"family", familyID(res),
		"detected_version", res.Version,
		"supported_range", supportedRange(res),
		"openai_port", p.openAIPassthroughPort(),
		"reason", res.Reason)
	writeStreamErrorChunk(w, originalPath, model, text)
	return true
}

// logModelFamily — debug-строка «кто опознан» на каждый запрос конвертации.
//
// ЗАЧЕМ. До R268 по логам нельзя было понять, каким диалектом балансер
// обслуживал модель: оставалось гадать по симптомам («метки в ответе», «вызов
// уехал текстом»). Одна строка снимает этот вопрос.
func (p *Proxy) logModelFamily(originalPath, model string, hasTools bool) {
	if !isOllamaConversionPath(originalPath) {
		return
	}
	res := p.resolveModelFamily(model, hasTools, originalPath)
	caps := modelfamily.Capabilities{}
	if res.Family != nil {
		caps = res.Family.CapabilitiesFor(res.Major)
	}
	logger.Get().Debugw("model family resolved",
		"model", model,
		"family", familyID(res),
		"version", res.Version,
		"status", res.Status.String(),
		"native_tools", caps.NativeTools,
		"reasoning_channel", caps.ReasoningChannel,
		"tool_text_form", caps.ToolCallTextForm,
		"has_tools", hasTools)
}

func familyID(res modelfamily.Resolution) string {
	if res.Family == nil {
		return modelfamily.GenericID
	}
	return res.Family.ID()
}

func supportedRange(res modelfamily.Resolution) string {
	if res.Family == nil {
		return "любая"
	}
	return res.Family.Versions().String()
}

// modelFamilyPreference — каким диалектом детектору стоит начать разбор.
//
// Возвращает пустую строку, если предпочтения нет (generic/неизвестное): тогда
// детектор работает прежним порядком, и поведение не меняется.
func (p *Proxy) modelFamilyPreference(model string, hasTools bool, endpoint string) string {
	res := p.resolveModelFamily(model, hasTools, endpoint)
	if res.Family == nil {
		return ""
	}
	return res.Family.CapabilitiesFor(res.Major).ToolCallTextForm
}

// compile-time подсказка: путь /v1 и /api различаются по префиксу, никакой магии.
var _ = fmt.Sprintf
