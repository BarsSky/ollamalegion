// one_inference_r493.go — R493-12q (2026-11-13, этап 5 плана «два мозга»):
// ОДИН ПУТЬ ИНФЕРЕНСА — паритет R86 на `/api/chat` (флаг LB_ONE_INFERENCE).
//
// ЖИВОЙ ФАКТ (план §1, таблица «LlamaCppRouter.handleChat недостижим по
// /api/chat»). Инструмент генерации изображений (R84/R86) объявлялся и
// исполнялся ТОЛЬКО на handler-пути: `/v1/chat/completions`
// (`handleOpenAIChatCompletions` → `injectImageTool`) и `/ollama/api/chat`
// (`handleChat` → `injectImageToolOllama` + `runImageToolLoopOllama`). На
// «голом» `/api/chat`, который идёт generic-путём, инструмента не было вовсе —
// то есть Open WebUI в штатном режиме («модель не видит инструменты») и Cline
// через `/api/chat` получали разное поведение от одного и того же кластера в
// зависимости от того, какой URL прописан у клиента.
//
// ЧТО ЗДЕСЬ. Паритет достигается не копированием: generic-путь зовёт ТУ ЖЕ
// подготовку и ТОТ ЖЕ цикл, что и `/ollama/api/chat` (`injectImageToolOllama`,
// `runImageToolLoopOllama`). Признак поверхности — путь запроса, а не
// «догадка по телу»: на Ollama-чате объявляется Ollama-форма инструментов, на
// OpenAI-поверхности — OpenAI-форма (её уже даёт handler-путь).
//
// ПОЧЕМУ ЗА ФЛАГОМ. Это первый шаг этапа 5, а не весь этап: единый
// `runInference()` (транспорт, стрим, сводки) ещё не сведён. Флаг
// `LB_ONE_INFERENCE` = 1 включает паритет фич на generic-пути; 0 — историческое
// поведение, где `/api/chat` инструмента не объявляет.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// oneInferenceEnabledR493 — включён ли единый путь инференса.
//
// Один разбор значения (settings: панель → env → умолчание реестра).
func oneInferenceEnabledR493() bool {
	v, ok := settings.HotInt("LB_ONE_INFERENCE")
	return ok && v == 1
}

// Причины, по которым generic-путь инструмент не объявляет. Нужны, чтобы
// «почему /api/chat не видит инструменты» отвечалось строкой лога, а не чтением
// исходников (ровно так же, как у handler-пути — R86).
const (
	oneInferenceSkipDisabledR493   = "one_inference_disabled"
	oneInferenceSkipSurfaceR493    = "surface_has_no_ollama_chat"
	oneInferenceSkipNoClientR493   = "inject_mode_client_sent_no_tools"
	oneInferenceSkipNoTargetR493   = "no_image_tool_target"
	oneInferenceSkipClientOwnsR493 = "client_owns_tool"
)

// chooseGenericImageToolR493 — решение «объявить ли наш инструмент в ЭТОМ теле
// на generic-пути» и само тело.
//
// Возвращает (новое_тело, имена_добавленных, цель, причина_отказа). Тело
// меняется ТОЛЬКО когда инструмент реально объявлен: без image-бэкенда (или при
// выключенном инструменте) путь остаётся байт-в-байт прежним.
//
// Отдельная функция, а не «сразу цикл», чтобы решение проверялось без сети: цикл
// делает запросы к воркеру и image-воркеру.
func (p *Proxy) chooseGenericImageToolR493(ctx context.Context, surface, model string, body []byte) ([]byte, []string, *imageToolTarget, string) {
	if p == nil {
		return body, nil, nil, oneInferenceSkipDisabledR493
	}
	if !oneInferenceEnabledR493() {
		return body, nil, nil, oneInferenceSkipDisabledR493
	}
	// Поверхность определяется ПУТЁМ: на Ollama-чате объявляется Ollama-форма
	// инструментов. Для остальных путей generic-пути (generate, embed) цикла нет
	// и на handler-пути — там объявлять нечего.
	if !isOllamaChatSurfaceR493(surface) {
		return body, nil, nil, oneInferenceSkipSurfaceR493
	}
	tgt, skip := p.imageToolDecision(ctx)
	if tgt == nil {
		return body, nil, nil, skip
	}
	clientTools := countClientTools(body)
	if !imageToolShouldInject(p.imageToolSettings(), clientTools) {
		return body, nil, nil, oneInferenceSkipNoClientR493
	}
	injected, added := injectImageToolOllama(body, tgt)
	if len(added) == 0 {
		// Инжектор не добавил ничего: чаще всего клиент объявил свой инструмент с
		// тем же именем — молча подменять его нельзя.
		return body, nil, nil, oneInferenceSkipClientOwnsR493
	}
	return injected, added, tgt, ""
}

// modelFromCtxOrBodyR493 — модель запроса: сначала из контекста (её кладёт
// роутер при выборе бэкенда), иначе из тела.
//
// ПОЧЕМУ НЕ ОДНО ИСТОЧНИКОВ. На generic-пути контекст заполняется не на всех
// ветках (read-only/магия алиасов), а для объявления инструмента нужна именно та
// модель, что уйдёт воркеру. Пустая модель — не повод объявлять инструмент:
// image-инструмент привязан к текстовой модели (см. imageToolTargetFor).
func modelFromCtxOrBodyR493(r *http.Request, body []byte) string {
	if r != nil {
		if m, ok := r.Context().Value(modelContextKey).(string); ok && strings.TrimSpace(m) != "" {
			return m
		}
	}
	var req struct {
		Model string `json:"model"`
	}
	if len(body) > 0 && json.Unmarshal(body, &req) == nil {
		return strings.TrimSpace(req.Model)
	}
	return ""
}

// isOllamaChatSurfaceR493 — путь, на котором generic-путь обязан вести себя как
// Ollama-чат.
//
// Алиас `/ollama/api/chat` нормализуется роутером (R479) и сюда не доходит, но
// проверка терпима к обоим написаниям: если нормализация когда-нибудь изменится,
// паритет не пропадёт молча.
func isOllamaChatSurfaceR493(path string) bool {
	p := strings.TrimSuffix(path, "/")
	return p == ollamaChatPath || strings.HasSuffix(p, "/ollama"+ollamaChatPath)
}

// genericImageToolLoopR493 — объявить инструмент и исполнить вызов на
// generic-пути тем же циклом, что и handler-путь.
//
// Возвращает (ответ_записан, тело_для_обычного_пути):
//   - (true, nil) — цикл ответил клиенту, штатный прокси-путь не нужен;
//   - (false, injected) — инструмент объявлен, но первый шаг цикла не удался:
//     запрос обслуживается обычным путём с ТЕМ ЖЕ телом, что у handler-пути
//     (объявленный инструмент остаётся, заголовок X-Image-Tool честен);
//   - (false, nil) — инструмент не объявлялся: тело не менялось.
func (p *Proxy) genericImageToolLoopR493(w http.ResponseWriter, r *http.Request, backendID, model string, body []byte) (bool, []byte) {
	injected, added, tgt, skip := p.chooseGenericImageToolR493(r.Context(), r.URL.Path, model, body)
	if tgt == nil || len(added) == 0 {
		if skip != "" && skip != oneInferenceSkipDisabledR493 {
			logImageToolDecision(r.URL.Path, model, countClientTools(body), false, nil,
				imageToolSkipReason(skip))
		}
		return false, nil
	}
	if p.llamaCppRouter == nil {
		// Роутера нет (лёгкая сборка/тест): объявленный инструмент без исполнителя
		// отвечал бы клиенту вызовом, который никто не выполнит, а заголовок
		// X-Image-Tool врал бы. Откатываемся на исходное тело ДО объявления.
		logger.Get().Warnw("R493-12q: image tool объявлен, но исполнителя нет — откат на исходное тело",
			"path", r.URL.Path, "model", model)
		return false, nil
	}
	// Наблюдаемость: клиент и лог видят, что инструменты объявлены (то же, что на
	// handler-пути). Ставится ПОСЛЕ проверки исполнителя: заголовок не должен
	// обещать инструмент, которого в этом запросе не будет.
	if w != nil {
		w.Header().Set("X-Image-Tool", strings.Join(added, ","))
	}
	logImageToolDecision(r.URL.Path, model, countClientTools(body), true, added, "")
	if p.llamaCppRouter.runImageToolLoopOllama(w, r, imageToolLoopArgsOllama{
		bodyBuf:    injected,
		model:      model,
		backendID:  backendID,
		backendURL: p.backendHTTPAddrByID(backendID),
		target:     tgt,
	}) {
		return true, nil
	}
	logger.Get().Warnw("R493-12q: цикл инструмента не справился с первым шагом — обычный путь",
		"path", r.URL.Path, "model", model)
	return false, injected
}
