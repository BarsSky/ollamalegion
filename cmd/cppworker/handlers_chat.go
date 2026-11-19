package main

import (
	"context"
	"encoding/json"

	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Chat types (Ollama /api/chat)
// ============================================================

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// R66b (2026-09-22): тип изменён с []openAIToolCall на []ollamaToolCall.
	// Ollama-нативные клиенты (Cline CLI, ollama-python, ollama run) шлют и
	// ждут tool_calls[].function.arguments как JSON-ОБЪЕКТ, а не строку
	// (см. openai_types.go: ollamaToolCall принимает обе формы, отдаёт Ollama-форму).
	// Со старым типом запрос второго шага (assistant.tool_calls + tool-результат)
	// падал с 400 `cannot unmarshal object into ... Arguments of type string`.
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	// R66b (2026-09-22): `tool_name` — имя инструмента в tool-сообщении.
	//
	// Ollama-нативная схема для сообщения с результатом инструмента:
	//   {"role":"tool","tool_name":"editor","content":"..."}
	// (OpenAI использует `name` + `tool_call_id`). Реальный Cline CLI 3.0.64 в
	// провайдере "ollama" шлёт ОБА поля: tool_call_id И tool_name — и на втором
	// шаге агентского цикла получал 400 `invalid JSON: json: unknown field
	// "tool_name"`, из-за чего цикл обрывался ровно после первого вызова
	// инструмента (файл создавался, но ответа клиент не получал).
	ToolName string `json:"tool_name,omitempty"`
	// R65d (2026-09-20): images в сообщении (Ollama vision-запросы).
	// До R65d строгий декодер отвечал 400 `unknown field "images"` на любой
	// vision-запрос из OpenWebUI/ollama run. Текстовая часть обрабатывается
	// как обычно; сами изображения требуют mmproj в C-bridge, поэтому пока
	// принимаются и логируются (не падаем — это важнее, чем 400).
	Images []string `json:"images,omitempty"`
	// 2026-07-01: reasoning-контент для reasoning-моделей (qwen3.5, deepseek-r1,
	// gemma-4 и т.п.). В OpenAI-совместимом формате называется `reasoning_content`,
	// в Ollama — `reasoning`. Используется при non-streaming ответе, чтобы клиенты
	// могли отображать think-блок и видимый ответ раздельно.
	Reasoning string `json:"reasoning,omitempty"`

	// R106 (2026-10-11): `thinking` — reasoning-ТЕКСТ ПРЕДЫДУЩЕГО ответа
	// ассистента, который клиент присылает ОБРАТНО в истории сообщений.
	//
	// `message.thinking` — каноническое имя reasoning-поля в Ollama API
	// (docs/architecture/balancer-cppworker-contract.md §4.1). Наши поверхности отдают
	// рассуждения под обоими именами: нативный /api/chat — `reasoning`
	// (handlers_chat.go:1070,1209), трансляция SSE→NDJSON балансера — `thinking`
	// (internal/balancer/llamacpp_translate_resp.go:970), /api/generate —
	// `thinking`. Cline нормализует ответ в своё поле `thinking` и при
	// ПРОДОЛЖЕНИИ СТАРОЙ СЕССИИ отправляет историю назад вместе с ним.
	// Строгий декодер (types.DecodeJSONRequest → DisallowUnknownFields,
	// pkg/types/contract_validation.go:71) отвечал на это
	//
	//	400 {"error":"invalid JSON: json: unknown field \"thinking\""}
	//
	// то есть продолжить сессию было нельзя вообще (живая жалоба 2026-10-11:
	// Cline → балансер :18080, модель qwen3.8-27b).
	//
	// Поле ПРИНИМАЕТСЯ и в промпт НЕ попадает — как и `reasoning` выше: промпт
	// собирается из role+content. Прокинуть thinking в chat template нельзя без
	// расширения bridge.ChatMessage (c/bridge/bridge.go:1272 — только Role и
	// Content), а предыдущие рассуждения большинство шаблонов и не рендерят.
	Thinking string `json:"thinking,omitempty"`

	// ReasoningContent — то же history-echo поле в OpenAI-написании. Клиенты с
	// одним кодом на оба API (и балансер в mixed-режиме) присылают историю в
	// OpenAI-форме и на /api/chat тоже; принимаем, чтобы не ловить тот же 400.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	NumCtx      *int          `json:"num_ctx,omitempty"`
	Tools       []openAITool  `json:"tools,omitempty"`

	// ===== R65d (2026-09-20): полный Ollama-набор полей =====================
	//
	// Аудит 2026-09-20 (находка 1.1) показал: балансер транслировал /api/chat в
	// /v1/chat/completions, но Ollama-поля терялись/отвергались. После R65d
	// Ollama-пути идут в cppworker НАТИВНО (см. internal/balancer/
	// llamacpp_native_path.go), поэтому этот обработчик обязан принимать всё,
	// что реально шлют Ollama-клиенты (OpenWebUI, ollama run/python, Cline в
	// ollama-режиме), — иначе строгий декодер ниже отвечает 400
	// `unknown field "options"`.
	//
	// Options переиспользует generateOptions (types.go:11-30) — единый источник
	// истины для Ollama options.*. До R65d набор жил только в /api/generate, из-за
	// чего /api/chat молча терял top_k/repeat_penalty/seed/min_p/mirostat*.
	Options generateOptions `json:"options"`

	// KeepAlive — Ollama-семантика времени жизни модели ("5m", "0", "-1").
	// До R65d /api/chat его вообще не принимал → клиент не мог ни выгрузить
	// модель, ни продлить её жизнь.
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`

	// Format — "json", "" (свободный текст) ИЛИ объект JSON-схемы
	// (structured outputs). RawMessage, потому что Ollama допускает оба типа.
	Format json.RawMessage `json:"format,omitempty"`

	// Think — включить/выключить reasoning у thinking-моделей (qwen3, gemma-4,
	// deepseek-r1). Может быть bool или строкой ("low"/"medium"/"high").
	Think json.RawMessage `json:"think,omitempty"`

	// System/Template/Raw — Ollama-семантика переопределения промпта.
	System   string `json:"system,omitempty"`
	Template string `json:"template,omitempty"`
	Raw      bool   `json:"raw,omitempty"`

	// Truncate — обрезать историю, чтобы влезть в контекст (Ollama >= 0.1.x).
	Truncate *bool `json:"truncate,omitempty"`

	// Options для logprobs (Ollama 0.9+). Принимаем, чтобы не падать 400;
	// фактическая выдача logprobs зависит от поддержки в C-bridge.
	Logprobs    *bool `json:"logprobs,omitempty"`
	TopLogprobs *int  `json:"top_logprobs,omitempty"`

	// ToolChoice — R66b (2026-09-22): управление выбором инструмента в
	// Ollama-нативном /api/chat ("auto" | "none" | "required" | {"type":"function",...}).
	//
	// Это НЕ поле Ollama-протокола, но реальный Cline CLI (провайдер "ollama",
	// v3.0.63) ВСЕГДА шлёт его вместе с tools[] — зафиксировано захватом payload'а
	// в scripts/mock_capture_server.js:
	//
	//	{"model":..., "messages":[...], "options":{"num_ctx":32768},
	//	 "tools":[...25 функций...], "tool_choice":"auto", "stream":true}
	//
	// До этого фикса строгий декодер отвечал 400 `invalid JSON: json: unknown
	// field "tool_choice"`, Cline получал "Bad Request" на первой же итерации
	// (6 ретраев → agent_error) и не мог вызвать НИ ОДИН инструмент, то есть
	// клиент был полностью неработоспособен против cppworker.
	//
	// Семантика: "none" — tools не инжектятся в system prompt и tool_calls не
	// парсятся (клиент явно просит ответ без инструментов); "auto"/"required"/
	// конкретная функция — прежнее поведение (llama.cpp сам решает по grammar,
	// см. c/llama.cpp/common/chat.cpp: common_chat_tool_choice_parse_oaicompat).
	// OpenAI-путь /v1/chat/completions уже принимал это поле (handlers_openai.go:99).
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	// MaxOutputTokens — R69 (2026-09-23): реальный Cline (VS Code extension +
	// CLI, провайдер "ollama") шлёт ЭТО поле вместе с tools[] — как и
	// tool_choice в R66b. Ollama-протокол такого поля не знает (у него
	// options.num_predict), но строгий декодер отвечал
	//   400 {"error":"invalid JSON: json: unknown field \"max_output_tokens\""}
	// на каждом запросе: клиент показывал ошибку и не мог выполнить ни одного
	// действия (скриншот пользователя 2026-09-23).
	//
	// Принимаем как алиас: используется, если клиент НЕ задал num_predict
	// (options.num_predict) и max_tokens (top-level).
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`

	// _keepAliveDuration — результат parseKeepAlive(req.KeepAliveRaw); не из JSON.
	_keepAliveDuration time.Duration `json:"-"`
}

// toolsDisabledByChoice — true, если клиент явно запретил инструменты// (`tool_choice:"none"`). Все прочие значения ("auto", "required", объект
// конкретной функции, отсутствие поля, невалидный JSON) означают «инструменты
// разрешены» — так же ведёт себя upstream llama.cpp.
func toolsDisabledByChoice(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Объект {"type":"function",...} или что-то ещё — инструменты разрешены.
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s), "none")
}

type chatResponse struct {
	Model         string      `json:"model"`
	CreatedAt     string      `json:"created_at"`
	Message       chatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"`
	// R176 (2026-10-19): СОСТАВ ОТВЕТА нативной поверхности.
	//
	// ЧЕГО НЕ ХВАТАЛО. Не-стрим `/api/chat` отдавал model/created_at/message/done/
	// total_duration — и ни одного счётчика токенов, хотя в стриме финальный чанк
	// их отдаёт (см. ниже по файлу), а `/api/generate` отдаёт. Клиенты (OpenWebUI,
	// Cline, localai-совместимые) считают по ним цену и скорость ответа: без полей
	// «tokens/s» в UI не из чего построить, и это ровно та жалоба, из-за которой
	// разбирались с «нереальной скоростью».
	//
	// ЧЕСТНО О ГРАНИЦАХ. Счётчики здесь — результат токенизации текста (тем же
	// токенизатором модели), а не внутренние счётчики llama.cpp: мост в не-стрим
	// пути их не возвращает. Длительностей (eval_duration/prompt_eval_duration) не
	// выдумываем: в не-стриме мы их не измеряем, а подставить total_duration значило
	// бы соврать (клиент получил бы «tokens/s», посчитанный непонятно от чего).
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
	DoneReason      string `json:"done_reason,omitempty"`
}

// effectiveToolName — имя инструмента в tool-сообщении: `name` (OpenAI) или
// `tool_name` (Ollama). R66b: нужно потому, что Ollama-клиенты (Cline CLI,
// ollama-python) заполняют именно `tool_name`.
func (m chatMessage) effectiveToolName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ToolName
}

// ============================================================
// Chat handler
// ============================================================

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req chatRequest
	// R106c (2026-10-12): КЛИЕНТСКОЕ декодирование — терпимое.
	//
	// Раньше здесь стоял строгий декодер (DisallowUnknownFields), и любой
	// незнакомый ключ давал 400 «invalid JSON: json: unknown field "…"»: у
	// OpenWebUI — options.use_mmap / num_gpu / num_thread из advanced params, у
	// Cline — tool_choice/tool_name/max_output_tokens, у истории сессии —
	// thinking/reasoning_content. Настоящая Ollama такие поля игнорирует;
	// теперь так делаем и мы (имена полей пишем в Debug-лог).
	if err := decodeClientJSON(r, types.MaxStreamingBodyBytes, &req, "handleChat"); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}

	// InFlight counter: защищает активные запросы от reload-обрыва.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
	}

	// Round 18 P0.3 (2026-08-04): per-user parallel admission (fair-share).
	// 0 = unlimited (default, backward-compat). Слот acquire ДО model load —
	// иначе 100 параллельных запросов вызовут 100 model load'ов до отказа.
	userID := getUserID(r)
	if max := maxParallelPerUser(); max > 0 {
		if !backend.UserTracker().TryAcquire(userID, max) {
			logger.Get().Warnw("handleChat: user exceeded MaxParallelPerUser",
				"user_id", userID, "max", max, "model", req.Model, "remote", r.RemoteAddr)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d (in-flight requests). Wait for current requests to complete.",
					userID, max))
			return
		}
		defer backend.UserTracker().Release(userID)
	}

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking for /api/cancel.
	// ВАЖНО: используем helper setupCancelTracking, который re-bind r к child context —
	// без этого writeChatStreamResponse будет смотреть на parent (r.Context()) и не увидит
	// отмену через /api/cancel (фикс первого P0.2-бага).
	r, requestID, cancelCleanup := setupCancelTracking(r, req.Model, "cppworker-gpu", "chat")
	defer cancelCleanup()
	// R493-12a: ячейка «сколько ушло на загрузку модели» на этот запрос.
	r = r.WithContext(withLoadScopeR493(r.Context()))
	logger.Get().Debugw("handleChat: cancel tracking enabled",
		"request_id", requestID, "model", req.Model, "user_id", userID)

	// Ленивая загрузка модели
	// R83-политика (2026-10-01): окно загрузки = num_ctx запроса (если клиент
	// его указал). Раньше lazy-load брал окно из env/профиля, и клиент получал
	// модель с чужим окном, после чего шли «перезагрузки под клиента».
	//
	// R493-12a (2026-11-12): загрузка измеряется и уезжает в контекст запроса —
	// живой замер показал 40 с ожидания, которых не было ни в одном поле
	// статистики (клиент видел total_duration 1.37 с).
	actualModel, err := loadModelMeasuredR493(r.Context(), req.Model, req.Options.NumCtx)
	if err != nil {
		// R119 (2026-10-15): 503 «грузится» / 404 «нет файла» / 500 «поломка» —
		// одна точка перевода, см. writeModelLoadError. До этого ЛЮБАЯ причина
		// уходила как 500 с пустым телом.
		writeModelLoadError(w, req.Model, err)
		return
	}
	// Заголовок — до первой записи тела: после Flush он уже не уйдёт.
	markLoadHeaderR493(w, r.Context())

	// R442: дамп того, что ПРИСЛАЛ клиент (messages/tools после декодирования).
	if debugRequestDumpEnabledR442() {
		if data, mErr := json.Marshal(req); mErr == nil {
			dumpRequestBodyR442(req.Model, "/api/chat", data)
		}
	}

	// Inject tool definitions into system prompt if tools are provided
	// (R66b: ...и клиент не запретил инструменты через tool_choice:"none").
	//
	// R434 (2026-11-05): здесь БОЛЬШЕ НЕ дописываем преамбулу в system. Инструменты
	// уходят в сборку промпта (promptOpts.Tools) и сначала пробуются в самом chat
	// template; текстовая преамбула добавляется только если шаблон их не отрисовал.
	toolsEnabled := len(req.Tools) > 0 && !toolsDisabledByChoice(req.ToolChoice)
	msgs := req.Messages

	// Собираем prompt через GGUF chat template (если доступен)
	// ApplyChatTemplate использует tokenizer.chat_template из GGUF метаданных,
	// что гарантирует корректную токенизацию и избегает segfault при ручной сборке.
	//
	// R66b (2026-09-22): per-request think / format. Раньше оба поля
	// принимались, но ни на что не влияли: reasoning зависел только от
	// глобального config.EnableReasoning, а format игнорировался полностью.
	promptOpts := chatPromptOptions{
		JSONMode:   formatWantsJSON(req.Format),
		JSONSchema: jsonSchemaText(req.Format),
	}
	// R434 (2026-11-05): инструменты передаём В СБОРКУ ПРОМПТА, а не вставляем в
	// system заранее: решение «шаблон нарисовал секцию инструментов сам» или
	// «нужна текстовая преамбула» принимается там, по факту рендера шаблона.
	if toolsEnabled {
		promptOpts.Tools = req.Tools
		promptOpts.ToolChoice = req.ToolChoice
	}
	if thinkOn, thinkExplicit := thinkEnabledFromRaw(req.Think); thinkExplicit {
		promptOpts.ReasoningOverride = &thinkOn
	}
	// R248/B3: картинки. Payload'ы валидируются ДО сборки промпта, а маркер
	// проектора вставляется в сообщения — тогда chat template увидит текст с
	// маркером на месте каждой картинки, и тот же порядок уйдёт в мост.
	var visionImages [][]byte
	if payloads := chatImages(msgs); len(payloads) > 0 {
		decoded, verr := decodeVisionImages(payloads)
		if verr != nil {
			writeVisionError(w, req.Model, verr)
			return
		}
		if !backend.VisionCapable(actualModel) {
			writeVisionError(w, req.Model,
				fmt.Errorf("vision_not_supported: model %s is loaded without mmproj", actualModel))
			return
		}

		marker := backend.MMProjMarker(actualModel)
		if marker == "" {
			writeVisionError(w, req.Model,
				fmt.Errorf("vision_not_supported: model %s has no media marker", actualModel))
			return
		}
		visionImages = decoded
		msgs = injectMediaMarkers(msgs, marker)
	}
	prompt, err := buildChatPromptWithOptions(msgs, actualModel, promptOpts)
	// R442: дамп СОБРАННОГО промпта — по нему видно, попал ли в модель результат
	// инструмента и нет ли дублей.
	if err == nil && prompt != "" && debugPromptDumpEnabledR442() {
		dumpPromptToFileR442(actualModel, "/api/chat", prompt)
	}
	if err != nil {
		logger.Get().Errorw("handleChat: failed to build prompt", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "chat prompt error: "+err.Error())
		return
	}

	logger.Get().Debugw("handleChat: assembled prompt",
		"model", req.Model,
		"messages_count", len(msgs),
		"prompt_len", len(prompt),
		"has_tools", toolsEnabled,
		"json_mode", promptOpts.JSONMode,
		"think_override", promptOpts.ReasoningOverride,
		"stream", req.Stream)

	genReq := buildGenerateRequestFromChat(req, prompt)

	params := buildGenerationParams(genReq)
	// Round 6 #6: прокидываем HasTools=true чтобы увеличить резерв токенов
	// под tools-prompt в n_ctx clamp (для Ollama-пути), как и в Round 4 для
	// OpenAI handlers. Без этого tools-запросы получают 1024 токен резерва →
	// n_ctx overflow → reload-loop → 413.
	ApplyCppCtxHeaderWithOptions(r, &params, CppCtxApplyOptions{
		HasTools: toolsEnabled,
	})
	// Antiprompts для gemma/non-gemma
	params.Antiprompts = append(params.Antiprompts, defaultAntipromptsForModel(req.Model)...)
	logger.Get().Debugw("handleChat: antiprompts",
		"model", req.Model, "count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	// Round 26 v0.5.13: n_ctx overflow detection + X-Model-Context-Warning.
	// Проверяем, помещается ли prompt + n_predict в n_ctx. Если нет —
	// 1) корректируем n_predict чтобы избежать overflow (Round 18 retry-loop)
	// 2) ставим X-Model-Context-Warning header чтобы клиент (OpenWebUI/Cline/Hermes)
	//    мог показать предупреждение пользователю ДО обрыва ответа.
	// Это объясняет cutoff-баг из Round 26: при длинной беседе prompt > n_ctx,
	// reload-loop после 3 попыток → 413 + abrupt stop.
	warning, adjustedNPredict := ComputeContextWarning(req.Model, prompt, params.NPredict, params.NCtxOverride)
	if warning.NPredict != params.NPredict {
		logger.Get().Warnw("handleChat: clamping n_predict to fit n_ctx",
			"model", req.Model, "old_n_predict", params.NPredict,
			"new_n_predict", warning.NPredict, "n_ctx", warning.NCtx,
			"prompt_tokens", warning.PromptTokens, "used_pct", warning.UsedPercent)
		params.NPredict = warning.NPredict
	}
	_ = adjustedNPredict
	SetContextWarningHeader(w, warning)

	// R65d (2026-09-20): применяем keep_alive, как это делает /api/generate
	// (handlers_generate.go:353-355). До R65d /api/chat не принимал keep_alive
	// вообще, поэтому клиент (OpenWebUI, ollama run) не мог ни выгрузить модель
	// ("0"), ни продлить её жизнь ("5m") — модель жила по дефолту cppworker.
	//
	// req._keepAliveDuration заполняется в buildGenerateRequestFromChat.
	// Семантика applyKeepAlive: 0 → UnloadModel, >0 → UpdateLastUsed(+dur).
	defer func() {
		applyKeepAlive(actualModel, genReq._keepAliveDuration)
	}()

	// R248/B3: image-запросы стримингуются буферизованно (префилл libmtmd
	// синхронный): считаем ответ и отдаём валидным NDJSON из двух чанков —
	// content + done. Вместо прежнего отказа 400 на stream+картинку.
	if len(visionImages) > 0 && req.Stream {
		result, verr := generateBufferedOrImages(r.Context(), req.Model, prompt, params, toolsEnabled, visionImages)
		if verr != nil {
			if handleInferenceError(w, verr) {
				return
			}
			writeCppWorkerErrorWithBridgeInfo(w, http.StatusInternalServerError, "vision generate failed", verr)
			return
		}
		writeChatNDJSONFromResult(w, r, req.Model, result)
		return
	}

	if req.Stream {
		// При stream=true && tools!=[] стримим токены как обычно, но накапливаем
		// полный output параллельно. После завершения генерации проверяем tool_calls
		// и эмитим финальный чанк с done:true + message.tool_calls при необходимости.
		// Раньше здесь был bug: cppworker возвращал non-streaming JSON через writeJSON,
		// что ломало OpenWebUI (он ждал NDJSON чанки).
		//
		// R66b: JSON-режим (format:"json"/схема) идёт через ТОТ ЖЕ буферизованный
		// путь, что и tools: снять markdown-обёртку ```json ... ``` можно только
		// зная весь ответ целиком, поэтому для format:json поток отдаётся двумя
		// чанками (content + done) — это валидный Ollama NDJSON, клиенты
		// (OpenWebUI, ollama-python, Cline) его принимают.
		if toolsEnabled || promptOpts.JSONMode {
			// R406: альтернативный промпт БЕЗ размышлений — аварийный выход,
			// когда модель потратила весь клиентский бюджет на thinking и не
			// сказала ни слова. Строим из тех же сообщений (tools уже внедрены)
			// через native chat template c enable_thinking=false: LEGACY-путь
			// оставил бы thinking включённым по умолчанию шаблона.
			promptNoThink := ""
			if toolsEnabled {
				promptNoThink = buildChatPromptNoThinkingR406(msgs, actualModel)
			}
			writeChatStreamResponseWithTools(w, r, actualModel, prompt, params, promptOpts.JSONMode, promptOpts.ReasoningOverride, req.Tools, promptNoThink, clientSetOutputLimitChat(req.MaxTokens, req.Options.NumPredict))
			return
		}
		writeChatStreamResponse(w, r, actualModel, prompt, params, promptOpts.ReasoningOverride)
		return
	}

	start := time.Now()
	hasTools := toolsEnabled
	result, err := generateBufferedOrImages(r.Context(), req.Model, prompt, params, hasTools, visionImages)
	// 2026-06-25: записываем snapshot последнего inference для endpoint /debug/last-prompt.
	// Это помогает диагностировать случаи "prompt_exceeds_context" в Cline/OpenWebUI.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими endpoints).
	defer func() {
		recordLastPromptFromError(req.Model, "/api/chat", prompt, &params, hasTools, err)
	}()
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError → HTTP 413 (см. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
		}
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat generate failed", err)
		return
	}
	duration := time.Since(start)

	// Parse tool_calls from output if tools were provided
	resp := chatResponse{
		Model:     req.Model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Message: chatMessage{
			Role:    "assistant",
			Content: result.Output,
		},
		Done:          true,
		TotalDuration: duration.Nanoseconds(),
		// R176: счётчики токенов и причина завершения - состав, по которому клиент
		// считает цену и скорость. Считаем токенизатором модели; длительности не
		// выдумываем (в не-стриме не измеряем).
		PromptEvalCount: countTokensSafe(req.Model, prompt),
		EvalCount:       countTokensSafe(req.Model, result.Output),
		// R394: пересчитывается ниже по факту бюджета — «stop» здесь только
		// значение по умолчанию, а не утверждение о причине.
		DoneReason: "stop",
	}
	// R106c (2026-10-12): reasoning отделяем ВСЕГДА, в том числе при tools.
	//
	// ЖИВОЙ ДЕФЕКТ: с включёнными размышлениями gemma-4 отвечает
	//	<|channel>thought … <channel|>[{"name":"read_files", …}]
	// Раньше развилка была «toolsEnabled → parse tool_calls, иначе → split
	// reasoning», поэтому при tools thinking вообще не отделялся: он оставался в
	// message.content, а parseToolCallsFromOutput получал строку с
	// thinking-префиксом и часто не находил вызов. Cline показывал «команду
	// инструмента в квадратных скобках» и инструмент не вызывался.
	output := result.Output
	if rsn, cnt, has := SplitReasoningContentFromPrompt(prompt, output); has {
		resp.Message.Reasoning = rsn
		// R108 (2026-10-13): каноническое имя поля в Ollama — `thinking`
		// (docs/architecture/balancer-cppworker-contract.md §4.1). Отдаём оба имени:
		// `thinking` понимают Ollama-клиенты (Cline/OpenWebUI), `reasoning`
		// остаётся для обратной совместимости с прежними сборками.
		resp.Message.Thinking = rsn
		output = cnt
	}
	if toolsEnabled {
		// R113: схема инструментов запроса — для приведения arguments к объекту.
		if calls := parseToolCallsForRequest(output, req.Tools); len(calls) > 0 {
			// R440: дамп исходящих аргументов (CPPWORKER_DEBUG_TOOL_ARGS=1).
			logOutgoingToolCalls(req.Model, "/api/chat(nonstream)", calls, output)
			resp.Message.ToolCalls = ollamaToolCallsFromOpenAI(calls)
			resp.Message.Content = ""
		} else {
			// R106c (2026-10-12): чистим хвостовые служебные токены шаблона и в
			// NON-STREAM пути. Живой ответ на простой вопрос выглядел как
			// «ок<end_of_turn>»: cleanFinalContent вызывался только в
			// streaming-ветке, поэтому токен gemma-шаблона оставался в content.
			//
			// R113: плюс вырезаем разметку нераспознанного вызова — она не ответ
			// (в content клиента ей места нет).
			// R430 (2026-11-04): и режем по НАЧАЛУ разметки. Раньше здесь стоял
			// только stripToolCallMarkup, а он не знает переэкранированную форму
			// (`\"name\"`) и питоновскую `call:NAME(` — живая проба вернула в
			// content `[[{\"function\":{\"name\":\"editor\"…`. Теперь клиенту
			// достаётся проза + пояснение «вызов не выполнен», а сырой вывод
			// сохраняется в диагностику (R427).
			content, unparsedMarkup := nonStreamVisibleContentR430(output)
			resp.Message.Content = content
			if unparsedMarkup {
				recordUnparsedCapture(req.Model, "/api/chat", "nonstream_markup",
					result.Output, output, 0, resp.EvalCount, params.NPredict, req.Tools)
			}
		}
	} else {
		resp.Message.Content = cleanFinalContent(output)
	}

	// R66b: format:"json" — снимаем markdown-обёртку ```json ... ```, иначе
	// клиент, который делает json.Unmarshal на content, получает ошибку
	// парсинга (раньше поле format игнорировалось целиком).
	if promptOpts.JSONMode {
		resp.Message.Content = stripJSONFence(resp.Message.Content)
	}

	// R66b: think=false — клиент явно не хочет видеть рассуждения. Модель может
	// всё равно вернуть <think>/<reasoning> блок (native template), поэтому
	// вырезаем его из content и НЕ отдаём в message.reasoning.
	// R314: сколько символов размышлений пришло — считаем ДО того, как их скроют
	// (think=false). Нужно для пояснения ниже: клиент должен узнать, что видимого
	// текста нет именно потому, что бюджет ушёл в размышления.
	reasoningCharsForNotice := len([]rune(strings.TrimSpace(resp.Message.Thinking)))
	if reasoningCharsForNotice == 0 {
		reasoningCharsForNotice = len([]rune(strings.TrimSpace(resp.Message.Reasoning)))
	}

	if promptOpts.ReasoningOverride != nil && !*promptOpts.ReasoningOverride {
		if r, c, has := SplitReasoningContent(resp.Message.Content); has {
			resp.Message.Content = c
			resp.Message.Reasoning = ""
			resp.Message.Thinking = ""
			_ = r
		}
	}

	// R314 (2026-10-30): пустой ответ, когда весь бюджет ушёл в размышления.
	//
	// ЖИВОЙ ФАКТ. /api/chat (gemma-4, reasoning включён, num_predict=160) вернул
	// content:"" и 592 символа в message.thinking при done_reason:"length". Для
	// клиента это «пустой ответ»: Cline показывает «Model returned empty
	// response», оператор идёт искать поломку. У OpenAI-поверхности такое
	// пояснение есть с R261, но нативная поверхность идёт passthrough'ом и его
	// не получала.
	//
	// ПОЧЕМУ ПОСЛЕ think=false: при явном «не показывай размышления» ответ тоже
	// остаётся пустым, и объяснить это надо тем же текстом — иначе клиент с
	// think=false получает ровно то же молчание. Признак «размышления были»
	// считаем ДО того, как их скрыли.
	// R394 (2026-11-04): причина остановки в НЕ-стриме тоже обязана быть честной.
	//
	// Видимый ответ для правила берём ДО подстановки пояснения (R314 ниже):
	// пояснение — наш текст, а не ответ модели, и по нему нельзя судить об обрыве.
	visibleForReason := resp.Message.Content
	// R417 (2026-11-03): при распознанном tool_calls это НЕ «пустой ответ» —
	// пояснение рядом с вызовом инструмента клиент сохранит как сообщение
	// ассистента (живой случай: Qwen3.8-9B + read_files, stream:false).
	resp.Message.Content = nonStreamContentR417(visibleForReason,
		len(resp.Message.ToolCalls), reasoningCharsForNotice)
	// До R394 здесь было жёстко DoneReason:"stop" — даже когда модель упиралась
	// в клиентский n_predict (ровно тот случай, который описывает комментарий
	// R314: num_predict=160, 592 символа размышлений, видимого ответа нет).
	// Агентский клиент по «stop» закрывает ход и продолжения не просит.
	resp.DoneReason = toolsTerminalReasonR394(result.Output, visibleForReason, prompt,
		len(resp.Message.ToolCalls), params.NPredict, resp.EvalCount)

	// R496 (2026-11-18): сторож «слабый ход агента» — warn по умолчанию.
	//
	// Живой случай: Qwen3.8-9B на агентском клиенте заканчивает ход текстом
	// «сейчас сделаю …» без вызова инструмента, клиент видит завершённый ответ и
	// цикл останавливается. Здесь ход уже измерен (tool_calls, done_reason,
	// видимый текст), поэтому проверка детерминированная: заголовки видны клиенту,
	// Warn с числами — оператору. Догенерации в режиме warn нет (решение владельца
	// 2026-11-18); retry — отдельный шаг.
	toolGuardVerdict := observeToolGuardR496(req.Model, "/api/chat(nonstream)", ToolGuardInputR496{
		ToolsOffered:   len(req.Tools) > 0,
		ToolCalls:      len(resp.Message.ToolCalls),
		DoneReason:     resp.DoneReason,
		VisibleContent: visibleForReason,
		// Бюджет исчерпан, если модель встала по n_predict: тогда пустой ответ —
		// следствие бюджета, а не срыва хода (пояснения R314/R394 уже есть).
		BudgetExhausted: resp.EvalCount >= params.NPredict && params.NPredict > 0,
	})
	applyToolGuardHeaderR496(w, toolGuardVerdict)

	writeJSON(w, http.StatusOK, resp)
}

// emptyAnswerNoticeForNativeR314 — пояснение вместо пустого content на нативной
// поверхности (/api/chat, /api/generate).
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. Правило простое, но его надо ПРОВЕРЯТЬ без поднятого
// бэкенда: «видимого ответа нет, а размышления были» — единственный случай, когда
// подменяем пустоту. Всё остальное (пусто и размышлений нет; непустой content)
// остаётся как есть, иначе мы начнём дописывать текст в чужие ответы.
func emptyAnswerNoticeForNativeR314(content string, reasoningChars int) string {
	if strings.TrimSpace(content) != "" || reasoningChars <= 0 {
		return ""
	}
	return i18n.T("cppworker.handlers_chat.empty_answer_reasoning_only", reasoningChars)
}

// buildGenerateRequestFromChat — R65d (2026-09-20): переносит ПОЛНЫЙ набор
// Ollama-параметров из /api/chat запроса в generateRequest, чтобы
// buildGenerationParams (handlers_generate.go:130-189) применил их так же, как
// для /api/generate.
//
// До R65d /api/chat переносил только temperature / max_tokens / num_ctx —
// top_k, repeat_penalty, seed, min_p, typical_p, tfs_z, mirostat*,
// repeat_last_n, frequency_penalty, presence_penalty, stop и keep_alive
// молча терялись (аудит 2026-09-20, находка 1.2).
//
// Порядок разрешения конфликтов:
//  1. Значения из Options.* (Ollama-канонический вложенный блок) — база.
//  2. Top-level поля запроса (temperature/max_tokens/num_ctx) переопределяют
//     Options.*, если заданы явно — так же, как normalizeGenerateRequest
//     делает для /api/generate (`req.X == nil && Options.X != 0`).
//
// Prompt передаётся уже собранным вызывающим (buildChatPrompt).
// System/Raw намеренно НЕ прокидываются: /api/chat собирает system из
// messages[] и не должен повторно префиксовать его к готовому промпту.
func writeChatStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, thinkOverride *bool) {
	// R83 (2026-09-26): ВСЕ записи в ответ — через safeStreamWriter.
	//
	// Почему это критично именно здесь. В этой функции пишут ДВЕ горутины: колбэк
	// токенов (ниже) и heartbeat раз в 15 с («Round 6 Fix 5»). Раньше обе писали
	// в один http.ResponseWriter через fmt.Fprintf БЕЗ синхронизации, а
	// ResponseWriter не потокобезопасен: на длинной генерации (32 мин, 130+
	// heartbeat'ов) записи могли перемешаться внутри одного NDJSON-фрейма →
	// клиент получал невалидный JSON и падал с «Invalid control character».
	// safeStreamWriter держит мьютекс, а заодно:
	//   - проверяет ctx.Done() перед каждой записью;
	//   - логирует первый обрыв клиента (markBroken → Warnw + /debug/last-stream);
	//   - отдаёт IsBroken(), по которому колбэк прекращает генерацию
	//     (без этого cppworker молотил ещё секунды после ухода клиента — ровно то,
	//     что видно в живом логе: обрыв в 15:53:57, HTTP-лог в 15:54:05).
	sw := newSafeStreamWriter(w, r, "writeChatStreamResponse", modelName)
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sw.SetHeader("Content-Type", "application/x-ndjson")
	sw.SetHeader("Cache-Control", "no-cache")
	sw.SetHeader("Connection", "keep-alive")
	// Round 32 (2026-08-09): emit immediate "prefill" heartbeat перед inference.
	// C-bridge prefill занимает 5-30s на reasoning моделях. Без heartbeat клиент
	// не видит никаких данных в течение всего prefill ("зависший спиннер").
	// С heartbeat клиент получает валидный NDJSON {"done":false} event мгновенно —
	// connection "alive" с точки зрения EventSource, и пользователь видит
	// что запрос "пошёл". Heartbeat совместим с OpenWebUI (игнорирует строки
	// без "message" поля) и aiohttp/Cline (NDJSON парсер tolerates done:false).
	prefillStartTime := time.Now()
	// R334 (2026-10-30, поймано живой матрицей API): prefill-heartbeat обязан
	// быть кадром КОНТРАКТА Ollama, а не {"done":false}.
	//
	// Round 32 писал минимальный кадр без model/created_at/message, чтобы
	// OpenWebUI не принял "message":{"role":"assistant"} за начало нового
	// сообщения. Но кадр без message — не объект контракта: строгий разборщик
	// видит нарушение формы (матрица это и показала), а клиент не может связать
	// кадр с моделью. Компромисс, который устраивает оба случая: кадр контракта,
	// но с ПУСТЫМ content и без role (role клиенту для heartbeat не нужен, а
	// именно он сбивал OpenWebUI).
	prefillHB := map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"message":    map[string]interface{}{"content": ""},
		"done":       false,
	}
	prefillHBJSON, _ := json.Marshal(prefillHB)
	sw.Writef("%s\n", prefillHBJSON)
	sw.Flush()
	// R66b (2026-09-22): время ПЕРВОГО сгенерированного токена. Раньше
	// prompt_eval_duration считался как time.Since(prefillStartTime) в самом
	// конце генерации, то есть равнялся всей длительности запроса: условие
	// ttft <= total давало prompt_eval_duration == total_duration, а
	// eval_duration == 0. Клиенты (OpenWebUI) считают tokens/sec из
	// eval_duration и показывали 0/N/A. Теперь TTFT пишется из callback'а в
	// момент первого непустого дельта-токена.
	var firstTokenAt int64 // UnixNano, atomic
	ctx := r.Context()
	// R63 (2026-09-15): per-infer AbortWatcher создаётся внутри GenerateStream
	// (inference.go:521), там где доступен abortFlag от C-bridge. Удаляем
	// старый per-model watcher (R60.58) — был race condition для
	// параллельных infers одной модели.
	var outputBuf strings.Builder
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// 2026-06-25: запись snapshot после streaming (через именованный результат).
	var streamErr error
	defer func() {
		// Round 5 Fix 3: пробрасываем r для детекции client_disconnect.
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, false, streamErr, r)
	}()

	// 2026-07-01: reasoning-??????.
	//
	// R108 (2026-10-13): разбор идёт ВСЕГДА и только по тегам формата. Раньше
	// здесь стоял гейт IsReasoningModel(modelName) (whitelist префиксов), из-за
	// чего модель вне списка (например Qwen3.8-27B) отдавала ответ целиком в
	// content: клиент (Cline) видел служебные маркеры размышлений как обычный
	// текст. Имя модели/флаг влияют только на промпт, не на разбор — см.
	// «Политика отдачи reasoning» в reasoning_content.go.
	rcParser := NewReasoningStreamState()
	// R216: если шаблон открыл блок размышлений в ПРОМПТЕ, вывод начинается внутри него.
	rcParser.SetImpliedOpen(PromptOpensReasoning(prompt))
	// R470 НЕ применяется на этом пути (исправление R478, 2026-11-09).
	//
	// ЖИВОЙ ДЕФЕКТ (OpenWebUI): «модель генерирует токены, ответ не доходит».
	// Гейт R470 (`rcSendReasoning && reasoningEcho`) стоял в этой ОБЩЕЙ функции,
	// хотя задумывался для tools-пути. Последствие для reasoning-моделей
	// (Qwen3, gemma-4): парсер стартует ВНУТРИ размышлений
	// (`SetImpliedOpen(PromptOpensReasoning(prompt))` выше), и весь вывод до
	// закрывающего маркера — то есть обычно весь ответ — выбрасывался как
	// размышления. Клиент получал пустые кадры и keepalive, а в конце — пояснение
	// «бюджет исчерпан» вместо текста. На tools-пути
	// (writeChatStreamResponseWithTools) гейт остаётся: там клиент — агент,
	// который складывает thinking в текст ответа, и это отдельная проблема (R470).
	rcSendReasoning := ReasoningWantedByClient(thinkOverride)
	// emittedContentRawLen — сколько байт content (ДО замены `\\n`) уже отдано
	// дельтами. Нужен в think=false-ветке ниже, чтобы дослать остаток без
	// R91-фолбэка «незакрытый reasoning → content».
	emittedContentRawLen := 0
	// contentSeen — ровно тот ВИДИМЫЙ текст, который ушёл клиенту (после
	// разделения reasoning/content и замены `\\n`). Из него собирается финальный
	// done-чанк: в нём не должно быть размышлений (R108).
	var contentSeen strings.Builder

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			// R483: клиент отменил запрос — это событие хода (в кольцо сбоев).
			recordWorkerEventR483(r, r.URL.Path, "client_cancelled", "request context canceled by client")
			return false
		default:
		}
		// R83: если writer сломан (клиент отвалился, ctx.Done или write error) —
		// прекращаем генерацию немедленно. Без этого cppworker продолжал молотить
		// токены после ухода клиента: в живом логе обрыв 15:53:57, а HTTP-лог с
		// status=200 и duration=32m40s появился только в 15:54:05.
		if sw.IsBroken() {
			// R483: обрыв записи клиенту — событие хода, а не только строка в логе.
			recordWorkerEventR483(r, r.URL.Path, "client_gone", "stream write failed or client disconnected")
			return false
		}
		// R83: чистим управляющие символы — клиентские JSON-парсеры (артефакты,
		// tool-call аргументы) на них падают с «Invalid control character».
		// \n, \r, \t сохраняются: их несут markdown и код.
		token = sanitizeStreamText(token)
		outputBuf.WriteString(token)
		// R66b: TTFT — время первого непустого токена (см. firstTokenAt выше).
		if token != "" {
			atomic.CompareAndSwapInt64(&firstTokenAt, 0, time.Now().UnixNano())
		}

		// 2026-07-01: ????? reasoning-??????? ????? ????? ?? (reasoning, content) ?
		// эмитим НЕЗАВИСИМЫЕ NDJSON-чанки с message.reasoning / message.content.
		//
		// R108 (2026-10-13): безусловно (разбор — по тегам, не по имени модели).
		reasoningDelta, contentDelta := rcParser.Feed(token)
		// Счётчик ведём по СЫРЫМ дельтам: Finalize/повторный разбор работает по
		// сырому накопленному тексту, а замена `\\n` ниже меняет длину.
		emittedContentRawLen += len(contentDelta)
		// Round 32 #18 (2026-08-11): workaround для gemma-4 Q4_K_M detokenizer.
		// C-bridge detokenize'ит токены line break (`\n`) как literal `\\n`
		// (2 chars: backslash + n) вместо LF (1 char). Без этой замены
		// content приходит в JSON output как escaped `\\\\n` (4 source chars),
		// что после parsing даёт literal backslash + n (НЕ real newline).
		// В UI это видно как "весь markdown идёт сплошной линией".
		// Post-process: заменяем literal `\\n` на real `\n` ДО json.Marshal.
		// Безопасно — real newlines остаются нетронутыми (только 2-char
		// backslash+n конвертируются).
		reasoningDelta = strings.ReplaceAll(reasoningDelta, `\\n`, "\n")
		contentDelta = strings.ReplaceAll(contentDelta, `\\n`, "\n")
		// Правило 3 политики: think=false → размышления не отдаются вовсе.
		if !rcSendReasoning {
			reasoningDelta = ""
		}
		// R114b (2026-10-13): та же чистка, что в OpenAI-пути — одиночный
		// `</think>` (prefill-канал шаблона) не должен уезжать в message.content.
		// Здесь чистка тем более обязательна: из contentSeen собирается финальный
		// done-чанк, и без неё дельты расходились бы с ним, а балансер вырезал бы
		// расхождение как «дубль».
		if contentDelta != "" {
			contentDelta = stripReasoningTagsInStreamDelta(contentDelta)
		}
		if reasoningDelta != "" {
			rcChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]interface{}{
					"role": "assistant",
					// R108: `thinking` — каноническое имя по контракту §4.1;
					// `reasoning` оставляем как legacy-алиас.
					"thinking":  reasoningDelta,
					"reasoning": reasoningDelta,
				},
				"done": false,
			}
			rcJSON, _ := json.Marshal(rcChunk)
			sw.Writef("%s\n", rcJSON)
			sw.Flush()
		}
		if contentDelta != "" {
			contentSeen.WriteString(contentDelta)
			ccChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]string{
					"role":    "assistant",
					"content": contentDelta,
				},
				"done": false,
			}
			ccJSON, _ := json.Marshal(ccChunk)
			sw.Writef("%s\n", ccJSON)
			sw.Flush()
		}
		return true
	}

	// /api/chat endpoint: tools НЕ поддерживаются (Ollama-чат), reload разрешён при n_ctx overflow.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Round 31 #6 (2026-08-09): если streamErr — это ErrAborted (отменено через
		// bridge.RequestAbort), выдаём done_reason="cancelled" + cancelled: true
		// вместо "error" чтобы клиент мог отличить cancel от failure. DECISION Q3.
		if errors.Is(streamErr, bridge.ErrAborted) {
			cancelledChunk := map[string]interface{}{
				"model":       modelName,
				"created_at":  createdAt,
				"message":     map[string]string{"role": "assistant", "content": ""},
				"done":        true,
				"done_reason": "cancelled",
				"cancelled":   true,
			}
			cancelledJSON, _ := json.Marshal(cancelledChunk)
			sw.Writef("%s\n", cancelledJSON)
			sw.Flush()
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 в NDJSON-чанке).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"model":           modelName,
				"created_at":      createdAt,
				"message":         map[string]string{"role": "assistant", "content": ""},
				"done":            true,
				"done_reason":     "error",
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
				"http_status":     http.StatusRequestEntityTooLarge,
			}
			errJSON, _ := json.Marshal(errChunk)
			w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
			w.Header().Set("X-HTTP-Status", "413")
			sw.Writef("%s\n", errJSON)
			sw.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message":    map[string]string{"role": "assistant", "content": ""},
			"done":       true,
			"error":      streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("%s\n", errJSON)
		sw.Flush()
		return
	}

	fin := &chatStreamFinish{
		sw:                   sw,
		r:                    r,
		ctx:                  ctx,
		modelName:            modelName,
		prompt:               prompt,
		params:               params,
		thinkOverride:        thinkOverride,
		start:                start,
		prefillStartTime:     prefillStartTime,
		createdAt:            createdAt,
		outputBuf:            &outputBuf,
		firstTokenAt:         &firstTokenAt,
		rcParser:             rcParser,
		rcSendReasoning:      rcSendReasoning,
		emittedContentRawLen: &emittedContentRawLen,
		contentSeen:          &contentSeen,
		streamErr:            &streamErr,
		callback:             callback,
	}
	finishChatStream(fin)
}

// writeChatStreamResponseWithTools — streaming-ответ с поддержкой tool calling.
//
// В отличие от writeChatStreamResponse, эта функция:
//  1. Стримит токены как обычно через generateStreamWithRamFallback (real-time feedback).
//  2. Параллельно накапливает полный output в буфер.
//  3. После завершения генерации проверяет tool_calls через parseToolCallsFromOutput.
//  4. Если есть tool_calls — очищает content и эмитит финальный чанк с done:true,
//     done_reason:"tool_calls" ? message.tool_calls.
//  5. Если нет — эмитит обычный done:true чанк.
//
// Это решает баг, при котором OpenWebUI (stream=true + tools) получал non-streaming
// JSON-ответ и не мог корректно его обработать (видел "источник использован" без ответа).
// jsonMode (R66b, 2026-09-22): клиент запросил format:"json". В этом режиме
// функция вызывается даже БЕЗ tools — потому что снять markdown-обёртку
// ```json ... ``` можно только имея весь ответ целиком (буферизация). Финал:
// один content-чанк (без обёртки) + done-чанк.
// thinkOverride (R108, 2026-10-13): явный think клиента (nil = не задавал).
// Разбор ответа безусловный и тего-зависимый, поэтому при think=false
// размышления не отдаются вовсе (см. «Политика отдачи reasoning» в
// reasoning_content.go).
// startsWithReasoningMarker — R106c (2026-10-12): начинается ли вывод с
// reasoning-блока. Теги распознаёт общий SplitReasoningContent (поддерживает
// <think>, <thinking>, <reasoning>, <analysis>, <|channel>thought — gemma-4),
// включая НЕзакрытый блок: для потоковой отдачи этого достаточно, чтобы решить
// «дельты клиенту не отдаём, это размышления, а не ответ».
func startsWithReasoningMarker(s string) bool {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" {
		return false
	}
	if _, _, has := SplitReasoningContent(trimmed); has {
		return true
	}
	return false
}

// toolsReasoningUnclosedR320 — R320 (2026-10-30): закончился ли вывод ВНУТРИ
// незакрытого блока размышлений, НЕ дав видимого ответа.
//
// R320.1 (2026-10-30, найдено аудитом маршрутов): первый вариант правила
// проверял только «в промпте был открыт канал размышлений, а закрывающего тега в
// выводе нет» — и объявлял `length` ЛЮБОМУ нормальному ответу, если шаблон
// открывает канал в промпте (gemma-4). Клиент получал ложное «ответ усечён» на
// каждом ходе: отсюда жалоба «Cline не ловит COMPLETE».
//
// Ключевое условие, которого не хватало: размышления «съели ход» только тогда,
// когда ВИДИМОГО ответа не появилось вовсе. Если ответ есть, блок размышлений
// фактически закрылся (маркер мог не совпасть с тем, что ждёт парсер), и причина
// остановки — обычная.
func toolsReasoningUnclosedR320(fullOutput, visibleOutput, prompt string) bool {
	if strings.TrimSpace(fullOutput) == "" {
		return false
	}
	// Появился видимый ответ — ход не «ушёл в размышления».
	if strings.TrimSpace(visibleOutput) != "" {
		return false
	}
	if hasOpenThink(fullOutput) {
		return true
	}
	return PromptOpensReasoning(prompt) && !hasReasoningCloseTag(fullOutput)
}

// toolsStopReasonR320 — R320: ЧЕСТНАЯ причина остановки на tools-пути.
//
// ЖИВОЙ ДЕФЕКТ (Cline через балансер, 2026-10-30). На tools-пути
// (writeChatStreamResponseWithTools) `done_reason` выставлялся ЖЁСТКО в "stop" —
// и в ветке tool_calls, и в ветке обычного текста. Пока ответ доходил до конца,
// это было незаметно; но когда модель упиралась в бюджет вывода ВНУТРИ
// размышлений, клиент получал done:true + done_reason:"stop" при пустом content.
// По контракту Ollama «stop» означает «модель закончила», а не «обрезано»:
// Cline закрывал ход как полноценный ответ, и всё, что модель собиралась сказать,
// терялось. Именно это оператор описал как «завершение прямо посреди
// рассуждений» и «следующий запрос считается ответом».
//
// Контракт: Ollama для усечения по бюджету отдаёт done_reason:"length"; для
// вызова инструмента — "tool_calls". Оба значения клиенты понимают, и оба
// отличимы от «модель закончила».
func toolsStopReasonR320(fullOutput, visibleOutput, prompt string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	if toolsReasoningUnclosedR320(fullOutput, visibleOutput, prompt) {
		return "length"
	}
	return "stop"
}

// R113 (2026-10-13): tools — определения инструментов из ЗАПРОСА. Нужны, чтобы
// привести arguments вызова к объекту по их схеме (см. tool_args_schema.go):
// без этого клиент получает arguments-массив/голую строку, не может выполнить
// вызов и считает задачу завершённой после первой итерации.
func finishChatStream(in *chatStreamFinish) {
	// r — локальная ссылка на запрос: сторож load_duration_r493 ищет точную
	// строку кадра с loadDurationNsR493(r.Context()), поэтому имя сохранено.
	r := in.r
	fullOutput := in.outputBuf.String()
	duration := time.Since(in.start)

	// R83/v59: пустой вывод модели — один повторный проход (см.
	// empty_output_retry_budget.go). Здесь ничего не потеряно: при raw_len=0 дельт
	// клиенту не отправлялось.
	// R83/v62 (2026-10-02): автоповтор ТОЛЬКО если клиент ещё ждёт.
	// Если контекст запроса уже отменён (клиент ушёл — по своему таймауту, кнопке
	// Stop или обрыву), повторный проход бессмысленен: он мгновенно упадёт на
	// взведённом abort-флаге и в логе появится пугающее «повторная генерация не
	// удалась: BRIDGE_ERR_ABORTED» (живой лог A10, запрос 5 минут в prefill).
	// В этом случае честнее сразу закрыть стрим/отдать ошибку с причиной.
	//
	// R110 (2026-10-13): повторяем не только при raw_len=0, но и когда модель
	// выдала ОДНИ служебные токены шаблона (`<end_of_turn>` и подобные): после
	// чистки видимого текста нет, а клиенту при этом ничего не отправлено.
	// Живой случай: gemma-4 через Cline в ~1 прогоне из 3 отвечала одним
	// `<end_of_turn>`, и клиент получал «model produced an empty response».
	for attempt := 1; attempt <= maxEmptyOutputRetries; attempt++ {
		if !shouldRetryEmptyStreamingOutput(fullOutput, in.contentSeen.Len(), in.ctx) {
			break
		}
		logger.Get().Warnw(i18n.T("cppworker.handlers_chat.empty_output_retry"),
			"model", in.modelName, "attempt", attempt, "raw_len", len(fullOutput),
			"raw_head", truncateForLog(fullOutput, 80),
			"prompt_tokens", countTokensSafe(in.modelName, in.prompt))
		in.outputBuf.Reset()
		in.contentSeen.Reset()
		(*in.emittedContentRawLen) = 0
		atomic.StoreInt64(in.firstTokenAt, 0)
		if retryErr := generateStreamWithRamFallback(in.ctx, in.modelName, in.prompt, in.params, in.callback, false); retryErr != nil {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.empty_output_retry_failed"),
				"model", in.modelName, "attempt", attempt, "error", retryErr)
			break
		}
		fullOutput = in.outputBuf.String()
		duration = time.Since(in.start)
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.empty_output_retry_done"),
			"model", in.modelName, "attempt", attempt, "raw_len", len(fullOutput),
			"visible_len", in.contentSeen.Len())
	}

	// R83/v67 (2026-10-02): финальный сброс reasoning-парсера — выпускаем
	// удержанный хвост (возможный неполный маркер), иначе последние символы
	// ответа потеряются (см. ReasoningStreamState.Finalize).
	//
	// R108 (2026-10-13): при think=false разбираем накопленный текст заново, но
	// БЕЗ R91-фолбэка «незакрытый reasoning → content» — иначе текст размышлений
	// всё равно уехал бы клиенту как ответ (в non-stream /api/chat в этом случае
	// content пуст: см. handleChat).
	var rTail, cTail string
	if in.rcSendReasoning {
		rTail, cTail = in.rcParser.FinalizeForStream()
	} else {
		_, contentOnly, _ := SplitReasoningContent(in.outputBuf.String())
		if len(contentOnly) > (*in.emittedContentRawLen) {
			cTail = contentOnly[(*in.emittedContentRawLen):]
		}
	}
	rTail = strings.ReplaceAll(rTail, `\\n`, "\n")
	cTail = strings.ReplaceAll(cTail, `\\n`, "\n")
	// R114b: хвост — тот же видимый канал, что и дельты (см. чистку выше).
	cTail = stripReasoningTagsInStreamDelta(cTail)
	if rTail != "" {
		rcChunk := map[string]interface{}{
			"model":      in.modelName,
			"created_at": in.createdAt,
			"message": map[string]interface{}{
				"role":      "assistant",
				"thinking":  sanitizeStreamText(rTail),
				"reasoning": sanitizeStreamText(rTail),
			},
			"done": false,
		}
		rcJSON, _ := json.Marshal(rcChunk)
		in.sw.Writef("%s\n", rcJSON)
		in.sw.Flush()
	}
	if cTail != "" {
		in.contentSeen.WriteString(cTail)
		ccChunk := map[string]interface{}{
			"model":      in.modelName,
			"created_at": in.createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": sanitizeStreamText(cTail),
			},
			"done": false,
		}
		ccJSON, _ := json.Marshal(ccChunk)
		in.sw.Writef("%s\n", ccJSON)
		in.sw.Flush()
	}

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       in.modelName,
			"created_at":  in.createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, in.prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		in.sw.Writef("%s\n", errJSON)
		in.sw.Flush()
		return
	}

	// R402 (2026-11-05): eval_count — ВСЕ сгенерированные токены, включая
	// размышления.
	//
	// ЖИВОЙ ДЕФЕКТ. Здесь считался countTokensSafe(cleanedOutput), то есть
	// только видимый текст без think-блока. Клиенты (Cline/OpenWebUI) считают
	// заполненность контекста как prompt_eval_count + eval_count: занижение на
	// объём размышлений убеждало клиента, что место ещё есть, — он набивал
	// историю до мнимого предела, получал реальное переполнение окна и показывал
	// «увеличьте контекст вручную». Считаем по СЫРОМУ выводу, как eval_count в
	// Ollama (туда входят все сгенерированные токены).
	generatedTokens := countTokensSafe(in.modelName, fullOutput)
	promptTokens := countTokensSafe(in.modelName, in.prompt)
	evalNs := duration.Nanoseconds()
	promptEvalNs := int64(0)
	// R66b (2026-09-22): TTFT = время от начала запроса до ПЕРВОГО токена.
	// Раньше здесь было time.Since(in.prefillStartTime) в конце генерации, из-за
	// чего prompt_eval_duration совпадал с total_duration, а eval_duration
	// получался 0 → OpenWebUI показывал tokens/sec = 0/N/A.
	if deadline := atomic.LoadInt64(in.firstTokenAt); deadline > 0 {
		ttft := deadline - in.prefillStartTime.UnixNano()
		if ttft >= 0 && ttft <= evalNs {
			promptEvalNs = ttft
		}
	}
	// R108 (2026-10-13): в финальный чанк кладём ТОЛЬКО видимый ответ, собранный
	// из отданных дельт (in.contentSeen), а не сырой fullOutput.
	//
	// Раньше здесь был cleanFinalContent(fullOutput) — весь текст модели вместе с
	// think-блоком. Настоящая Ollama в done-чанке отдаёт сам ответ без
	// размышлений; из-за расхождения балансер вынужден был вырезать этот текст
	// как «дубль» и писать WARN о несовпадении с дельтами (см.
	// internal/balancer/llamacpp_transport.go: stripNativeDoneContent).
	doneContent := cleanFinalContent(in.contentSeen.String())
	// R231: незакрытый блок размышлений = ответ не дописан (см.
	// FinalizeForStream). Честная причина остановки — length.
	doneReason := "stop"
	snapR320 := in.rcParser.Snapshot()
	if snapR320.Unclosed {
		doneReason = "length"
	}
	// R394 (2026-11-04): исчерпанный клиентский лимит тоже даёт length.
	//
	// ЖИВОЙ ПРОГОН на стенде (этот самый путь, /api/chat без tools):
	// options.num_predict=16, eval_count=17, видимый ответ обрезан на середине
	// фразы — а причина была "stop". Cline по "stop" закрывает ход, поэтому
	// продолжения не будет. R320 ставил length только при НЕзакрытом блоке
	// размышлений; закрытый блок + обрыв по бюджету давал ложное «модель
	// закончила».
	//
	// Считаем по СЫРОМУ выводу: бюджет тратится и на размышления, а
	// cleanFinalContent их вырезает — по видимому тексту лимит выглядел бы
	// неизрасходованным. generatedTokens посчитан выше (R402) и уходит в
	// eval_count, чтобы usage и решение о причине завершения были об одном числе.
	if doneReason == "stop" && toolsBudgetExhaustedR394(in.params.NPredict, generatedTokens) {
		doneReason = "length"
	}
	// R320 (2026-10-30): пустая «видимая» часть при непустых размышлениях НЕ
	// должна уезжать клиенту молча.
	//
	// ЖИВАЯ ЖАЛОБА (Cline через балансер): «приходит завершение прямо посреди
	// рассуждений, обрубая всё, что приходит после». Механика: модель ушла в
	// размышления и уперлась в бюджет вывода — дельты thinking отданы, content
	// пуст, и клиент получает done:true. У Cline это выглядит как ЗАКОНЧЕННЫЙ
	// ответ без текста, поэтому он закрывает ход, а следующий запрос «продолжи»
	// модель воспринимает как новый ход.
	//
	// R314 ввёл пояснение для НЕпотокового пути (handleChat); на потоковом его не
	// было вовсе — а Cline ходит именно потоком. Теперь правило одно на обе
	// поверхности: emptyAnswerNoticeForNativeR314.
	//
	// ВАЖНО: текст кладём в done-чанк только когда content-дельт НЕ было, поэтому
	// клиент, складывающий чанки, не получит дубль (у балансера тот же инвариант —
	// stripNativeDoneContent чистит done только при уже отданных дельтах).
	//
	// R461 (2026-11-06): здесь числа ИЗВЕСТНЫ (бюджет num_predict и фактически
	// сгенерированные токены), поэтому пояснение называет проверенный факт
	// «бюджет исчерпан / не исчерпан», а не утверждает исчерпание всегда.
	if notice := emptyAnswerNoticeWithBudgetR461(doneContent, snapR320.ReasoningChars,
		in.params.NPredict, generatedTokens); notice != "" {
		doneContent = notice
	}
	// R477 (2026-11-09): сводка хода на НЕ-tools пути.
	//
	// Раньше строка "/api/chat stream response summary" была только на
	// tools-пути, поэтому по обычному /api/chat (а это и есть OpenWebUI) нельзя
	// было понять, сколько реально отдано клиенту: «токены генерировались, ответ
	// не дошёл» разбиралось по догадке. Теперь сводка есть на обоих путях и
	// несёт request_id, длину контента и размышлений, причину завершения и
	// побайтовый учёт стрима (при CPPWORKER_STREAM_TRACE=1).
	snapStreamR477 := in.sw.Snapshot()
	chunksR477, traceBytesR477, traceSHAR477 := in.sw.StreamTrace()
	logger.Get().Infow("/api/chat stream response summary (no tools)",
		"request_id", getRequestID(r),
		"model", in.modelName,
		"path", "/api/chat",
		"content_len", len(doneContent),
		"thinking_len", snapR320.ReasoningChars,
		"done_reason", doneReason,
		"generated_tokens", generatedTokens,
		"n_predict", in.params.NPredict,
		"streamed_bytes", snapStreamR477.BytesWritten,
		"stream_chunks", chunksR477,
		"stream_bytes", traceBytesR477,
		"stream_sha256", traceSHAR477,
	)
	doneChunk := map[string]interface{}{
		"model":                in.modelName,
		"created_at":           in.createdAt,
		"message":              map[string]string{"role": "assistant", "content": doneContent},
		"done":                 true,
		"done_reason":          doneReason,
		"total_duration":       evalNs,
		"load_duration":        loadDurationNsR493(r.Context()),
		"prompt_eval_count":    promptTokens,
		"prompt_eval_duration": promptEvalNs,
		"eval_count":           generatedTokens,
		"eval_duration":        evalNs - promptEvalNs,
	}
	doneJSON, _ := json.Marshal(doneChunk)
	in.sw.Writef("%s\n", doneJSON)
	in.sw.Flush()
}

// toolsStreamRun — состояние tools-стрима, которое живёт в вызывающей функции (R527).
//
// ЗАЧЕМ. writeChatStreamResponseWithTools была 1426 строк — вдвое больше порога
// читаемости (docs/plans/2026-11-19-readability-refactor.md, R2). Начало стрима
// вынесено в startToolsStreamGeneration, но финализация осталась в исходной
// функции И ДОЛЖНА читать те же переменные: callback меняет их во время
// генерации, финализация меняет их после (хвост ответа, чистый парсер на повторе
// R406). Поэтому состояние не копируется, а передаётся указателями, и
// вызывающая функция продолжает работать со своими локальными именами — по ним
// же читают исходник сторожа R394/R404.
//
// Порядок полей — под fieldalignment: значение, интерфейсы и строки, затем
// указатели и функции, затем флаги.
type toolsStreamRun struct {
	// Порядок полей — под fieldalignment (R527: «432 pointer bytes could be 384»):
	// сначала указатели/интерфейсы/функции, затем строки, значение params и флаги.
	ctx                context.Context
	w                  http.ResponseWriter
	firstTokenAt       *int64
	holdReleasedAt     *time.Time
	holdReleaseReason  *string
	firstContentAt     *time.Time
	streamedBytes      *int
	emitThinkingDelta  func(string)
	callback           func(string) bool
	outputBuf          *strings.Builder
	sw                 *safeStreamWriter
	r                  *http.Request
	thinkingParserCell **ReasoningStreamState
	streamErr          *error
	emittedLen         *int
	holdSince          *time.Time
	emitContentDelta   func(string)
	holdReleasedBytes  *int
	anyStreamed        *bool
	contentStreamed    *bool
	thinkingStreamed   *bool
	reasoningHold      *bool
	toolsHold          *bool
	holdKeptReasoning  *bool
	modelName          string
	prompt             string
	createdAt          string
	params             bridge.GenerationParams
	streamContent      bool
	reasoningEcho      bool
}

// startToolsStreamGeneration — начало tools-стрима (R527): дельты content и
// thinking, сторожа удержания вывода (R482/R485), callback генерации и сама
// генерация с обработкой ошибок.
//
// Тело перенесено дословно (R527), изменён только способ доступа к изменяемому
// состоянию: у вызывающей функции оно остаётся локальными переменными.
//
// Возвращает false, если ответ уже отправлен (ошибка генерации) — вызывающая
// функция обязана выйти.
func startToolsStreamGeneration(x *toolsStreamRun) bool {
	// Неизменяемые входы — локальными именами, чтобы перенесённое тело читалось
	// как прежде.
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	createdAt := x.createdAt
	params := x.params
	ctx := x.ctx
	w := x.w
	sw := x.sw
	// streamErr — результат генерации. Вызывающая функция читает его в своём
	// defer (recordLastPromptFromErrorWithContext), поэтому итог пишется в её
	// переменную при любом выходе.
	var streamErr error
	defer func() { *x.streamErr = streamErr }()

	emitContentDelta := func(delta string) {
		// R114b (2026-10-13): служебные токены шаблона — не текст ответа. Живой
		// случай: дельта «other coding task.\n<end_of_turn>» уехала клиенту
		// (маркер заканчивался в самом хвосте потока, а прежняя чистка
		// (cleanFinalContent) срабатывает только на финальном чанке).
		delta = stripServiceTokensKeepSpaces(delta)
		// R429 (2026-11-04): одиночные маркеры размышлений не отдаём и здесь.
		//
		// ЗАЧЕМ. Дельта на tools-пути приходит из ДВУХ мест: из нарезки буфера по
		// окну удержания (там R414 сам пропускает маркеры) и из предохранителей
		// 2/2b/2c — они отдают срез `buf[emittedLen:pos]`, где pos — начало
		// разметки вызова. `<channel|>` стоит РОВНО перед вызовом и попадал в
		// такой срез: живой прогон gemma-4 + Cline (R430) закончил потоковую
		// прозу на «…in the file `d:\cline-e2e-web\index.html`.<channel|>».
		// Чистка та же, что на не-tools путях (R114b): список маркеров один —
		// orphanReasoningTags.
		delta = stripReasoningTagsInStreamDelta(delta)
		if delta == "" {
			return
		}
		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": delta,
			},
			"done": false,
		}
		data, _ := json.Marshal(chunk)
		sw.Writef("%s\n", data)
		sw.Flush()
		*x.streamedBytes += len(delta)
		*x.anyStreamed = true
		*x.contentStreamed = true
		if (*x.firstContentAt).IsZero() {
			*x.firstContentAt = time.Now()
		}
	}

	// R404: thinking-дельта уходит сразу, не дожидаясь закрытия блока
	// размышлений (он может не закрыться: бюджет вывода кончается внутри).
	emitThinkingDelta := func(delta string) {
		if delta == "" {
			return
		}
		// R470 (2026-11-06): ЕДИНСТВЕННАЯ точка отправки thinking-дельт на
		// tools-пути. Если клиент не просил размышления (think не задан), не
		// отдаём их: Cline сохраняет их как текст нашего ответа, и модель на
		// следующем ходу видит свои размышления как свой прошлый ответ.
		if !reasoningEcho {
			return
		}
		chunk := toolsThinkingChunkR404(modelName, createdAt, delta)
		data, _ := json.Marshal(chunk)
		sw.Writef("%s\n", data)
		sw.Flush()
		*x.thinkingStreamed = true
		*x.anyStreamed = true
	}

	// R527: дельты и callback — методы на состоянии (funlen: у функции 100
	// вызовов, а не строк). Финализации нужны те же замыкания, поэтому
	// ссылки кладём в структуру сразу.
	x.emitContentDelta = emitContentDelta
	x.emitThinkingDelta = emitThinkingDelta
	callback := x.onStreamToken

	// /api/chat streaming with tools: full buffer, single final response.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)

	// Если во время streaming произошла ошибка — отдаём финальный chunk с error,
	// как в writeChatStreamResponse. Tool calls в этом случае не анализируем.
	if streamErr != nil {
		writeToolsStreamGenerationError(w, sw, modelName, createdAt, streamErr)
		return false
	}

	x.emitContentDelta = emitContentDelta
	x.emitThinkingDelta = emitThinkingDelta
	x.callback = callback
	return true
}

// logToolsStreamSummary — сводка tools-хода одной строкой и финальный кадр (R527).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это последние ~110 строк writeChatStreamResponseWithTools:
// сбор полей сводки, сторож «слабый ход агента» (R496) и запись done-кадра. Тело
// перенесено дословно; поля, которые продолжают жить в вызывающей функции,
// читаются через toolsStreamRun.
//
// ВАЖНО: причина завершения уже посчитана вызывающей функцией (R394) — здесь
// она только печатается. Переносить сюда расчёт причины нельзя: сторож
// TestR394_ToolsPathUsesBudgetAwareReason проверяет вызовы именно в
// writeChatStreamResponseWithTools.
func logToolsStreamSummary(x *toolsStreamRun, sw *safeStreamWriter, r *http.Request, start time.Time,
	duration time.Duration, fullOutput string, finalChunk map[string]interface{},
	toolCalls []openAIToolCall, tools []openAITool, budgetTokens, generatedTokens int,
	reasoningOut string, toolCallsFromReasoning bool,
) {
	modelName := x.modelName
	params := x.params

	// R113 (2026-10-13): ЯВНОЕ ОТСЛЕЖИВАНИЕ ОТВЕТА. Одна строка на запрос, по
	// которой видно, ЧТО именно уехало клиенту: сколько content отдано дельтами,
	// сколько ушло в финальном чанке, какие вызовы и какого типа аргументы,
	// с каким done_reason. Раньше вопрос «инструмент в теле ответа?» приходилось
	// выяснять по логам клиента.
	toolNames := make([]string, 0, len(toolCalls))
	argKinds := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		toolNames = append(toolNames, tc.Function.Name)
		argKinds = append(argKinds, toolArgShape(tc.Function.Arguments))
	}
	finalContent := ""
	if msg, ok := finalChunk["message"].(map[string]interface{}); ok {
		finalContent, _ = msg["content"].(string)
	}
	// R457 (2026-11-06): запрос был С инструментами, а ответ — телом файла
	// текстом и без вызова. Живой прогон: HTML-страница на 8587 символов ушла в
	// чат, файл не создан; в логах это выглядело как обычный done_reason=stop,
	// то есть причину «инструменты перестали работать» не было видно вообще.
	fileBodyInsteadOfCall := len(tools) > 0 && len(toolCalls) == 0 && looksLikeFileBodyR457(finalContent)
	if fileBodyInsteadOfCall {
		logger.Get().Warnw("R457: модель ответила телом файла вместо вызова инструмента",
			"model", modelName, "content_len", len(finalContent),
			"tools", len(tools), "done_reason", finalChunk["done_reason"])
	}
	// R474 (2026-11-07): аргументы вызова, выданные текстом. В сводке это
	// отдельное поле по той же причине, что и R457: снаружи такой ход выглядит
	// обычным done_reason=stop, и без поля причину «вызов не выполнен» в логах
	// не видно. Считаем по СЫРОМУ выводу: в finalContent уже лежит пояснение.
	_, _, dumpedArgsInsteadOfCall := looksLikeDumpedArgumentsR474(fullOutput)
	dumpedArgsInsteadOfCall = dumpedArgsInsteadOfCall && len(tools) > 0 && len(toolCalls) == 0
	// R476: снимок трассировки стрима (если включена CPPWORKER_STREAM_TRACE).
	streamTraceChunks, streamTraceBytes, streamTraceSHA := sw.StreamTrace()
	// R496 (2026-11-18): сторож «слабый ход агента» — та же проверка, что в
	// не-стриме, но здесь заголовки уже отправлены (поток идёт), поэтому диагноз
	// уходит в сводку и счётчик: строка с числами хода — единственное место, где
	// оператор сопоставит «пустой/обещающий ответ» с моделью и request_id.
	toolGuardVerdict := observeToolGuardR496(modelName, "/api/chat", ToolGuardInputR496{
		ToolsOffered:    len(tools) > 0,
		ToolCalls:       len(toolCalls),
		DoneReason:      doneReasonStringR496(finalChunk["done_reason"]),
		VisibleContent:  finalContent,
		BudgetExhausted: toolsBudgetExhaustedR394(params.NPredict, budgetTokens),
	})
	logArgs := []interface{}{
		"model", modelName,
		"path", "/api/chat",
		// R476 (2026-11-07): id, который выдал балансер — по нему ход находится
		// в логе балансера одной строкой `request trace`. Без него разбор
		// дефекта сводился к сопоставлению по времени.
		"request_id", getRequestID(r),
		"streamed_content_bytes", *x.streamedBytes,
		// R429: позиция разбора в буфере — отдельным полем. Раньше она печаталась
		// как streamed_content_bytes и расходилась с реально отданным текстом
		// (пропущенные маркеры размышлений, хвост из финального чанка).
		"emitted_len", *x.emittedLen,
		"final_content_len", len(finalContent),
		"thinking_len", len(reasoningOut),
		"tool_calls", len(toolCalls),
		"file_body_instead_of_call", fileBodyInsteadOfCall,
		"dumped_args_instead_of_call", dumpedArgsInsteadOfCall,
		"tool_names", strings.Join(toolNames, ","),
		"tool_arg_shapes", strings.Join(argKinds, ","),
		"done_reason", finalChunk["done_reason"],
		"raw_len", len(fullOutput),
		// R394: по этим полям следующий разбор «почему ход закрылся» решается
		// без догадок: видно клиентский лимит, сколько токенов реально
		// сгенерировано (упёрлись ли в лимит) и сколько символов размышлений
		// было в СЫРОМ выводе (в том числе скрытых через think=false).
		"n_predict", params.NPredict,
		"generated_tokens", generatedTokens,
		"budget_tokens", budgetTokens,
		"budget_exhausted", toolsBudgetExhaustedR394(params.NPredict, budgetTokens),
		"reasoning_chars_raw", reasoningCharsRawR394(fullOutput),
		"elapsed_ms", duration.Milliseconds(),
		// R476: побайтовое доказательство отдачи (CPPWORKER_STREAM_TRACE=1):
		// чанки, записанные байты и sha256 всего записанного. Пусто — трассировка
		// выключена (дефолт).
		"stream_chunks", streamTraceChunks,
		"stream_bytes", streamTraceBytes,
		"stream_sha256", streamTraceSHA,
		// R482: был ли вызов найден в reasoning и снималось ли удержание вывода.
		// Без этих полей живая жалоба «инструменты не применяются, ответ пришёл
		// одним куском» снова разбиралась бы по догадкам.
		"tool_calls_from_reasoning", toolCallsFromReasoning,
		"hold_released", *x.holdReleaseReason,
		"hold_released_ms", func() int64 {
			if (*x.holdReleasedAt).IsZero() || (*x.holdSince).IsZero() {
				return 0
			}
			return (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds()
		}(),
		"hold_released_bytes", *x.holdReleasedBytes,
		// R485: сторож сработал, но удержание оставлено, потому что оно вызвано
		// размышлениями. Без этого поля в архиве диагностики снова пришлось бы
		// выяснять по логам, почему ход «не потоковый».
		"hold_kept_reasoning", *x.holdKeptReasoning,
		"hold_max_sec", int(toolsHoldMaxHoldR482().Seconds()),
		"hold_max_bytes", toolsHoldMaxBytesR482(),
		"first_content_ms", func() int64 {
			if (*x.firstContentAt).IsZero() {
				return -1
			}
			return (*x.firstContentAt).Sub(start).Milliseconds()
		}(),
	}
	logArgs = append(logArgs, toolGuardFieldsForLogR496(toolGuardVerdict)...)
	logger.Get().Infow("/api/chat stream response summary", logArgs...)

	doneJSON, _ := json.Marshal(finalChunk)
	sw.Writef("%s\n", doneJSON)
	sw.Flush()
}

// startToolsStreamKeepalive — немедленный кадр «запрос принят» и heartbeat-горутина
// tools-стрима (R527).
//
// Тело перенесено дословно из writeChatStreamResponseWithTools, включая разбор
// причин (Round 6 Fix 5, Round 27, R189). Изменено одно: cleanup возвращается
// функцией, а вызывающая сторона ставит его через defer — иначе горутина
// пережила бы ответ и писала в закрытый writer.
func startToolsStreamKeepalive(sw *safeStreamWriter, ctx context.Context, modelName, createdAt string) func() {
	// Round 6 Fix 5: heartbeat goroutine prevents idle-timeout during
	// long generations.
	//
	// Round 27 (v0.5.14 follow-up): /api/chat (Ollama native) is NDJSON,
	// not SSE. SSE comment ": keepalive\n\n" was being parsed as JSON
	// by Cline and other strict NDJSON clients, causing "invalid json"
	// spam. Use a valid NDJSON object `{"keepalive":true}` instead —
	// NDJSON parsers parse it as a no-content chunk and continue.
	// Also bumped interval from 100ms to 15s default (same as OpenAI
	// SSE handler) — 100ms × N seconds of generation = thousands of
	// useless chunks. 15s × N still keeps TCP alive (typical proxy
	// idle-timeout is 30-60s).
	// R189 (2026-10-20): НЕМЕДЛЕННЫЙ кадр «запрос принят».
	//
	// ЖИВОЙ РАЗБОР. На tools-пути текст ответа уходит только по мере генерации
	// (а при размышлениях удерживается окном), поэтому клиент до первого токена
	// не получает НИЧЕГО. На 27B-модели с CPU-оффлоадом префилл + размышления
	// занимают десятки секунд-минуты, и Cline показывал «зависание», хотя
	// запрос обслуживался. Пустой NDJSON-кадр с done:false — валидный ответ
	// Ollama API: клиент видит, что стрим начался, и не рвёт соединение.
	if sw.Writef("{\"model\":%q,\"created_at\":%q,\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":false}\n", modelName, createdAt) {
		sw.Flush()
	}

	keepaliveInterval := getHeartbeatInterval(15 * time.Second)
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !sw.Writef("{\"keepalive\":true}\n") {
					// Клиент отвалился — writer помечен broken, выходим.
					return
				}
				sw.Flush()
			}
		}
	}()
	return func() {
		close(heartbeatStop)
		select {
		case <-heartbeatDone:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// releaseHoldIfOverdue — R482/R485: снять удержание вывода, если оно длится
// слишком долго или накопило слишком много (R527: вынесено методом из
// startToolsStreamGeneration; тело и комментарии перенесены дословно).
//
// releaseHoldIfOverdue — R482: снять удержание, если оно длится слишком
// долго или накопило слишком много. Накопленное НЕ отдаём (в нём могут быть
// размышления), но все следующие токены пойдут клиенту нормально.
//
// R485: если удержание вызвано РАЗМЫШЛЕНИЯМИ (открытый блок/канал открыт
// шаблоном), удержание НЕ снимаем — иначе размышления уходят в content,
// клиент сохраняет их как ответ ассистента, и на следующем ходу модель видит
// свои же размышления как прошлый ответ (живой архив: content_len=18720/65109
// у assistant-сообщений с tool_calls). Аварийный тумблер —
// CPPWORKER_TOOLS_HOLD_RELEASE_REASONING=1.
func (x *toolsStreamRun) releaseHoldIfOverdue() bool {
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	outputBuf := x.outputBuf
	r := x.r
	if !*x.reasoningHold && !*x.toolsHold {
		// Удержание закончилось само (R190 нашёл закрывающий маркер) —
		// следующий эпизод считается с нуля.
		*x.holdSince = time.Time{}
		return false
	}
	if (*x.holdSince).IsZero() {
		*x.holdSince = time.Now()
		return false
	}
	held := len(outputBuf.String()) - *x.emittedLen
	ok, why := toolsHoldShouldReleaseR482(*x.holdSince, held, time.Now())
	if !ok {
		return false
	}
	if toolsHoldReasoningR485(*x.reasoningHold, outputBuf.String(), prompt, modelName, reasoningEcho, *x.emittedLen) &&
		!toolsHoldReleaseReasoningR485() {
		if !*x.holdKeptReasoning {
			*x.holdKeptReasoning = true
			logger.Get().Warnw("R485: удержание reasoning не снимаем — иначе размышления уйдут в content",
				"model", modelName,
				"reason", why,
				"held_ms", time.Since(*x.holdSince).Milliseconds(),
				"held_bytes", held,
				"switch", "CPPWORKER_TOOLS_HOLD_RELEASE_REASONING=1 — снять принудительно (риск загрязнения истории)",
				"thinking_switch", "CPPWORKER_TOOLS_THINKING=on — отдавать размышления в поле thinking")
			recordWorkerEventR483(r, "/api/chat", "hold_kept_reasoning",
				fmt.Sprintf("%s held_ms=%d held_bytes=%d", why, time.Since(*x.holdSince).Milliseconds(), held))
		}
		return false
	}
	*x.reasoningHold = false
	*x.toolsHold = false
	*x.emittedLen = len(outputBuf.String())
	*x.holdReleaseReason = why
	*x.holdReleasedAt = time.Now()
	*x.holdReleasedBytes = held
	logger.Get().Warnw("R482: удержание вывода снято — ход снова потоковый",
		"model", modelName,
		"reason", why,
		"held_ms", (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds(),
		"held_bytes", held,
		"hold_max_sec", int(toolsHoldMaxHoldR482().Seconds()),
		"hold_max_bytes", toolsHoldMaxBytesR482(),
		"note", "накопленное не отдаётся (возможны размышления); последующие токены идут в content")
	// R483: событие в кольцо воркера — иначе «ответ одним куском» в архиве
	// диагностики не видно без чтения текста лога.
	recordWorkerEventR483(r, "/api/chat", "hold_released",
		fmt.Sprintf("%s held_ms=%d held_bytes=%d", why, (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds(), held))
	return true
}

// onStreamToken — callback генерации tools-пути (R527): чистит дельты, отдаёт
// thinking сразу, держит content окном до конца возможного маркера вызова.
// Тело перенесено дословно; thinkingParser читается из ячейки вызывающей
// функции на КАЖДОМ вызове — повтор R406 в финализации заменяет парсер.
func (x *toolsStreamRun) onStreamToken(token string) bool {
	modelName := x.modelName
	ctx := x.ctx
	r := x.r
	sw := x.sw
	outputBuf := x.outputBuf
	streamContent := x.streamContent
	// thinkingParser читаем у вызывающей функции на КАЖДОМ вызове: повтор
	// R406 в финализации заменяет парсер чистым, и callback обязан взять новый,
	// иначе дельты второй попытки смешались бы с состоянием первой.
	thinkingParser := *x.thinkingParserCell
	select {
	case <-ctx.Done():
		// R483: клиент отменил запрос — это событие хода (в кольцо сбоев).
		recordWorkerEventR483(r, r.URL.Path, "client_cancelled", "request context canceled by client")
		return false
	default:
	}
	// R83: writer broken (клиент ушёл) → прекращаем генерацию немедленно.
	if sw.IsBroken() {
		// R483: обрыв записи клиенту — событие хода, а не только строка в логе.
		recordWorkerEventR483(r, r.URL.Path, "client_gone", "stream write failed or client disconnected")
		return false
	}
	// R83: чистим управляющие символы (см. stream_sanitize.go).
	token = sanitizeStreamText(token)
	outputBuf.WriteString(token)
	// R482: если удержание вывода длится слишком долго (шаблон не закрывает
	// канал размышлений), снимаем его — иначе весь ответ уйдёт одним куском.
	if x.releaseHoldIfOverdue() {
		return true
	}
	// R404: thinking отдаём сразу — он не может быть вызовом инструмента,
	// поэтому не ждёт закрытия блока и не попадает под reasoningHold ниже.
	if td := toolsFeedThinkingR404(thinkingParser, token); td != "" {
		x.emitThinkingDelta(td)
	}
	// R66b: TTFT — время первого непустого токена.
	if token != "" {
		atomic.CompareAndSwapInt64(x.firstTokenAt, 0, time.Now().UnixNano())
	}
	// R190: пока открыт блок размышлений — держим; как только закрыт — отдаём дельты.
	if *x.reasoningHold {
		if end := reasoningBlockEnd(outputBuf.String()); end > 0 {
			*x.reasoningHold = false
			*x.toolsHold = false
			*x.emittedLen = end
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_reasoning_block_closed"),
				"model", modelName, "reasoning_end", end)
		} else {
			return true
		}
	}
	if !streamContent || *x.toolsHold || token == "" {
		return true
	}
	// R527: предохранители и нарезка окна — отдельным методом (funlen).
	x.emitContentWindow(token)
	return true
}

// emitContentWindow — предохранители и нарезка окна удержания tools-пути
// (R527: вынесено из onStreamToken; порядок проверок и комментарии прежние).
// Дельта уходит клиенту только после всех предохранителей: разметка вызова,
// JSON-тело, служебные маркеры и размышления в content не попадают.
func (x *toolsStreamRun) emitContentWindow(token string) {
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	outputBuf := x.outputBuf
	buf := outputBuf.String()
	// Предохранитель 1: ответ начинается с JSON — парсер может счесть его
	// tool_calls целиком (стратегия 4), поэтому дельтами не отдаём.
	//
	// R106d (2026-10-12): то же для markdown-фенса (```json) — модели кладут
	if *x.emittedLen == 0 && (toolsStreamStartsWithJSON(buf) || toolsStreamStartsWithFence(buf)) {
		*x.toolsHold = true
		return
	}
	// R106d (2026-10-12): то же для markdown-фенса (```json) — вызывать после JSON-проверки.
	// Раньше здесь стояла вторая ветка guard-а; после R190 она объединена выше.
	// Предохранитель 1b (R106c, 2026-10-12): ответ начинается с reasoning-блока
	// (<think>, <thinking>, <reasoning>, <analysis>, <|channel>thought).
	// Размышления — НЕ ответ, и клиенту в content их отдавать нельзя: Cline
	// показывал thinking как ответ («команда инструмента в квадратных
	// скобках»), а сам tool call терялся. Держим дельты и отдаём reasoning
	// отдельным полем в финальном чанке (см. reasoningOut ниже).
	if *x.emittedLen == 0 && startsWithReasoningMarker(buf) {
		*x.reasoningHold = true
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_reasoning"),
			"model", modelName)
		return
	}
	// R471 (2026-11-06): ОТКРЫВАЮЩЕГО маркера может НЕ быть в тексте (канал
	// открыт шаблоном), и тогда начало вывода уходило клиенту как ответ.
	// Живой прогон: streamed_content_bytes=5363 при content_len=3323 — 2040
	// символов английских размышлений уехали в content вместе с русским
	// ответом, хотя финальный разбор считал их reasoning. Держим начало до
	// закрывающего маркера (см. reasoning_start_hold.go).
	if *x.emittedLen == 0 && reasoningStartHoldR471(prompt, modelName, reasoningEcho) {
		*x.reasoningHold = true
		*x.toolsHold = true
		logger.Get().Infow("R471: начало вывода удерживается до закрывающего маркера размышлений",
			"model", modelName,
			"why", "клиент не просил размышления, но модель их умеет и в промпте они включены",
			"switch", "CPPWORKER_TOOLS_THINKING")
		return
	}
	// Предохранитель 1c (R110, 2026-10-13): ответ начинается со СЛУЖЕБНОГО
	// токена шаблона (`<end_of_turn>`, `<|end|>`, `<start_of_turn>model`).
	// Такие дельты тоже не отдаём: служебный токен — не текст ответа, а когда
	// буфер перерастал окно удержания, `cut` попадал уже ПОСЛЕ токена и клиент
	// получал его как обычный текст (живой случай: «ERROR: EMPTY
	// CONTENT<end_of_turn>» в записи сессии Cline).
	if *x.emittedLen == 0 && toolsStreamStartsWithServiceToken(buf) {
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_service_token"),
			"model", modelName)
		return
	}
	// Предохранитель 2: в хвосте появился маркер tool call. Prose ПЕРЕД
	// маркером обязан уйти клиенту (он уже сгенерирован и не является
	// частью вызова) — иначе окно удержания съедало бы до 32 байт ответа.
	if pos, ok := toolsStreamMarkerPos(buf, toolsStreamHoldback+len(token)); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_marker_found"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2b (R113, 2026-10-13): JSON-форма вызова ВНУТРИ прозы.
	// Прежние проверки держали только вывод, НАЧИНАЮЩИЙСЯ с JSON; живой прогон
	// Cline-запроса дал прозу с последующим JSON вызова — и он уехал клиенту
	// дельтами (в сессии это «инструмент в теле ответа»).
	if pos, ok := toolsStreamJSONCallPos(buf, toolsStreamHoldback+len(token)); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_call_in_prose"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2c (R114b, 2026-10-13): в НЕПЕРЕДАННОМ хвосте началась
	// JSON-структура. Дельты прекращаем на её первой скобке: иначе начало
	// вызова уходит клиенту обрывком («…task complete. [{"id":
	// "call_submit_and_exit","type":"function",…» — живая сессия Cline:
	// объект начинался раньше, чем в окне удержания появлялся ключ, на
	// который смотрел прежний предохранитель).
	if pos, ok := toolsStreamJSONStructPos(buf, len(buf)-*x.emittedLen); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_struct_in_tail"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2d (R114b): открывающий маркер размышлений в хвосте —
	// блок начался после отданной прозы, и его тело не является ответом.
	if reason, hold := toolsStreamHoldsTailStart(buf[*x.emittedLen:]); hold {
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_tail_held"),
			"model", modelName, "reason", reason,
			"emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	cut := len(buf) - toolsStreamHoldback
	if cut <= *x.emittedLen {
		return
	}
	cut = toolsStreamRuneSafeCut(buf, *x.emittedLen, cut)
	if cut <= *x.emittedLen {
		return
	}
	// R414 (2026-11-03): служебные маркеры размышлений (в частности
	// gemma-4 `<channel|>`) не должны уходить клиенту как текст ответа.
	// Живой случай: «…run the check command.<channel|>» в сессии Cline.
	// Сначала удерживаем хвост, который может быть НАЧАЛОМ маркера, затем
	// отдаём текст до первого ПОЛНОГО маркера и пропускаем сам маркер.
	delta := buf[*x.emittedLen:cut]
	if hb := toolsStreamMarkerPrefixHoldbackR414(delta); hb > 0 {
		cut -= hb
		if cut <= *x.emittedLen {
			return
		}
		cut = toolsStreamRuneSafeCut(buf, *x.emittedLen, cut)
		if cut <= *x.emittedLen {
			return
		}
		delta = buf[*x.emittedLen:cut]
	}
	if pos, markerLen, found := toolsStreamMarkerScanR414(delta); found {
		if pos > 0 {
			x.emitContentDelta(delta[:pos])
		}
		// Маркер не отдаём: сдвигаем позицию ЗА него, чтобы он не попал
		// в следующую дельту.
		*x.emittedLen += pos + markerLen
		return
	}
	x.emitContentDelta(delta)
	*x.emittedLen = cut
}

// writeToolsStreamGenerationError — кадры ошибки генерации tools-пути (R527):
// reload-loop-limit уходит с кодом и HTTP-статусом 413, остальное — общим кадром.
func writeToolsStreamGenerationError(w http.ResponseWriter, sw *safeStreamWriter, modelName, createdAt string, streamErr error) {
	maybeRestartOnMemorySlotError(streamErr, modelName)
	// Специальная обработка reload-loop-limit: HTTP-статус 413 в NDJSON-чанке
	// с error+code+model+attempts. OpenWebUI увидит error и завершит сессию
	// с понятным сообщением (вместо бесконечного цикла unload+reload).
	if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
		errChunk := map[string]interface{}{
			"model":           modelName,
			"created_at":      createdAt,
			"message":         map[string]string{"role": "assistant", "content": ""},
			"done":            true,
			"done_reason":     "error",
			"error":           rllErr.Error(),
			"code":            "reload_loop_limit",
			"attempts":        rllErr.Count,
			"elapsed_seconds": rllErr.Elapsed.Seconds(),
			"http_status":     http.StatusRequestEntityTooLarge,
		}
		errJSON, _ := json.Marshal(errChunk)
		// Также ставим HTTP-статус в response (best-effort: если streaming уже начат — http.status будет проигнорирован клиентом, но код в NDJSON всё равно будет передан).
		w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
		w.Header().Set("X-HTTP-Status", "413")
		sw.Writef("%s\n", errJSON)
		sw.Flush()
		return
	}
	errChunk := map[string]interface{}{
		"model":       modelName,
		"created_at":  createdAt,
		"message":     map[string]string{"role": "assistant", "content": ""},
		"done":        true,
		"done_reason": "error",
		"error":       streamErr.Error(),
	}
	errJSON, _ := json.Marshal(errChunk)
	sw.Writef("%s\n", errJSON)
	sw.Flush()
}