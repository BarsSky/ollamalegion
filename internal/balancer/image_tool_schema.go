// image_tool_schema.go — R637 (2026-11-19): схема инструмента для клиента: OpenAI/ Ollama, имена, инъекция tools в тело.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/logger"
	"strings"
)

// imageToolOpenAITools — набор инструментов в форме OpenAI tools[].
//
// R85: при AllowLoad добавляется второй инструмент list_image_models (каталог).
// При off возвращается ровно один generate_image — прежняя проводная форма.
func imageToolOpenAITools(t *imageToolTarget) []map[string]interface{} {
	spec := imagetool.Defaults()
	spec.Sync = true
	if t != nil {
		spec.Models = t.Models
		spec.AllowLoad = t.AllowLoad
	}
	tools := imagetool.OpenAITools(spec)
	if t != nil && t.AllowLoad {
		// Семейства — из того же каталога, что и модели: enum параметра family
		// не должен предлагать семейство, которого в кластере нет.
		tools[1] = imagetool.BuildList(imagetool.ListSpec{Families: t.Families}).OpenAIFunction()
	}
	return tools
}

// imageToolOpenAI — схема generate_image в форме OpenAI tools[] (совместимость:
// используется contract-тестами и документом контракта).
func imageToolOpenAI(t *imageToolTarget) map[string]interface{} {
	return imageToolOpenAITools(t)[0]
}

// imageToolNamesHeader — значение заголовка X-Image-Tool: имена объявленных
// инструментов через запятую (в порядке схемы).
func imageToolNamesHeader(tools []map[string]interface{}) string {
	return strings.Join(imageToolNames(tools), ",")
}

// imageToolNames — имена инструментов набора (пустые пропускаются).
func imageToolNames(tools []map[string]interface{}) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := imageToolEntryName(tool); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// countClientTools — сколько инструментов прислал клиент (для диагностики).
func countClientTools(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	tools, _ := doc["tools"].([]interface{})
	return len(tools)
}

// sortStringsInPlace — сортировка строк на месте (маленькие срезы: сортировка
// вставками дешевле sort.Strings и не тянет рефлексию).
func sortStringsInPlace(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// injectImageTool — добавить инструмент в тело запроса /v1/chat/completions.
//
// НЕ НАВЯЗЫВАЕМСЯ, ЕСЛИ:
//   - tool_choice="none" — клиент явно запретил инструменты в этом запросе;
//   - клиент САМ объявил инструмент с таким именем — тогда исполняет его
//     сторона, и перехватывать вызов нельзя (иначе клиент не получит свой
//     tool_call). Для набора R85 (generate_image + list_image_models) это
//     означает: любой из них, объявленный клиентом, отменяет инъекцию ЦЕЛИКОМ —
//     частичный набор сломал бы ожидания клиента.
//
// Возвращает исходное тело, если ничего не добавлено (байт-в-байт: путь без
// image-бэкенда не должен меняться вообще).
func injectImageTool(body []byte, tools []map[string]interface{}) ([]byte, bool, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false, fmt.Errorf("parse chat body: %w", err)
	}
	if tc, ok := doc["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		return body, false, nil
	}
	existing, _ := doc["tools"].([]interface{})
	if clientOwnsImageTool(existing, tools) {
		return body, false, nil
	}
	for _, tool := range tools {
		existing = append(existing, tool)
	}
	doc["tools"] = existing
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false, fmt.Errorf("marshal chat body with tool: %w", err)
	}
	return out, true, nil
}

// clientOwnsImageTool — объявил ли клиент хотя бы один из наших инструментов.
func clientOwnsImageTool(existing []interface{}, tools []map[string]interface{}) bool {
	if len(existing) == 0 || len(tools) == 0 {
		return false
	}
	ours := make(map[string]bool, len(tools))
	for _, tool := range tools {
		switch fn := tool["function"].(type) {
		case imagetool.Tool:
			ours[fn.Name] = true
		case map[string]interface{}:
			if name, _ := fn["name"].(string); name != "" {
				ours[name] = true
			}
		}
	}
	if len(ours) == 0 {
		return false
	}
	for _, t := range existing {
		if ours[openAIToolName(t)] {
			return true
		}
	}
	return false
}

// openAIToolName — имя функции в элементе tools[] (формат OpenAI и Ollama:
// обе поверхности описывают инструмент как {"type":"function","function":{...}}).
func openAIToolName(t interface{}) string {
	entry, ok := t.(map[string]interface{})
	if !ok {
		return ""
	}
	fn, ok := entry["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// injectImageToolsOllama — добавить наши инструменты в тело /api/chat.
//
// ЧЕМ ОТЛИЧАЕТСЯ ОТ OpenAI-ПУТИ (сознательно):
//   - инструменты КЛИЕНТА СОХРАНЯЮТСЯ: в Open WebUI у пользователя обычно
//     включены свои инструменты (веб-поиск и т.п.), и «клиент что-то объявил →
//     ничего не добавляем» лишало бы его картинок;
//   - но если клиент объявил инструмент с ИМЕНЕМ ИЗ НАШЕГО НАБОРА
//     (generate_image / list_image_models) — не добавляем НИЧЕГО: исполняет его
//     сторона, а смешанный набор (наш каталог + его генерация) привёл бы к тому,
//     что часть вызовов исполняем мы, часть клиент, и никто не знает, кто;
//   - tool_choice="none" уважается: клиент явно запретил инструменты в этом
//     запросе (docs/archive/audits/AUDIT-client-api-fidelity-2026.md, C7).
//
// Возвращает (тело, имена добавленных инструментов). Пустой список = тело не
// менялось.
func injectImageToolsOllama(body []byte, tools []map[string]interface{}) ([]byte, bool, []string) {
	if len(tools) == 0 {
		return body, false, nil
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false, nil
	}
	if tc, ok := doc["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		return body, false, nil
	}

	existing, _ := doc["tools"].([]interface{})
	for _, t := range existing {
		if isImageToolName(openAIToolName(t)) {
			// Клиент объявил инструмент с нашим именем — исполняет он, не мы.
			return body, false, nil
		}
	}

	added := make([]string, 0, len(tools))
	for _, tool := range tools {
		existing = append(existing, tool)
		added = append(added, imageToolEntryName(tool))
	}
	if len(added) == 0 {
		return body, false, nil
	}
	doc["tools"] = existing
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false, nil
	}
	return out, true, added
}

// imageToolEntryName — имя инструмента в элементе tools[] в любой из двух форм
// (собранная схема imagetool.Tool или уже сериализованный map).
func imageToolEntryName(tool map[string]interface{}) string {
	switch fn := tool["function"].(type) {
	case imagetool.Tool:
		return fn.Name
	case map[string]interface{}:
		name, _ := fn["name"].(string)
		return name
	}
	return ""
}

// logImageToolDecision — ЕДИНАЯ диагностическая строка решения об инструменте.
//
// ЗАЧЕМ (живой вопрос оператора, 2026-10-06): «модель не видит инструменты» нельзя
// было объяснить без чтения исходников — отказ объявления жил на debug-уровне, а
// причина «клиент прислал tool_choice=none» вообще нигде не логировалась. Строка
// уровня INFO на каждый tools-запрос отвечает сразу: каким ПУТЁМ пришёл запрос,
// что прислал клиент, что решил балансер и почему.
func logImageToolDecision(path, model string, clientTools int, injected bool, names []string, reason string) {
	log := logger.Get()
	if log == nil {
		return
	}
	fields := []interface{}{
		"path", path,
		"model", model,
		"client_tools", clientTools,
		"injected", injected,
	}
	if len(names) > 0 {
		fields = append(fields, "injected_tools", strings.Join(names, ","))
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	switch {
	case injected:
		log.Infow(i18n.T("balancer.image_tool.injected_info"), fields...)
	default:
		// Отказ — не ошибка: чаще всего это «нечего грузить» или «клиент запретил».
		log.Infow(i18n.T("balancer.image_tool.not_injected_info"), fields...)
	}
}

// imageToolNoInjectReason — почему в этом запросе не объявляем инструмент,
// когда imageToolShouldInject вернул false (R216).
//
// Разделение важно для лога: «auto, клиент не прислал tools» и «оператор
// поставил off» — разные решения, и раньше второе выглядело как первое.
func imageToolNoInjectReason(cfg imageToolConfig) string {
	if cfg.InjectOff {
		return imageToolSkipInjectOff
	}
	return imageToolSkipNoClientTools
}

// imageToolShouldInject — объявлять ли наш инструмент в ЭТОМ запросе.
//
// R141 (2026-10-16). В режиме always (дефолт) — да, как раньше: фича
// «модель умеет генерировать картинки» доступна любому клиенту, а исполняет
// вызов сам балансер (см. image_tool_loop*).
//
// В режиме auto — только если клиент прислал хотя бы один свой инструмент.
// Причина не в экономии как таковой: клиент, не объявивший инструментов, не
// может обработать tool_calls, а воркер, увидев непустой tools, вклеивает в
// промпт ~200 токенов преамбулы и уводит модель в reasoning (замер на стенде:
// 19 → 220 prompt-токенов, has_tools=false → true). Клиент видит это как
// «нереальный tokens/s»: 220 prompt-токенов на сообщение из трёх слов.
func imageToolShouldInject(cfg imageToolConfig, clientTools int) bool {
	if cfg.InjectOff {
		return false
	}
	if !cfg.InjectAuto {
		return true
	}
	return clientTools > 0
}

// imageToolSkipReason — человеческая причина отказа объявления (для лога).
func imageToolSkipReason(skip string) string {
	switch skip {
	case imageToolSkipDisabled:
		return i18n.T("balancer.image_tool.skip_disabled")
	case imageToolSkipNoBackend:
		return i18n.T("balancer.image_tool.skip_no_backend")
	case imageToolSkipNoModels:
		return i18n.T("balancer.image_tool.skip_no_models")
	case imageToolSkipAllowLoadOff:
		return i18n.T("balancer.image_tool.skip_allow_load_off")
	case imageToolSkipClientNone:
		return i18n.T("balancer.image_tool.skip_client_none")
	case imageToolSkipClientOwns:
		return i18n.T("balancer.image_tool.skip_client_owns")
	case imageToolSkipNoClientTools:
		return i18n.T("balancer.image_tool.skip_no_client_tools")
	case imageToolSkipInjectOff:
		return i18n.T("balancer.image_tool.skip_inject_off")
	default:
		return skip
	}
}
