package main

import (
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
	logger.Get().Debugw("handleChat: cancel tracking enabled",
		"request_id", requestID, "model", req.Model, "user_id", userID)

	// Ленивая загрузка модели
	// R83-политика (2026-10-01): окно загрузки = num_ctx запроса (если клиент
	// его указал). Раньше lazy-load брал окно из env/профиля, и клиент получал
	// модель с чужим окном, после чего шли «перезагрузки под клиента».
	actualModel, err := ensureModelLoadedWithNCtx(r.Context(), req.Model, req.Options.NumCtx)
	if err != nil {
		// R119 (2026-10-15): 503 «грузится» / 404 «нет файла» / 500 «поломка» —
		// одна точка перевода, см. writeModelLoadError. До этого ЛЮБАЯ причина
		// уходила как 500 с пустым телом.
		writeModelLoadError(w, req.Model, err)
		return
	}

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
		writeChatNDJSONFromResult(w, req.Model, result)
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
func buildGenerateRequestFromChat(req chatRequest, prompt string) generateRequest {
	genReq := generateRequest{
		Model:   req.Model,
		Prompt:  prompt,
		Stream:  req.Stream,
		Options: req.Options,
	}

	// Шаг 1: разворачиваем Options.* в top-level поля generateRequest.
	normalizeGenerateRequest(&genReq)

	// Шаг 2: top-level поля /api/chat имеют приоритет над Options.*
	if req.Temperature != nil {
		v := *req.Temperature
		genReq.Temperature = &v
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}
	// R69/R83-v67 (2026-10-02): max_output_tokens (Cline) — ВЕРХНЯЯ ГРАНИЦА, а не
	// длина ответа. Раньше значение подставлялось в genReq.MaxTokens, то есть
	// «клиент примет до 64000» превращалось в «сгенерируй до 64000» (десятки
	// минут на слабой карте, обрыв по клиентскому таймауту). Теперь граница
	// только уменьшает финальный num_predict.
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		genReq._upperBoundNumPredict = *req.MaxOutputTokens
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.max_output_tokens_upper_bound"),
			"model", req.Model, "max_output_tokens", *req.MaxOutputTokens)
	}
	if req.NumCtx != nil && *req.NumCtx > 0 {
		genReq.NumCtx = *req.NumCtx
	} else if genReq.NumCtx == 0 && genReq.Options.NumCtx > 0 {
		genReq.NumCtx = genReq.Options.NumCtx
	}

	// Шаг 3: keep_alive — до R65d /api/chat его не принимал вообще, поэтому
	// клиент не мог ни выгрузить модель, ни продлить её жизнь в VRAM.
	genReq.KeepAlive = keepAliveFromRaw(req.KeepAlive)
	genReq._keepAliveDuration = parseKeepAlive(genReq.KeepAlive)

	return genReq
}

// keepAliveFromRaw нормализует Ollama keep_alive к строке.
//
// Ollama допускает как строку ("5m", "0", "-1"), так и число (секунды, -1).
func keepAliveFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if f, ferr := n.Float64(); ferr == nil {
			if f < 0 {
				return "-1"
			}
			return (time.Duration(f) * time.Second).String()
		}
	}
	return ""
}

// formatWantsJSON — true если клиент запросил JSON-вывод (format:"json"
// или объект JSON-схемы). Фактическое ограничение вывода выполняет chat
// template / грамматика в C-bridge; здесь значение используется для
// диагностики и для будущего проброса в params.
func formatWantsJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(strings.TrimSpace(s), "json")
	}
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// thinkEnabledFromRaw — true/false/"low"/"medium"/"high" → (enabled, explicit).
// explicit=false означает «клиент не задавал think» (использовать конфиг).
func thinkEnabledFromRaw(raw json.RawMessage) (enabled bool, explicit bool) {
	// R66b: `"think": null` — клиент поле не задавал (а не «выключить»).
	if len(raw) == 0 || strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "false", "none", "off":
			return false, true
		default:
			// "true" / "low" / "medium" / "high" — reasoning включён.
			return true, true
		}
	}
	return false, false
}

// jsonSchemaText — текст JSON-схемы из format-поля ("json" → "" ; объект/массив
// → его компактный JSON). Используется только для инструкции в system-промпте.
func jsonSchemaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed
	}
	return ""
}

// buildChatPrompt — прежняя сигнатура (используется OpenAI-путём и тестами):
// без per-request переопределений.
func buildChatPrompt(msgs []chatMessage, modelName string) (string, error) {
	return buildChatPromptWithOptions(msgs, modelName, chatPromptOptions{})
}

// buildChatPromptWithOptions — сборка промпта с per-request опциями (R66b):
// think (reasoning on/off для одного запроса) и format:"json".
func buildChatPromptWithOptions(msgs []chatMessage, modelName string, opts chatPromptOptions) (string, error) {
	// Извлекаем system промпт (если есть)
	system := extractSystemFromMessages(msgs)
	lang := detectPrimaryLanguage(msgs)

	// R66b: эффективный режим reasoning = per-request think (если клиент задал)
	// поверх глобального config.EnableReasoning.
	//
	// R83/v67 (2026-10-02): fallback — НЕ только глобальный config.
	//
	// ЖИВОЙ ДЕФЕКТ (жалоба оператора: «флаг включить размышление был активен,
	// однако при ответе размышление не применилось»). Галочка «размышления» в
	// WebUI пишется в профиль модели (enableReasoning=true) и честно доезжает до
	// загрузки — /api/models показывает reasoning_enabled=true. Но сборка промпта
	// смотрела ТОЛЬКО на глобальный config.EnableReasoning (по умолчанию false) и
	// per-model состояние (inst.reasoningEnabled, на которое влияет галочка) не
	// читала вовсе: ни native enable_thinking, ни soft-инструкция не применялись.
	// Проверено на стенде через /api/v1/cppworker/debug/last-prompt: промпт
	// приходил без thinking-инструкции вообще.
	//
	// Порядок приоритета: явный think клиента > per-model (WebUI/профиль) >
	// глобальный config.EnableReasoning.
	configReasoning := PerModelReasoningEnabled(modelName)
	if currentConfig != nil && currentConfig.EnableReasoning {
		configReasoning = true
	}
	reasoningEnabled := effectiveReasoningEnabled(opts, configReasoning)

	// R66b: format:"json"/схема → инструкция структурированного вывода.
	// Грамматику C-bridge не поддерживает, поэтому ограничиваем промптом, а
	// markdown-обёртку снимаем пост-обработкой (stripJSONFence).
	if opts.JSONMode {
		system = injectJSONInstruction(system, lang, opts.JSONSchema)
	}

	// R66b (2026-09-22): инструкции (JSON/thinking) должны попасть и в
	// Jinja-шаблон. ApplyChatTemplateWithThinking НЕ принимает system отдельным
	// аргументом — он берёт его ТОЛЬКО из messages. До этого фикса format:"json"
	// не доходил до модели на native-thinking пути: system собирался, но в
	// messages не попадал (проверено через /api/v1/cppworker/debug/last-prompt —
	// prompt_head содержал только user-сообщение без system-блока).
	//
	// Legacy C-API путь (ApplyChatTemplate) роль `system` в messages игнорирует
	// и использует одноимённый аргумент, поэтому дублирования нет.
	msgs = upsertSystemMessage(msgs, system)

	// thinkingInjected защищает от повторной вставки soft-prompt: native-путь
	// может поддержать thinking сам, и тогда инструкция не нужна.
	thinkingInjected := false
	injectThinking := func() {
		if thinkingInjected {
			return
		}
		system = injectThinkingInstructionWithLang(system, lang)
		msgs = upsertSystemMessage(msgs, system)
		thinkingInjected = true
	}

	// R434 (2026-11-05): ИНСТРУМЕНТЫ — В ШАБЛОН, ПРЕАМБУЛА — ТОЛЬКО FALLBACK.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline в VS Code + gemma-4): преамбула задавала единственный
	// формат вызова, придуманный нами, модель его копировала с потерей полей, и
	// ход закрывался как «COMPLETED» без созданного файла. Причина — инструменты
	// вообще не попадали в chat template (C-мост звал
	// common_chat_templates_apply с пустым inputs.tools), поэтому нативный
	// синтаксис модели молчал, а формат задавали мы.
	//
	// Теперь: сначала пробуем отрисовать tools[] самим шаблоном. Шаблон нарисовал
	// (toolsRendered) — преамбулы нет вообще. Не нарисовал (нет секции tools в
	// шаблоне/шаблон сломан/рубильник CPPWORKER_TEMPLATE_TOOLS=prompt) — прежний
	// текстовый список как fallback.
	// R445 (2026-11-05): если в истории есть ОТКАЗЫ клиента (файл существует без
	// old_text, new_text больше лимита), добавляем точную инструкцию в system.
	// Текст ошибки модель видит и так (role:"tool"), но слабые модели его не
	// применяют: без подсказки цикл повторяется до остановки задачей.
	if hint := clientErrorHintsR445(msgs); hint != "" {
		system = strings.TrimSpace(system + "\n\n" + hint)
		msgs = upsertSystemMessage(msgs, system)
		logger.Get().Infow("client error hints injected (R445)",
			"model", modelName, "hint_len", len(hint))
	}

	// R455/R456/R457 (2026-11-06): три класса, на которых модель теряет
	// инструменты и уходит в текст:
	//   - последние команды падали с ненулевым кодом (R455);
	//   - один и тот же вызов повторён с теми же аргументами (R456);
	//   - предыдущий ответ выдал содержимое файла текстом вместо вызова (R457).
	// Инструкция добавляется в system, данные клиента не трогаем.
	if hint := clientToolGuidanceHint(msgs); hint != "" {
		system = strings.TrimSpace(system + "\n\n" + hint)
		msgs = upsertSystemMessage(msgs, system)
		logger.Get().Infow("client tool guidance injected (R455/R456/R457)",
			"model", modelName, "hint_len", len(hint),
			"command_failed", commandFailureHintR455(msgs) != "",
			"repeated_call", repeatedToolCallHintR456(msgs) != "",
			"file_body_answer", fileBodyAnswerHintR457(msgs) != "")
	}

	if len(opts.Tools) > 0 {
		if mode := templateToolsMode(); mode == templateToolsNative {
			if prompt, ok := tryRenderToolsPromptR434(modelName, msgs, opts.Tools, lang, reasoningEnabled); ok {
				return prompt, nil
			}
		}
		msgs = augmentSystemWithToolsForChoice(msgs, opts.Tools, opts.ToolChoice)
		system = extractSystemFromMessages(msgs)
	}

	// Round 11 (2026-07-28) SOFT PROMPT Injection для EnableReasoning:
	// Если config.EnableReasoning=true, добавляем "thinking instruction"
	// к system промпту. Это работает универсально для ЛЮБОЙ модели
	// (включая gemma-4-it, который не эмитит нативные <think> блоки).
	//
	// Модели с native thinking (Qwen3-thinking, DeepSeek-R1) игнорируют
	// soft prompt и продолжают эмитить <think> блоки — парсер
	// (cmd/cppworker/reasoning_content.go) корректно их извлекает.
	//
	// Round 14b (2026-07-28): native enable_thinking через
	// common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
	// Если native path работает И template поддерживает thinking —
	// soft prompt НЕ нужен, template сам эмитит <think> блоки.
	// Иначе fallback на soft prompt.
	// R475 (2026-11-07): возможности шаблона нужны не только native-пути — от них
	// зависит и ФОРМА вызова в текстовой истории (`<function=…>` вместо `[[…]]`).
	caps := templateToolCapsForModel(modelName)

	if reasoningEnabled {
		// Round 35 (2026-08-12) CRITICAL bugfix: wrap C++ native chat template
		// in recover() to catch cgo SIGSEGV. The native path calls
		// common_chat_templates_apply (c/bridge/csrc/chat_thinking.cpp) which
		// periodically SIGSEGV'ится в cgo execution (signal arrived during cgo),
		// leading to crashloop (exit code 2). Confirmed live: 5+ crashes in 1h
		// on gemma-4 with Cline 65K requests after async reload.
		//
		// SIGSEGV в cgo конвертируется в Go panic через runtime.sigpanic, и
		// этот panic CATCHABLE в том же goroutine через recover(). После catch
		// fallback на простой C API (line 320) — llama_chat_apply_template, не
		// использует C++ common::chat и стабилен.
		//
		// Trade-off: recover() в той же goroutine что и cgo — но Go runtime
		// гарантирует что panic от cgo SIGSEGV всегда recoverable. C-state
		// после SIGSEGV может быть corrupted, но следующий cgo-вызов (если он
		// будет) — уже будет проверять handles через Go-уровневые проверки.
		var nativePrompt string
		var supportsThinking bool
		var err error
		// R91 (2026-10-08): recover() переехал внутрь
		// safeApplyChatTemplateWithThinking — там же, где помечается «сломанная»
		// модель, чтобы ВСЕ точки входа в C имели одну защиту (см.
		// chat_template_guard.go).
		//
		// R246 (2026-10-26): форма tool-истории выбирается по шаблону модели
		// (см. tool_history_r246.go). Структурные вызовы + role="tool" с
		// tool_call_id — единственный вид, из которого шаблон gemma-4 рисует и
		// вызов, и результат инструмента; на паре role+content результат
		// терялся целиком и модель просила «подождать вывод».
		nativePrompt, supportsThinking, err = safeApplyChatTemplateWithThinking(
			modelName, "" /* chatTemplateOverride */, buildNativeToolHistory(msgs, caps),
			true /* enableThinking */, true, /* addGenerationPrompt */
		)
		if err != nil && err != bridge.ErrNoChatTemplate && usesStructuredToolHistory(caps) {
			// Шаблон заявил структурные tool_calls, но отрендерить их отказался
			// (например, требует arguments объектом, как Qwen3.8). Один повтор
			// текстовой формой — модель обязана увидеть результат инструмента.
			logger.Get().Warnw("buildChatPrompt: structured tool history rejected by template, retrying as text",
				"model", modelName, "error", err)
			nativePrompt, supportsThinking, err = safeApplyChatTemplateWithThinking(
				modelName, "", buildLegacyToolHistory(msgs, caps), true, true,
			)
		}
		if err == nil && nativePrompt != "" {
			if supportsThinking {
				logger.Get().Debugw("buildChatPrompt: used native enable_thinking path",
					"model", modelName, "prompt_len", len(nativePrompt), "tool_caps", caps)
				return nativePrompt, nil
			}
			// Native API works but template doesn't support enable_thinking.
			// Fallback: re-apply legacy + soft prompt in system.
			// Round 32 #12 (2026-08-11): use language-aware instruction
			// чтобы модель не отвечала на языке инструкции (mixed RU/EN).
			logger.Get().Debugw("buildChatPrompt: native enable_thinking not supported by template, using soft prompt",
				"model", modelName, "prompt_len", len(nativePrompt), "lang", lang.String())
			injectThinking()
			prompt2, err2 := safeApplyChatTemplate(modelName, system, buildLegacyToolHistory(msgs, caps), true)
			if err2 == nil && prompt2 != "" {
				return prompt2, nil
			}
			// Both native + legacy GGUF template failed — fall through to manual assembly.
		} else if err != nil && err != bridge.ErrNoChatTemplate {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplateWithThinking failed, falling back",
				"model", modelName, "error", err)
		}
	}

	// Round 11 (2026-07-28) SOFT PROMPT PATH (для моделей без native support
	// или когда EnableReasoning=false): добавляем "thinking instruction"
	// к system промпту. Работает универсально для ЛЮБОЙ instruction-tuned
	// модели (включая gemma-4-it, который не эмитит нативные <think> блоки).
	// Round 32 #12 (2026-08-11): use language-aware version.
	if reasoningEnabled {
		injectThinking()
	}

	// Применяем chat template из GGUF.
	//
	// R91 (2026-10-08): через safeApplyChatTemplate, а НЕ напрямую. Это тот самый
	// вызов, на котором cppworker падал с SIGSEGV (bridge_apply_chat_template →
	// SIGSEGV) и терял все запросы узла: recover() тогда стоял только вокруг
	// native-пути выше, а этот остался без защиты.
	prompt, err := safeApplyChatTemplate(modelName, system, buildLegacyToolHistory(msgs, caps), true)
	if err == nil && prompt != "" {
		logger.Get().Debugw("buildChatPrompt: used GGUF chat template",
			"model", modelName, "prompt_len", len(prompt))
		return prompt, nil
	}
	if err != nil && err != bridge.ErrNoChatTemplate {
		// For gemma models (gemma4 tokenizer), chat template is not supported by bridge yet.
		// Downgrade to Debug since manual prompt assembly handles it correctly.
		if isGemmaModel(modelName) {
			logger.Get().Debugw("buildChatPrompt: GGUF chat template unavailable for gemma model, using manual fallback",
				"model", modelName, "error", err)
		} else {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplate failed, falling back to manual",
				"model", modelName, "error", err)
		}
	}

	// Fallback: ручная сборка промпта.
	logger.Get().Debugw("buildChatPrompt: using manual prompt assembly (no GGUF chat template)",
		"model", modelName)
	// R66b: system-сообщение (с JSON/thinking инструкциями) уже вставлено в msgs
	// выше через upsertSystemMessage — дополнительная инъекция не нужна и
	// привела бы к дублированию инструкции в промпте.
	return buildChatPromptFromMessages(msgs, modelName), nil
}

// Round 11 (2026-07-28) Soft Prompt Injection — две helper-функции.
//
// enable_thinking native C-bridge support требует common::chat
// миграцию (отложено). В soft mode добавляем "thinking instruction"
// к system промпту, что работает для ЛЮБОЙ instruction-tuned модели.

// injectThinkingInstruction возвращает system промпт с prepended
// thinking instruction. Если исходный system пустой — возвращает
// только instruction. Если есть — instruction + "\n\n" + original.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in `<reasoning>...</reasoning>` tags". Без этого модели без нативного thinking
// (qwen3-instruct, gemma-4-it, custom fine-tunes типа
// Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive) эмитят reasoning
// КАК ОБЫЧНЫЙ ТЕКСТ — парсер SplitReasoningContent не находит теги,
// кладёт reasoning в `content` вместо `reasoning_content`.
// OpenWebUI показывает reasoning как ответ — выглядит как "неполный ответ".
//
// Round 17.1 update: live test показал, что qwen3-instruct при prompt "use
// `<think>` tags" реально использует `<reasoning>...</reasoning>` (свой
// convention). Поэтому:
//   - Soft prompt просит `<reasoning>` напрямую (более широкая совместимость)
//   - Parser (cmd/cppworker/reasoning_content.go) поддерживает ОБА варианта
//     (плюс `<thinking>`, `<analysis>`) для других custom fine-tunes
//
// Round 32 #12 (2026-08-11): DEPRECATED thin wrapper — use
// injectThinkingInstructionWithLang instead. This keeps backward
// compat for external callers; all internal cppworker callsites
// have been switched to the language-aware version.
func injectThinkingInstruction(system string) string {
	return injectThinkingInstructionWithLang(system, LangEN)
}

// injectThinkingIntoMessages prepended system message с thinking
// instruction к msgs. Если system message уже есть — конкатенирует.
// Используется для fallback path (buildChatPromptFromMessages) где
// system промпт собирается руками, а не через chat template.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in `<reasoning>...</reasoning>` tags" (см. injectThinkingInstruction).
// Round 32 #12 (2026-08-11): DEPRECATED thin wrapper — use
// injectThinkingIntoMessagesWithLang instead. Backward compat
// для external callers; internal callsites переведены на WithLang.
func injectThinkingIntoMessages(msgs []chatMessage) []chatMessage {
	return injectThinkingIntoMessagesWithLang(msgs, LangEN)
}

// extractSystemFromMessages извлекает system сообщение из списка сообщений.
func extractSystemFromMessages(msgs []chatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// msgsToBridge конвертирует []chatMessage в []bridge.ChatMessage.
// Для assistant-сообщений с ToolCalls сериализует tool_calls в content,
// так как bridge.ApplyChatTemplate не имеет отдельного поля для tool_calls.
// Для tool-сообщений content передаётся как есть (уже содержит результат вызова).
func msgsToBridge(msgs []chatMessage) []bridge.ChatMessage {
	result := make([]bridge.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		// Если assistant сообщение содержит tool_calls — сериализуем их в content
		// Round 6 #2: wrap tool_calls as JSON array [{...},{...}] in Hermes/Qwen
		// format, which llama.cpp chat_template recognizes as structured
		// tool_call (not plain-JSON). bridge.ChatMessage has no native ToolCalls
		// field (C-bridge limitation), so we serialize into Content.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			tcJSON, err := json.Marshal(m.ToolCalls)
			if err == nil && len(tcJSON) > 0 {
				toolCallsBlock := "[" + string(tcJSON) + "]"
				if content != "" {
					content = content + "\n" + toolCallsBlock
				} else {
					content = toolCallsBlock
				}
			}
		}

		result = append(result, bridge.ChatMessage{
			Role:    m.Role,
			Content: content,
		})
	}
	return result
}

// buildChatPromptFromMessages — собирает prompt из массива сообщений.
// Поддерживает tool-сообщения и assistant-сообщения с ToolCalls для function calling.
func buildChatPromptFromMessages(msgs []chatMessage, modelName string) string {
	var promptBuilder strings.Builder
	isGemma := isGemmaModel(modelName)

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>system\n")
			} else {
				promptBuilder.WriteString("<|system|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "user":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>user\n")
			} else {
				promptBuilder.WriteString("<|user|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "assistant":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>model\n")
			} else {
				promptBuilder.WriteString("<|assistant|>\n")
			}
			// Если есть tool_calls, сериализуем их в content
			// Round 6 #2: wrap tool_calls as JSON array in Hermes/Qwen format
			// (see msgsToBridge comment for rationale).
			content := msg.Content
			if len(msg.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(msg.ToolCalls)
				if err == nil && len(tcJSON) > 0 {
					toolCallsBlock := "[" + string(tcJSON) + "]"
					if content != "" {
						content = content + "\n" + toolCallsBlock
					} else {
						content = toolCallsBlock
					}
				}
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "tool":
			// Форматируем tool-сообщение: имя инструмента : результат
			content := msg.Content
			// R66b: Ollama-клиенты кладут имя инструмента в `tool_name`,
			// OpenAI-совместимые — в `name`; поддерживаем оба.
			if toolName := msg.effectiveToolName(); toolName != "" {
				content = "tool_call_result(" + toolName + "): " + content
			} else if msg.ToolCallID != "" {
				content = "tool_call_result(" + msg.ToolCallID + "): " + content
			}
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>tool\n")
			} else {
				promptBuilder.WriteString("<|tool|>\n")
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		}
	}
	// Добавляем маркер для ответа ассистента
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}

	return promptBuilder.String()
}

// writeChatStreamResponse — streaming ответ в формате NDJSON для /api/chat.
//
// thinkOverride — R108 (2026-10-13): явный think клиента (nil = не задавал).
// Разбор ответа теперь БЕЗУСЛОВНЫЙ и тего-зависимый (см. блок «Политика отдачи
// reasoning» в reasoning_content.go), поэтому имя модели здесь не участвует:
// при think=false размышления не отдаются вовсе (ни в content, ни в отдельном
// поле) — та же семантика, что у non-stream /api/chat.
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
			return false
		default:
		}
		// R83: если writer сломан (клиент отвалился, ctx.Done или write error) —
		// прекращаем генерацию немедленно. Без этого cppworker продолжал молотить
		// токены после ухода клиента: в живом логе обрыв 15:53:57, а HTTP-лог с
		// status=200 и duration=32m40s появился только в 15:54:05.
		if sw.IsBroken() {
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

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// R83/v59: пустой вывод модели — один повторный проход (см.
	// empty_output_retry_r83.go). Здесь ничего не потеряно: при raw_len=0 дельт
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
		if !shouldRetryEmptyStreamingOutput(fullOutput, contentSeen.Len(), ctx) {
			break
		}
		logger.Get().Warnw(i18n.T("cppworker.handlers_chat.empty_output_retry"),
			"model", modelName, "attempt", attempt, "raw_len", len(fullOutput),
			"raw_head", truncateForLog(fullOutput, 80),
			"prompt_tokens", countTokensSafe(modelName, prompt))
		outputBuf.Reset()
		contentSeen.Reset()
		emittedContentRawLen = 0
		atomic.StoreInt64(&firstTokenAt, 0)
		if retryErr := generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false); retryErr != nil {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.empty_output_retry_failed"),
				"model", modelName, "attempt", attempt, "error", retryErr)
			break
		}
		fullOutput = outputBuf.String()
		duration = time.Since(start)
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.empty_output_retry_done"),
			"model", modelName, "attempt", attempt, "raw_len", len(fullOutput),
			"visible_len", contentSeen.Len())
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
	if rcSendReasoning {
		rTail, cTail = rcParser.FinalizeForStream()
	} else {
		_, contentOnly, _ := SplitReasoningContent(outputBuf.String())
		if len(contentOnly) > emittedContentRawLen {
			cTail = contentOnly[emittedContentRawLen:]
		}
	}
	rTail = strings.ReplaceAll(rTail, `\\n`, "\n")
	cTail = strings.ReplaceAll(cTail, `\\n`, "\n")
	// R114b: хвост — тот же видимый канал, что и дельты (см. чистку выше).
	cTail = stripReasoningTagsInStreamDelta(cTail)
	if rTail != "" {
		rcChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]interface{}{
				"role":      "assistant",
				"thinking":  sanitizeStreamText(rTail),
				"reasoning": sanitizeStreamText(rTail),
			},
			"done": false,
		}
		rcJSON, _ := json.Marshal(rcChunk)
		sw.Writef("%s\n", rcJSON)
		sw.Flush()
	}
	if cTail != "" {
		contentSeen.WriteString(cTail)
		ccChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": sanitizeStreamText(cTail),
			},
			"done": false,
		}
		ccJSON, _ := json.Marshal(ccChunk)
		sw.Writef("%s\n", ccJSON)
		sw.Flush()
	}

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("%s\n", errJSON)
		sw.Flush()
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
	generatedTokens := countTokensSafe(modelName, fullOutput)
	promptTokens := countTokensSafe(modelName, prompt)
	evalNs := duration.Nanoseconds()
	promptEvalNs := int64(0)
	// R66b (2026-09-22): TTFT = время от начала запроса до ПЕРВОГО токена.
	// Раньше здесь было time.Since(prefillStartTime) в конце генерации, из-за
	// чего prompt_eval_duration совпадал с total_duration, а eval_duration
	// получался 0 → OpenWebUI показывал tokens/sec = 0/N/A.
	if deadline := atomic.LoadInt64(&firstTokenAt); deadline > 0 {
		ttft := deadline - prefillStartTime.UnixNano()
		if ttft >= 0 && ttft <= evalNs {
			promptEvalNs = ttft
		}
	}
	// R108 (2026-10-13): в финальный чанк кладём ТОЛЬКО видимый ответ, собранный
	// из отданных дельт (contentSeen), а не сырой fullOutput.
	//
	// Раньше здесь был cleanFinalContent(fullOutput) — весь текст модели вместе с
	// think-блоком. Настоящая Ollama в done-чанке отдаёт сам ответ без
	// размышлений; из-за расхождения балансер вынужден был вырезать этот текст
	// как «дубль» и писать WARN о несовпадении с дельтами (см.
	// internal/balancer/llamacpp_transport.go: stripNativeDoneContent).
	doneContent := cleanFinalContent(contentSeen.String())
	// R231: незакрытый блок размышлений = ответ не дописан (см.
	// FinalizeForStream). Честная причина остановки — length.
	doneReason := "stop"
	snapR320 := rcParser.Snapshot()
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
	if doneReason == "stop" && toolsBudgetExhaustedR394(params.NPredict, generatedTokens) {
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
		params.NPredict, generatedTokens); notice != "" {
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
	snapStreamR477 := sw.Snapshot()
	chunksR477, traceBytesR477, traceSHAR477 := sw.StreamTrace()
	logger.Get().Infow("/api/chat stream response summary (no tools)",
		"request_id", getRequestID(r),
		"model", modelName,
		"path", "/api/chat",
		"content_len", len(doneContent),
		"thinking_len", snapR320.ReasoningChars,
		"done_reason", doneReason,
		"generated_tokens", generatedTokens,
		"n_predict", params.NPredict,
		"streamed_bytes", snapStreamR477.BytesWritten,
		"stream_chunks", chunksR477,
		"stream_bytes", traceBytesR477,
		"stream_sha256", traceSHAR477,
	)
	doneChunk := map[string]interface{}{
		"model":                modelName,
		"created_at":           createdAt,
		"message":              map[string]string{"role": "assistant", "content": doneContent},
		"done":                 true,
		"done_reason":          doneReason,
		"total_duration":       evalNs,
		"prompt_eval_count":    promptTokens,
		"prompt_eval_duration": promptEvalNs,
		"eval_count":           generatedTokens,
		"eval_duration":        evalNs - promptEvalNs,
	}
	doneJSON, _ := json.Marshal(doneChunk)
	sw.Writef("%s\n", doneJSON)
	sw.Flush()
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
// привести arguments вызова к объекту по их схеме (см. tool_args_schema_r113.go):
// без этого клиент получает arguments-массив/голую строку, не может выполнить
// вызов и считает задачу завершённой после первой итерации.
func writeChatStreamResponseWithTools(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, jsonMode bool, thinkOverride *bool, tools []openAITool, promptNoThink string, clientCapped bool) {
	// R83 (2026-09-26): тот же safeStreamWriter, что и в writeChatStreamResponse —
	// здесь тоже пишут две горутины (heartbeat + финальные чанки), и на длинной
	// генерации без мьютекса фреймы могли перемешаться.
	sw := newSafeStreamWriter(w, r, "writeChatStreamResponseWithTools", modelName)
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
	// Round 6 Fix 5: do NOT flush headers immediately — buffer the response.
	// Previously every prose token was sent on the wire and then the final
	// chunk tried to set content="" with tool_calls — clients got garbage
	// (raw JSON in chat). New approach: callback ONLY writes to outputBuf,
	// no flushes. After inference, parse tool_calls and emit one final
	// chunk (either tool_calls or content). Heartbeat goroutine keeps
	// the connection alive during long generations.
	sw.Flush()
	ctx := r.Context()
	// R63 (2026-09-15): per-infer AbortWatcher создаётся внутри GenerateStream
	// (inference.go:521), там где доступен abortFlag от C-bridge.
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	// R66b: TTFT для prompt_eval_duration (см. writeChatStreamResponse).
	var firstTokenAt int64

	// Buffer full output for final tool_calls detection.
	var outputBuf strings.Builder

	// R404: размышления отдаём ДЕЛЬТАМИ, не дожидаясь закрытия блока.
	//
	// На tools-пути content удерживается окном (там может начаться разметка
	// tool call), но thinking вызовом инструмента быть не может — его можно
	// отдавать сразу. До R404 старт блока размышлений включал reasoningHold, и
	// при исчерпании бюджета ВНУТРИ блока клиент не получал ни байта минутами
	// (живой лог: 16647 символов размышлений, done_reason=length, 362 с, затем
	// повтор R188 ещё на столько же и обрыв).
	thinkingParser := NewReasoningStreamState()
	thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
	// R470 (2026-11-06): эхо размышлений на tools-пути — только по явной просьбе
	// клиента (think:true). Иначе Cline сохраняет их как текст нашего ответа, и
	// модель на следующем ходу видит свои размышления как свой прошлый ответ
	// (разбор — reasoning_echo_r470.go). Разбор вывода и промпт не меняются.
	reasoningEcho := toolsThinkingEchoR470(thinkOverride)
	thinkingStreamed := false
	// anyStreamed — клиенту ушёл ХОТЬ ОДИН байт (thinking или content).
	// Нужен, чтобы не запускать R188-повтор после уже отданных размышлений:
	// повтор сгенерировал бы их заново и снова съел бюджет.
	anyStreamed := false
	// R406: видимый текст клиенту уже ушёл? Нужно, чтобы аварийный повтор без
	// размышлений не дублировал уже показанный ответ.
	contentStreamed := false
	retriedNoThink := false

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
	defer func() {
		close(heartbeatStop)
		select {
		case <-heartbeatDone:
		case <-time.After(50 * time.Millisecond):
		}
	}()

	// Snapshot for chat streaming (debug endpoint).
	var streamErr error
	defer func() {
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, true, streamErr, r)
	}()

	// R83/v56 (2026-10-01): потоковая отдача content.
	//
	// Round 6 Fix 5 буферизовал ответ целиком («клиент не должен видеть сырой
	// JSON tool call»), из-за чего на медленном инстансе клиент не получал ни
	// одного токена до конца генерации и отваливался по своему таймауту.
	// Теперь prose уходит по мере генерации, а маркер tool call удерживается
	// окном toolsStreamHoldback (см. tools_stream_r83.go).
	streamContent := toolsStreamContentEnabled()
	// R190 (2026-10-20): удержание из-за блока размышлений снимается, когда блок ЗАКРЫТ.
	// Раньше 	oolsHold после reasoning-маркера больше не снимался, и видимый ответ
	// (content_len=378 в живом логе) уходил клиенту одним кадром в конце хода.
	reasoningHold := false
	var emittedLen int
	// R429 (2026-11-04): РЕАЛЬНО отданные клиенту байты content, а не позиция в
	// буфере. Раньше сводка печатала emittedLen, и она врала в двух случаях:
	//   - хвост из финального чанка (не-tool_calls) отдаётся без сдвига
	//     emittedLen → сводка занижала поток (живой пример сегодня:
	//     streamed_content_bytes=3714 при принятых клиентом 14133);
	//   - сдвиг за маркер размышлений (R414) двигает emittedLen на длину
	//     маркера, которого клиент не получал.
	// Диагностика, по которой разбирают «ответ оборван/не доехал», обязана
	// считать отправленное, а не индексы — иначе разбор ведёт не туда.
	var streamedBytes int
	var toolsHold bool
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
		streamedBytes += len(delta)
		anyStreamed = true
		contentStreamed = true
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
		thinkingStreamed = true
		anyStreamed = true
	}

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		// R83: writer broken (клиент ушёл) → прекращаем генерацию немедленно.
		if sw.IsBroken() {
			return false
		}
		// R83: чистим управляющие символы (см. stream_sanitize.go).
		token = sanitizeStreamText(token)
		outputBuf.WriteString(token)
		// R404: thinking отдаём сразу — он не может быть вызовом инструмента,
		// поэтому не ждёт закрытия блока и не попадает под reasoningHold ниже.
		if td := toolsFeedThinkingR404(thinkingParser, token); td != "" {
			emitThinkingDelta(td)
		}
		// R66b: TTFT — время первого непустого токена.
		if token != "" {
			atomic.CompareAndSwapInt64(&firstTokenAt, 0, time.Now().UnixNano())
		}
		// R190: пока открыт блок размышлений — держим; как только закрыт — отдаём дельты.
		if reasoningHold {
			if end := reasoningBlockEnd(outputBuf.String()); end > 0 {
				reasoningHold = false
				toolsHold = false
				emittedLen = end
				logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_reasoning_block_closed"),
					"model", modelName, "reasoning_end", end)
			} else {
				return true
			}
		}
		if !streamContent || toolsHold || token == "" {
			return true
		}
		buf := outputBuf.String()
		// Предохранитель 1: ответ начинается с JSON — парсер может счесть его
		// tool_calls целиком (стратегия 4), поэтому дельтами не отдаём.
		//
		// R106d (2026-10-12): то же для markdown-фенса (```json) — модели кладут
		if emittedLen == 0 && (toolsStreamStartsWithJSON(buf) || toolsStreamStartsWithFence(buf)) {
			toolsHold = true
			return true
		}
		// R106d (2026-10-12): то же для markdown-фенса (```json) — вызывать после JSON-проверки.
		// Раньше здесь стояла вторая ветка guard-а; после R190 она объединена выше.
		// Предохранитель 1b (R106c, 2026-10-12): ответ начинается с reasoning-блока
		// (<think>, <thinking>, <reasoning>, <analysis>, <|channel>thought).
		// Размышления — НЕ ответ, и клиенту в content их отдавать нельзя: Cline
		// показывал thinking как ответ («команда инструмента в квадратных
		// скобках»), а сам tool call терялся. Держим дельты и отдаём reasoning
		// отдельным полем в финальном чанке (см. reasoningOut ниже).
		if emittedLen == 0 && startsWithReasoningMarker(buf) {
			reasoningHold = true
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_reasoning"),
				"model", modelName)
			return true
		}
		// R471 (2026-11-06): ОТКРЫВАЮЩЕГО маркера может НЕ быть в тексте (канал
		// открыт шаблоном), и тогда начало вывода уходило клиенту как ответ.
		// Живой прогон: streamed_content_bytes=5363 при content_len=3323 — 2040
		// символов английских размышлений уехали в content вместе с русским
		// ответом, хотя финальный разбор считал их reasoning. Держим начало до
		// закрывающего маркера (см. reasoning_start_hold_r471.go).
		if emittedLen == 0 && reasoningStartHoldR471(prompt, modelName, reasoningEcho) {
			reasoningHold = true
			toolsHold = true
			logger.Get().Infow("R471: начало вывода удерживается до закрывающего маркера размышлений",
				"model", modelName,
				"why", "клиент не просил размышления, но модель их умеет и в промпте они включены",
				"switch", "CPPWORKER_TOOLS_THINKING")
			return true
		}
		// Предохранитель 1c (R110, 2026-10-13): ответ начинается со СЛУЖЕБНОГО
		// токена шаблона (`<end_of_turn>`, `<|end|>`, `<start_of_turn>model`).
		// Такие дельты тоже не отдаём: служебный токен — не текст ответа, а когда
		// буфер перерастал окно удержания, `cut` попадал уже ПОСЛЕ токена и клиент
		// получал его как обычный текст (живой случай: «ERROR: EMPTY
		// CONTENT<end_of_turn>» в записи сессии Cline).
		if emittedLen == 0 && toolsStreamStartsWithServiceToken(buf) {
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_service_token"),
				"model", modelName)
			return true
		}
		// Предохранитель 2: в хвосте появился маркер tool call. Prose ПЕРЕД
		// маркером обязан уйти клиенту (он уже сгенерирован и не является
		// частью вызова) — иначе окно удержания съедало бы до 32 байт ответа.
		if pos, ok := toolsStreamMarkerPos(buf, toolsStreamHoldback+len(token)); ok {
			if pos > emittedLen {
				emitContentDelta(buf[emittedLen:pos])
				emittedLen = pos
			}
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_marker_found"),
				"model", modelName, "emitted", emittedLen, "buffered", len(buf))
			return true
		}
		// Предохранитель 2b (R113, 2026-10-13): JSON-форма вызова ВНУТРИ прозы.
		// Прежние проверки держали только вывод, НАЧИНАЮЩИЙСЯ с JSON; живой прогон
		// Cline-запроса дал прозу с последующим JSON вызова — и он уехал клиенту
		// дельтами (в сессии это «инструмент в теле ответа»).
		if pos, ok := toolsStreamJSONCallPos(buf, toolsStreamHoldback+len(token)); ok {
			if pos > emittedLen {
				emitContentDelta(buf[emittedLen:pos])
				emittedLen = pos
			}
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_call_in_prose"),
				"model", modelName, "emitted", emittedLen, "buffered", len(buf))
			return true
		}
		// Предохранитель 2c (R114b, 2026-10-13): в НЕПЕРЕДАННОМ хвосте началась
		// JSON-структура. Дельты прекращаем на её первой скобке: иначе начало
		// вызова уходит клиенту обрывком («…task complete. [{"id":
		// "call_submit_and_exit","type":"function",…» — живая сессия Cline:
		// объект начинался раньше, чем в окне удержания появлялся ключ, на
		// который смотрел прежний предохранитель).
		if pos, ok := toolsStreamJSONStructPos(buf, len(buf)-emittedLen); ok {
			if pos > emittedLen {
				emitContentDelta(buf[emittedLen:pos])
				emittedLen = pos
			}
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_struct_in_tail"),
				"model", modelName, "emitted", emittedLen, "buffered", len(buf))
			return true
		}
		// Предохранитель 2d (R114b): открывающий маркер размышлений в хвосте —
		// блок начался после отданной прозы, и его тело не является ответом.
		if reason, hold := toolsStreamHoldsTailStart(buf[emittedLen:]); hold {
			toolsHold = true
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_tail_held"),
				"model", modelName, "reason", reason,
				"emitted", emittedLen, "buffered", len(buf))
			return true
		}
		cut := len(buf) - toolsStreamHoldback
		if cut <= emittedLen {
			return true
		}
		cut = toolsStreamRuneSafeCut(buf, emittedLen, cut)
		if cut <= emittedLen {
			return true
		}
		// R414 (2026-11-03): служебные маркеры размышлений (в частности
		// gemma-4 `<channel|>`) не должны уходить клиенту как текст ответа.
		// Живой случай: «…run the check command.<channel|>» в сессии Cline.
		// Сначала удерживаем хвост, который может быть НАЧАЛОМ маркера, затем
		// отдаём текст до первого ПОЛНОГО маркера и пропускаем сам маркер.
		delta := buf[emittedLen:cut]
		if hb := toolsStreamMarkerPrefixHoldbackR414(delta); hb > 0 {
			cut -= hb
			if cut <= emittedLen {
				return true
			}
			cut = toolsStreamRuneSafeCut(buf, emittedLen, cut)
			if cut <= emittedLen {
				return true
			}
			delta = buf[emittedLen:cut]
		}
		if pos, markerLen, found := toolsStreamMarkerScanR414(delta); found {
			if pos > 0 {
				emitContentDelta(delta[:pos])
			}
			// Маркер не отдаём: сдвигаем позицию ЗА него, чтобы он не попал
			// в следующую дельту.
			emittedLen += pos + markerLen
			return true
		}
		emitContentDelta(delta)
		emittedLen = cut
		return true
	}

	// /api/chat streaming with tools: full buffer, single final response.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)

	// Если во время streaming произошла ошибка — отдаём финальный chunk с error,
	// как в writeChatStreamResponse. Tool calls в этом случае не анализируем.
	if streamErr != nil {
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
		return
	}

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// ?????????? ?????????? tool_calls ?? ?????????? ??????.
	//
	// R106c (2026-10-12): СНАЧАЛА отделяем reasoning. С включёнными
	// размышлениями модель отдаёт
	//	<|channel>thought … <channel|>[{"name":"read_files", …}]
	// и раньше весь этот текст уходил в message.content: размышления не
	// отделялись, parseToolCallsFromOutput получал строку с thinking-префиксом и
	// часто не находил вызов → клиент (Cline) видел «команду инструмента в
	// квадратных скобках» вместо вызова инструмента.
	//
	// R108 (2026-10-13): разбор безусловный (по тегам, не по имени модели), а при
	// явном think=false размышления не отдаются вовсе — см. «Политика отдачи
	// reasoning» в reasoning_content.go.
	reasoningOut, outputForParse, toolCalls, hasReasoning := splitToolsOutput(fullOutput, thinkOverride, tools)
	if hasReasoning {
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_reasoning_split"),
			"model", modelName, "reasoning_len", len(reasoningOut), "reasoning_echo", reasoningEcho, "content_len", len(outputForParse),
			"reasoning_sent", reasoningOut != "" && reasoningEcho,
			"reasoning_echo", reasoningEcho)
	}
	// R406: токены первой попытки нужны для решения о повторе без размышлений
	// и для честного eval_count (если повтор состоится — сложим обе попытки).
	generatedTokens := countTokensSafe(modelName, fullOutput)
	// budgetTokens — токены ПОСЛЕДНЕЙ попытки: по ним решается «упёрлись в
	// n_predict». generatedTokens после повтора — сумма обеих попыток (usage),
	// и по ней бюджет всегда выглядел бы исчерпанным.
	budgetTokens := generatedTokens

	// R83/v59 (2026-10-01) + R110 (2026-10-13): повтор генерации, когда видимого
	// ответа не получилось.
	//
	// Живой факт (пробник debug/streamprobe, gemma-4-E4B-it-Q4_K_M, запрос Cline
	// с 18 tools): модель иногда завершает ход ПЕРВЫМ же токеном EOG — в
	// /debug-логе видно raw_len=0, то есть не сгенерировано ни байта. Клиенту
	// уходил чанк "model produced an empty response", и ни повторить, ни понять
	// причину он не мог.
	//
	// R110: тот же симптом даёт вывод из ОДНИХ служебных токенов шаблона
	// (`<end_of_turn>`) — после чистки видимого текста нет. Повтор безопасен
	// ровно тем же условием, что и раньше: клиенту не отправлено ни байта
	// (emittedLen == 0), иначе он получил бы два ответа подряд.
	//
	// Сэмплирование стохастично, поэтому повторный проход обычно даёт ответ.
	if len(toolCalls) == 0 {
		// R188 (2026-10-20): клиентский лимит длины ответа делает повтор бессмысленным —
		// тот же лимит отрежет тот же вывод. При ограниченном выводе разрешён максимум
		// один повтор (см. handlers_chat_retry_r188.go).
		retryBudget := emptyOutputRetryBudget(clientCapped)
		if clientCapped {
			// R464 (2026-11-06): «клиент задал длину» — это только ЯВНЫЙ
			// num_predict/max_tokens. Наш собственный дефолт (8192 для
			// reasoning-моделей) таким ограничением не является: раньше лог
			// утверждал «клиент задал длину ответа n_predict=8192», хотя клиент
			// длину не задавал, и бюджет повторов урезался без причины.
			logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_retry_limited_r188"),
				"model", modelName, "n_predict", params.NPredict, "retries_allowed", retryBudget,
				"client_set_limit", true)
		}
		for attempt := 1; attempt <= retryBudget; attempt++ {
			// R404: клиенту уже ушли размышления (или текст) — повтор
			// сгенерировал бы их заново и снова съел бы весь бюджет. Живой
			// случай: 16647 символов thinking, 0 видимого текста, n_predict=4096,
			// повтор R188 — ещё 6 минут тишины и обрыв клиентом.
			if anyStreamed {
				logger.Get().Infow("writeChatStreamResponseWithTools: retry skipped — stream already started (R404)",
					"model", modelName, "attempt", attempt,
					"thinking_streamed", thinkingStreamed,
					"n_predict", params.NPredict)
				break
			}
			emptyOutput := shouldRetryEmptyStreamingOutput(fullOutput, emittedLen, ctx)
			// R113 (2026-10-13): повторяем и тогда, когда модель ВЫДАЛА разметку
			// вызова, но распознать её не удалось. Повтор безопасен по тому же
			// условию: клиенту не отправлено ни байта (emittedLen == 0) — в
			// tools-пути разметка удерживается окном, поэтому обычно так и есть.
			unparsedMarkup := shouldRetryUnparsedToolMarkup(fullOutput, emittedLen, ctx)
			if !emptyOutput && !unparsedMarkup {
				break
			}
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry"),
				"model", modelName, "attempt", attempt, "reason", retryReasonLabel(emptyOutput, unparsedMarkup),
				"raw_len", len(fullOutput),
				"raw_head", truncateForLog(fullOutput, 80),
				"prompt_tokens", countTokensSafe(modelName, prompt))
			outputBuf.Reset()
			emittedLen = 0
			toolsHold = false
			// R404: парсер thinking тоже начинается с чистого листа — иначе
			// дельты второй попытки смешались бы с состоянием первой.
			thinkingParser = NewReasoningStreamState()
			thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
			thinkingStreamed = false
			atomic.StoreInt64(&firstTokenAt, 0)
			if retryErr := generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false); retryErr != nil {
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry_failed"),
					"model", modelName, "attempt", attempt, "error", retryErr)
				break
			}
			fullOutput = outputBuf.String()
			duration = time.Since(start)
			reasoningOut, outputForParse, toolCalls, hasReasoning = splitToolsOutput(fullOutput, thinkOverride, tools)
			logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_retry_done"),
				"model", modelName, "attempt", attempt, "raw_len", len(fullOutput),
				"tool_calls", len(toolCalls), "visible_len", len(cleanFinalContent(outputForParse)),
				"has_reasoning", hasReasoning)
			if len(toolCalls) > 0 {
				break
			}
		}
	}

	// R406 (2026-11-03): АВАРИЙНЫЙ ПОВТОР БЕЗ РАЗМЫШЛЕНИЙ.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline): клиент задаёт max_tokens (4096), модель тратит весь
	// бюджет на thinking и не выдаёт видимого ответа. Мы честно отдаём
	// done_reason=length, Cline трактует это как «output token limit» и
	// запускает overflow-recovery: compacting → compacted, пересказ истории,
	// повтор — и часть переписки исчезает, а клиент падает с
	// «text part … not found».
	//
	// Повтор с ВЫКЛЮЧЕННЫМ thinking (тот же бюджет, тот же запрос, только
	// enable_thinking=false в chat template) даёт модели шанс ответить вместо
	// повторного ухода в размышления. Условие — ничего видимого ещё не ушло
	// (thinking-дельты не считаются: они уже показаны и повторять их не нужно).
	if toolsShouldRetryWithoutThinkingR406(params.NPredict, generatedTokens, fullOutput,
		cleanFinalContent(outputForParse), promptNoThink, contentStreamed, len(toolCalls), retriedNoThink) {

		attempt1Tokens := generatedTokens
		logger.Get().Warnw("writeChatStreamResponseWithTools: reasoning exhausted the output budget — retrying with thinking disabled (R406)",
			"model", modelName, "n_predict", params.NPredict,
			"generated_tokens", generatedTokens,
			"reasoning_chars", reasoningCharsRawR394(fullOutput),
			"thinking_streamed", thinkingStreamed)
		outputBuf.Reset()
		emittedLen = 0
		toolsHold = false
		reasoningHold = false
		// thinkingStreamed НЕ сбрасываем: клиент уже видел размышления первой
		// попытки, в финале их дублировать нельзя.
		thinkingParser = NewReasoningStreamState()
		thinkingParser.SetImpliedOpen(false)
		atomic.StoreInt64(&firstTokenAt, 0)
		retriedNoThink = true
		if retryErr := generateStreamWithRamFallback(ctx, modelName, promptNoThink, params, callback, false); retryErr != nil {
			logger.Get().Warnw("writeChatStreamResponseWithTools: no-thinking retry failed (R406)",
				"model", modelName, "error", retryErr)
		} else {
			fullOutput = outputBuf.String()
			duration = time.Since(start)
			reasoningOut, outputForParse, toolCalls, hasReasoning = splitToolsOutput(fullOutput, thinkOverride, tools)
			// eval_count — сумма обеих попыток: клиент видел thinking первой и
			// ответ второй; занижать нельзя (по usage он считает контекст).
			// Для решения о бюджете берём ТОЛЬКО вторую попытку: первая уже
			// израсходовала n_predict на размышления, но ответ второй может
			// быть полным.
			retryTokens := countTokensSafe(modelName, fullOutput)
			generatedTokens = attempt1Tokens + retryTokens
			budgetTokens = retryTokens
			logger.Get().Infow("writeChatStreamResponseWithTools: no-thinking retry produced output (R406)",
				"model", modelName,
				"attempt1_tokens", attempt1Tokens,
				"attempt2_tokens", generatedTokens-attempt1Tokens,
				"tool_calls", len(toolCalls),
				"visible_len", len(cleanFinalContent(outputForParse)))
		}
	}

	// R247 (2026-10-26), этап A+F плана: STRICT-аргументы.
	//
	// Клиент объявил tools[].function.strict=true — значит аргументы обязаны
	// соответствовать схеме (structured outputs). Ремонт R246 покрывает типовые
	// расхождения, но не все; раньше невалидный вызов уходил клиенту, тот отвечал
	// ошибкой инструмента, и агент тратил ход.
	//
	// Здесь: проверить по схеме и, если клиенту ещё ничего не отправлено, один
	// раз повторить генерацию (тот же промпт — сэмплирование стохастично).
	// Выключатель: CPPWORKER_STRICT_ARGS_RETRY=0.
	if len(toolCalls) > 0 && emittedLen == 0 && !anyStreamed && strictArgsRetryEnabled() {
		if violations := strictToolCallViolations(toolCalls, tools); len(violations) > 0 {
			logger.Get().Warnw("strict tool args invalid, retrying generation once",
				"model", modelName, "tool", toolCalls[0].Function.Name,
				"violations", truncateForLog(strings.Join(violations, "; "), 300))
			outputBuf.Reset()
			toolsHold = false
			thinkingParser = NewReasoningStreamState()
			thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
			thinkingStreamed = false
			atomic.StoreInt64(&firstTokenAt, 0)
			if retryErr := generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false); retryErr == nil {
				fullOutput = outputBuf.String()
				duration = time.Since(start)
				reasoningOut, outputForParse, toolCalls, hasReasoning = splitToolsOutput(fullOutput, thinkOverride, tools)
				if stillInvalid := strictToolCallViolations(toolCalls, tools); len(stillInvalid) > 0 {
					logger.Get().Warnw("strict tool args still invalid after retry",
						"model", modelName, "violations", truncateForLog(strings.Join(stillInvalid, "; "), 300))
				} else if len(toolCalls) > 0 {
					logger.Get().Infow("strict tool args valid after retry",
						"model", modelName, "tool", toolCalls[0].Function.Name)
				}
			} else {
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry_failed"),
					"model", modelName, "error", retryErr)
			}
		}
	}

	// R404: выпустить хвост, который парсер thinking удерживал (возможный
	// неполный маркер). Без этого последние символы размышлений терялись бы:
	// они не входили ни в одну дельту, а в финальном чанке thinking мы больше
	// не дублируем, если он уже отдан дельтами.
	if thinkingTail, _ := thinkingParser.FinalizeForStream(); thinkingTail != "" {
		emitThinkingDelta(thinkingTail)
	}

	// R394.2 (2026-11-04): сколько токенов реально сгенерировано — считаем
	// ТОКЕНИЗАТОРОМ по сырому выводу (то же число, что уходит в eval_count).
	//
	// ЖИВОЙ ПРОГОН на стенде показал, почему НЕЛЬЗЯ считать вызовы callback'а:
	// при num_predict=16 воркер выдал eval_count=17, а callback'ов с непустым
	// токеном было 14 (часть токенов приходит пустой строкой/служебной).
	// Счётчик по callback'ам занижал вывод, бюджет считался неизрасходованным,
	// и обрезанный ответ снова закрывался как done_reason=stop — ровно то, что
	// чинит R394. Токенизатор даёт точное число независимо от того, как мост
	// режет поток на куски.
	//
	// R406: generatedTokens посчитан выше и уже учитывает возможный повтор без
	// размышлений (сумма обеих попыток) — второй раз не пересчитываем.

	// R113 (2026-10-13): РАЗМЕТКА ВЫЗОВА НЕ ЯВЛЯЕТСЯ ОТВЕТОМ.
	//
	// Проверка стоит ЗДЕСЬ, до ветки «сырой вывод как content»: cleanFinalContent
	// разметку вызова не знает и НЕ делает вывод пустым, поэтому прежняя ветка не
	// срабатывала и разметка уезжала клиенту через финальный чанк (живой прогон:
	// content 2456 символов с сырым JSON вызова). Агентский клиент видит тогда
	// финальный ответ вместо вызова и помечает задачу выполненной — жалоба
	// «инструмент опять оказался в теле ответа».
	//
	// R430 (2026-11-04): если разметка найдена, а вызов не распознан, а проза
	// ЕСТЬ (markup_stripped), клиенту мало одной прозы: агентский клиент считает
	// ход завершённым и задачу — выполненной, хотя инструмент не вызывался
	// (живая жалоба: «Cline пишет COMPLETED, файла нет»). Поэтому к прозе
	// добавляется пояснение — один раз, в одном месте (см. unparsedNoticeR430).
	var unparsedNoticeR430 string
	// R468 (2026-11-06): ПОЧЕМУ вызов не выполнен — две разные причины, и
	// клиенту нельзя называть не ту.
	//
	// Живой артефакт (RAW-20261106-153057): модель выдала нативный вызов
	// call:editor{…}, парсер его РАЗОБРАЛ, но в запросе было объявлено 5
	// инструментов без editor — R428 правильно отбросил вызов. Текст же
	// говорил «нераспознанный формат», и оператор шёл чинить парсер.
	// Здесь считается именно случай «распознали, но не объявлен».
	undeclaredToolCallNames := undeclaredCallNamesR468(outputForParse, tools)
	honestNotice := unparsedToolCallNoticeR430
	if len(undeclaredToolCallNames) > 0 {
		honestNotice = undeclaredCallNoticeR468(undeclaredToolCallNames, tools)
	}
	if len(toolCalls) == 0 && looksLikeToolCallMarkup(outputForParse) {
		visibleAfterStrip := strings.TrimSpace(stripToolCallMarkup(cleanFinalContent(outputForParse)))
		if visibleAfterStrip == "" {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_unparsed"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"emitted_len", emittedLen,
				"preview", truncateForLog(outputForParse, 300))
			// R427: СЫРОЙ вывод сохраняем ПЕРЕД принятием решения.
			// R468: reason называет настоящую причину — «вызов распознан, но
			// инструмент не объявлен» отдельно от «разметку не распознали».
			unparsedReason := "markup_unparsed"
			if len(undeclaredToolCallNames) > 0 {
				unparsedReason = "call_undeclared"
			}
			recordUnparsedCaptureFieldsR468(modelName, "/api/chat", unparsedReason,
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools,
				undeclaredToolCallNames)
			if len(undeclaredToolCallNames) > 0 {
				logger.Get().Warnw("R468: вызов распознан, но инструмент не объявлен клиентом",
					"model", modelName,
					"called", strings.Join(undeclaredToolCallNames, ","),
					"declared_count", len(tools))
			}
			// R428: ХОД БОЛЬШЕ НЕ УБИВАЕТСЯ. Раньше здесь уходил errChunk с
			// done_reason:"error" и пустым content: клиент (Cline) сохранял наше
			// сообщение об ошибке как реплику ассистента, ход терялся, а следующий
			// ответ модели строился уже по испорченной истории (живая сессия:
			// после такого срыва обе модели начали писать файлы текстом).
			// Теперь отдаём честное пояснение как обычный финальный ответ —
			// клиент продолжает сессию, а сырой вывод лежит в диагностике.
			// R320: причина НЕ прошивается "stop" — считаем её по контракту
			// (обрезанный reasoning/бюджет = length), иначе клиент считает ход
			// законченным. Контракт R428: этот чанк — обычный финальный ответ.
			noticeReason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			noticeChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]string{
					"role":    "assistant",
					"content": honestNotice,
				},
				"done":        true,
				"done_reason": noticeReason,
			}
			noticeJSON, _ := json.Marshal(noticeChunk)
			sw.Writef("%s\n", noticeJSON)
			sw.Flush()
			return
		} else {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_stripped"),
				"model", modelName,
				"visible_len", len(visibleAfterStrip),
				"raw_len", len(fullOutput))
			// R427: разметка снята, но проза доставлена — фиксируем СЫРОЙ вывод:
			// так видно, в какой форме модель прислала вызов и почему он не распознан.
			unparsedReason := "markup_stripped"
			if len(undeclaredToolCallNames) > 0 {
				unparsedReason = "call_undeclared"
			}
			recordUnparsedCaptureFieldsR468(modelName, "/api/chat", unparsedReason,
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools,
				undeclaredToolCallNames)
			if len(undeclaredToolCallNames) > 0 {
				logger.Get().Warnw("R468: вызов распознан, но инструмент не объявлен клиентом",
					"model", modelName,
					"called", strings.Join(undeclaredToolCallNames, ","),
					"declared_count", len(tools))
			}
			// R430: пояснение добавляется к прозе (см. unparsedNoticeR430 выше) —
			// клиент видит, что вызов не выполнен, а проза не теряется.
			// R468: текст зависит от причины — «не объявлен» вместо
			// «нераспознанный формат», когда мы его распознали.
			unparsedNoticeR430 = honestNotice
		}
	}

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if len(toolCalls) == 0 && reasoningOut == "" && strings.TrimSpace(cleanFinalContent(outputForParse)) == "" {
		// R83/v51 (2026-10-12): не врём клиенту "empty response", если модель
		// реально что-то сгенерировала — просто наша очистка/парсер не смогли
		// распознать вывод (например, незакрытый tool-call блок, который срезал
		// antiprompt, или служебные токены, оставшиеся без полезного текста).
		// Отдаём сырой (но безопасный для клиента) вывод как content, чтобы
		// пользователь видел, что именно ответила модель.
		rawPreview := strings.TrimSpace(sanitizeStreamText(outputForParse))
		meaningful := stripKnownServiceTokens(rawPreview)
		// R113 (2026-10-13): РАЗМЕТКА ВЫЗОВА НЕ ЯВЛЯЕТСЯ ОТВЕТОМ.
		//
		// Раньше здесь сырой вывод отдавался как content — и клиент получал в теле
		// ответа текст вызова инструмента (живая жалоба: «инструмент опять оказался
		// в теле ответа»). Для агентского клиента это худший исход: вызова он не
		// видит (tool_calls пуст), считает сообщение финальным ответом и помечает
		// задачу выполненной после первой итерации (COMPLETE).
		//
		// Правильное поведение: если в выводе есть обёртка вызова, а вызов не
		// распознан — сообщить об ошибке, а не отдавать разметку текстом.
		if looksLikeToolCallMarkup(outputForParse) && stripToolCallMarkup(meaningful) == "" {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_unparsed"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"emitted_len", emittedLen,
				"preview", truncateForLog(outputForParse, 300))
			// R427: второй вход в ту же ошибку (путь «пустой видимый вывод»).
			// Сырой вывод сохраняем для диагностики.
			recordUnparsedCapture(modelName, "/api/chat", "empty_after_strip",
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools)
			// R428: ход не убиваем — уходим в обычную ветку ниже, которая отдаст
			// пояснение вместо done_reason:"error" (см. комментарий выше).
			// R320: причина НЕ прошивается "stop" — считаем её по контракту
			// (обрезанный reasoning/бюджет = length), иначе клиент считает ход
			// законченным. Контракт R428: этот чанк — обычный финальный ответ.
			noticeReason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			noticeChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]string{
					"role":    "assistant",
					"content": honestNotice,
				},
				"done":        true,
				"done_reason": noticeReason,
			}
			noticeJSON, _ := json.Marshal(noticeChunk)
			sw.Writef("%s\n", noticeJSON)
			sw.Flush()
			return
		}
		// R394.2 (2026-11-04): бюджет ушёл в размышления, а клиент их не видит
		// (think=false) — видимого вывода нет вообще. Раньше такой ход закрывался
		// ошибкой «model produced an empty response» (или пустым stop), и агентский
		// клиент терял ход: модель на самом деле рассуждала и упёрлась в лимит.
		// Отдаём пояснение и честную причину — клиент продолжит ход вместо
		// «пустого ответа». Разметку вызова по-прежнему считаем ошибкой (выше).
		// R417 (2026-11-03): если РАСПОЗНАН tool_calls — это валидный ход, а не
		// «пустой ответ». Пояснение «модель сгенерировала только размышления»
		// здесь не нужно: клиент получит его как текст ответа рядом с вызовом
		// инструмента и сохранит в истории (живой случай: Qwen3.8-9B, вызов
		// read_files + пояснение в content).
		if emittedLen == 0 {
			if notice := terminalContentR417("", fullOutput, len(toolCalls)); notice != "" {
				reason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
					len(toolCalls), params.NPredict, budgetTokens)
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_reasoning_only_notice"),
					"model", modelName, "raw_len", len(fullOutput),
					"reasoning_chars", reasoningCharsRawR394(fullOutput),
					"n_predict", params.NPredict, "generated_tokens", generatedTokens,
					"done_reason", reason)
				noticeChunk := map[string]interface{}{
					"model":       modelName,
					"created_at":  createdAt,
					"message":     map[string]string{"role": "assistant", "content": notice},
					"done":        true,
					"done_reason": reason,
				}
				noticeJSON, _ := json.Marshal(noticeChunk)
				sw.Writef("%s\n", noticeJSON)
				sw.Flush()
				return
			}
		}
		if rawPreview != "" && meaningful != "" {
			// R83/v56: если часть ответа уже ушла дельтами, повторно её не
			// отправляем — иначе клиент, складывающий чанки, увидит текст дважды.
			tail := rawPreview
			if emittedLen > 0 && emittedLen <= len(fullOutput) {
				// R429/R430: и здесь режем по началу разметки вызова — иначе
				// «сырой вывод как content» снова вернул бы тело вызова клиенту.
				tail, _ = visibleTailR429(fullOutput, emittedLen)
			}
			// R113: даже в «сыром» тексте вырезаем разметку вызова — она не ответ.
			tail = stripToolCallMarkup(tail)
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_unparsed_raw_content"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"clean_len", len(cleanFinalContent(outputForParse)),
				"emitted_len", emittedLen,
				"preview", truncateForLog(tail, 300))
			emitContentDelta(tail)
			doneChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message":    map[string]string{"role": "assistant", "content": ""},
				"done":       true,
				// R394: причина считается с учётом исчерпанного клиентского лимита
				// (R320 знал только про незакрытый reasoning и давал stop при обрыве).
				"done_reason": toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
					len(toolCalls), params.NPredict, budgetTokens),
			}
			doneJSON, _ := json.Marshal(doneChunk)
			sw.Writef("%s\n", doneJSON)
			sw.Flush()
			return
		}
		// R83/v56: если что-то уже отдано дельтами, «пустым» ответ назвать
		// нельзя — закрываем стрим без ложной ошибки. Причина при этом честная:
		// при исчерпанном бюджете это length, а не stop (R394).
		if emittedLen > 0 {
			reason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_empty_after_prefix"),
				"model", modelName, "emitted_len", emittedLen, "raw_len", len(fullOutput),
				"n_predict", params.NPredict, "generated_tokens", generatedTokens,
				"visible_len", len(strings.TrimSpace(cleanFinalContent(outputForParse))),
				"reasoning_chars", reasoningCharsRawR394(fullOutput),
				"done_reason", reason)
			doneChunk := map[string]interface{}{
				"model":       modelName,
				"created_at":  createdAt,
				"message":     map[string]string{"role": "assistant", "content": ""},
				"done":        true,
				"done_reason": reason,
			}
			doneJSON, _ := json.Marshal(doneChunk)
			sw.Writef("%s\n", doneJSON)
			sw.Flush()
			return
		}
		// R83/v51: сырой вывод тоже пуст или состоит только из служебных
		// токенов — только тогда честно сообщаем об ошибке.
		logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_empty_inference"),
			"model", modelName,
			"raw_len", len(fullOutput))
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("%s\n", errJSON)
		sw.Flush()
		return
	}

	// R65d (2026-09-20): реальные token counts вместо eval_count:0
	// (та же причина, что и в writeChatStreamResponse — OpenWebUI считает
	// tokens/sec из этих полей и показывал N/A).
	//
	// R394.2: не считаем второй раз — generatedTokens (выше) это ровно то же
	// число токенов сырого вывода, посчитанное тем же токенизатором.
	fullOutputCount := generatedTokens
	promptTokensTools := countTokensSafe(modelName, prompt)

	// R66b (2026-09-22): отделяем prefill (TTFT) от генерации. До фикса
	// eval_duration == total_duration включал prefill, а prompt_eval_duration
	// отсутствовал вовсе — tokens/sec у клиентов считался неверно.
	totalNs := duration.Nanoseconds()
	var promptEvalNsTools int64
	if ts := atomic.LoadInt64(&firstTokenAt); ts > 0 {
		if ttft := ts - start.UnixNano(); ttft >= 0 && ttft <= totalNs {
			promptEvalNsTools = ttft
		}
	}

	// R248 (2026-10-27): доехать хвост удержания.
	//
	// Живой симптом у Cline: текст ответа заканчивается посреди фразы, а следом
	// сразу идёт блок завершения — «ответ оборван». Причина: в tools-стриме
	// последние байты держатся в окне toolsStreamHoldback (чтобы не отдать
	// половину маркера вызова), и если ход закончился, пока они в удержании,
	// они НЕ отправлялись клиенту: финальный чанк закрывал стрим, а видимый
	// текст оставался обрезанным. Флашим остаток прозы (разметку вызова
	// вырезаем — она не часть ответа).
	if len(toolCalls) > 0 && emittedLen > 0 && emittedLen < len(fullOutput) {
		// R429 (2026-11-04): добор берёт ТОЛЬКО прозу ДО разметки вызова.
		//
		// Живой дефект (gemma-4 + Cline, сборка R428): вызов распознан
		// (done_reason=tool_calls, editor), но клиент получил в content всё тело
		// вызова — HTML файла целиком, 3254 байта — ОДНОЙ дельтой отсюда.
		// Причина: R248 брал `fullOutput[emittedLen:]` целиком и «чистил»
		// stripToolCallMarkup, а тот снимает только теговые обёртки и
		// ЗАВЕРШЁННЫЕ JSON-объекты; незавершённый JSON вызова (с экранированными
		// кавычками внутри new_text) оставался как есть вместе с `<channel|>`.
		// Теперь хвост режется по началу разметки: тело вызова в content не
		// попадает никогда, а проза перед ним — попадает.
		tail, callStart := visibleTailR429(fullOutput, emittedLen)
		if tail != "" {
			emitContentDelta(tail)
			logger.Get().Infow("writeChatStreamResponseWithTools: flushed held tail before done",
				"model", modelName, "tail_len", len(tail), "emitted_len", emittedLen,
				"call_start", callStart, "raw_len", len(fullOutput))
		}
		// Позиция разбора встаёт на начало разметки вызова: всё до неё уже
		// отдано, всё после — тело вызова, его место в message.tool_calls.
		emittedLen = callStart
	}

	finalChunk := map[string]interface{}{
		"model":                modelName,
		"created_at":           createdAt,
		"message":              map[string]interface{}{"role": "assistant"},
		"done":                 true,
		"total_duration":       totalNs,
		"prompt_eval_count":    promptTokensTools,
		"prompt_eval_duration": promptEvalNsTools,
		"eval_count":           fullOutputCount,
		"eval_duration":        totalNs - promptEvalNsTools,
	}

	if len(toolCalls) > 0 {
		// R440: что именно уходит клиенту (включается CPPWORKER_DEBUG_TOOL_ARGS=1).
		logOutgoingToolCalls(modelName, "/api/chat", toolCalls, fullOutput)
		// Tool calls обнаружены — эмитим message.tool_calls и очищаем content.
		// Это критично: OpenWebUI при `done_reason: "tool_calls"` ожидает
		// что content пустой, а tool_calls содержит вызовы функций.
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = ""
		// R66b (2026-09-22): нативный Ollama-формат — arguments это JSON-ОБЪЕКТ,
		// а не строка (OpenAI-форма). Cline CLI (провайдер "ollama")/ollama-python
		// читают именно объект; со строкой клиент повторно сериализовал аргументы
		// и tool call приезжал «пустым» → "Model returned empty response".
		msg["tool_calls"] = ollamaToolCallsFromOpenAI(toolCalls)
		// R106c: размышления — отдельным полем (Ollama: message.thinking), а не
		// в content: клиент показывает их как «thinking», а не как ответ.
		// R108: отдаём оба имени (`thinking` — канон §4.1, `reasoning` — legacy).
		// R404: если thinking уже ушёл дельтами — в финале НЕ дублируем его.
		// R470: эхо размышлений — только если клиент их попросил (см. reasoning_echo_r470.go).
		if reasoningEcho && reasoningOut != "" && !thinkingStreamed {
			msg["thinking"] = reasoningOut
			msg["reasoning"] = reasoningOut
		}
		finalChunk["done_reason"] = "tool_calls"
		logger.Get().Infow("writeChatStreamResponseWithTools: detected tool_calls in streamed output",
			"model", modelName, "tool_calls_count", len(toolCalls))
	} else {
		// Нет tool calls — обычный текстовый ответ. КРИТИЧНО: возвращаем
		// полный output в финальном чанке, иначе OpenWebUI видит пустой
		// content и завершает сессию ("один источник найден, ответа нет").
		//
		// Раньше здесь был bug: финальный чанк имел content: "", хотя
		// текст уже был отправлен в streaming chunks. OpenWebUI после
		// done:true берёт content из финального чанка (не агрегирует
		// из streaming chunks), поэтому видел пустой ответ.
		//
		// Делаем минимальную очистку: удаляем хвостовые служебные токены,
		// которые модель могла эмитить (</tool_call>, [TOOL_CALLS], и т.п.).
		msg := finalChunk["message"].(map[string]interface{})
		// R113: разметка вызова не является ответом — вырезаем её из видимого
		// текста, даже если рядом с ней есть проза (иначе клиент получает JSON
		// вызова в теле ответа; живой прогон: 2456 символов такого текста).
		content := stripToolCallMarkup(cleanFinalContent(outputForParse))
		// R66b: format:"json" — снимаем markdown-обёртку ```json ... ```.
		// Отдельного поля в ответе не добавляем: схема Ollama /api/chat его не
		// содержит, а лишние поля ломают строгих клиентов (см. историю с
		// unknown field в R65d).
		if jsonMode {
			content = stripJSONFence(content)
		}
		// R394.1 (2026-11-04): пояснение вместо пустого content — ОДНО место на
		// все ветки ниже. R320 ставил его только при `reasoningOut != ""`, и при
		// явном think=false (размышления клиенту не отдаются, reasoningOut пуст)
		// клиент получал content:"" + done_reason:stop — то же молчание, от
		// которого R314 защищает остальные поверхности. Признак «размышления
		// были» берём из СЫРОГО вывода (reasoningCharsRawR394), до политики.
		// R417: при распознанном tool_calls content обязан остаться пустым.
		// Пояснение «только размышления» — для случая «клиенту нечего показать»;
		// рядом с вызовом инструмента оно превращается в текст ответа и попадает
		// в историю клиента (живой случай: Qwen3.8-9B + read_files).
		content = terminalContentR417(content, fullOutput, len(toolCalls))
		// R430: разметку вызова не распознал парсер, но она ЕСТЬ — видимый текст
		// собираем до её начала + пояснение (см. unparsedNoticeR430 выше). Один раз,
		// ДО всех веток отдачи: и content финального чанка, и единственная дельта
		// короткого ответа (R424) идут из этого значения — без этого тело вызова
		// уезжало клиенту именно этим каналом (живая проба).
		if unparsedNoticeR430 != "" {
			content = visibleContentBeforeMarkupR430(outputForParse)
		}

		// R474 (2026-11-07): АРГУМЕНТЫ ВЫЗОВА, ВЫДАННЫЕ ТЕКСТОМ.
		//
		// ЖИВОЙ ДЕФЕКТ (Cline + Qwen3.8-9B, /api/chat): ход закончился
		// final_content_len=7095, tool_calls=0 — клиент получил ~7 КБ кода как
		// ответ, а в конце шли сериализованные аргументы
		// («…"path":"d:\cline-e2e-web\public\app.js"}]}}»). Файл не создан, Cline
		// следом сообщил «text part … not found» (его реакция на такой ответ).
		// Ни обёрток вызова, ни тела файла в начале текста здесь нет, поэтому
		// прежние детекторы (R430, R457) молчали. Разбор — text_arguments_dump_r474.go.
		// Условие len(tools) > 0 — как у R457: если клиент инструментов не
		// объявлял, «код + JSON в конце» может быть обычным ответом на просьбу
		// «напиши файл», и подменять его пояснением нельзя.
		if len(tools) > 0 && len(toolCalls) == 0 && unparsedNoticeR430 == "" {
			if hint, toolName, dumped := looksLikeDumpedArgumentsR474(outputForParse); dumped {
				logger.Get().Warnw("R474: аргументы вызова выданы текстом — вызов не выполнен",
					"model", modelName, "tool", toolName, "hint", hint,
					"raw_len", len(fullOutput), "visible_len", len(content))
				recordUnparsedCapture(modelName, "/api/chat", "arguments_as_text",
					fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools)
				content = dumpedArgumentsNoticeR474(toolName, hint)
			}
		}

		// R404: thinking уже мог уйти дельтами — тогда в финале его НЕ дублируем.
		thinkingField := reasoningOut
		if thinkingStreamed || !reasoningEcho {
			thinkingField = ""
		}
		if reasoningOut != "" {
			// R106c: размышления отделены — их место в message.thinking, а в
			// content только ответ.
			// R108: `thinking` — канон (контракт §4.1), `reasoning` — legacy-алиас.
			// R404: если thinking шёл дельтами, оставляем только content; при
			// этом content либо уже стримился (тогда в финал кладём пустое и
			// отдаём хвост), либо его не было — кладём целиком с пояснением.
			if thinkingField != "" {
				msg["thinking"] = thinkingField
				msg["reasoning"] = thinkingField
			}
			if streamContent && emittedLen > 0 && emittedLen <= len(fullOutput) {
				// R429/R430: хвост режется по началу разметки вызова — тело
				// вызова в content не попадает никогда.
				tail, _ := visibleTailR429(fullOutput, emittedLen)
				if unparsedNoticeR430 != "" {
					if tail == "" {
						tail = unparsedNoticeR430
					} else {
						tail = tail + "\n\n" + unparsedNoticeR430
					}
				}
				if jsonMode {
					tail = stripJSONFence(tail)
				}
				tail = stripToolCallMarkup(tail)
				if strings.TrimSpace(tail) == "" {
					// Резать нечего: остаток был только разметкой вызова. Тогда
					// видимый ответ обязан прийти в content финального чанка —
					// иначе клиент не получит ничего (R424: «text part not found»).
					msg["content"] = content
				} else {
					emitContentDelta(tail)
					msg["content"] = ""
				}
			} else {
				msg["content"] = content
			}
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_reasoning_separate_final_chunk"),
				"model", modelName, "reasoning_len", len(reasoningOut), "reasoning_echo", reasoningEcho,
				"thinking_streamed", thinkingStreamed, "content_len", len(content),
				"emitted_len", emittedLen)
		} else if streamContent && emittedLen > 0 {
			// R83/v56: префикс ответа уже ушёл дельтами. В финальный чанк кладём
			// только остаток (клиент, складывающий чанки, не должен получить
			// текст дважды), а сам остаток отдаём отдельной дельтой.
			//
			// Если маркер tool call всё-таки встречался (toolsHold), остаток
			// чистим от служебных токенов; иначе отдаём как есть — иначе
			// cleanFinalContent срезал бы значимый хвостовой пробел/перевод строки.
			tail := content
			if emittedLen <= len(fullOutput) {
				// R429/R430: хвост режется по началу разметки вызова. До этого
				// здесь отдавался ВЕСЬ остаток буфера: живой прогон gemma-4 + Cline
				// (сборка R429) отдал клиенту 3441 из 3442 байт — тело вызова
				// (HTML файла целиком) уехало как «ответ» и осело в истории
				// Cline. Чистки на месте (cleanFinalContent / stripToolCallMarkup)
				// разметку не знают: она была переэкранирована (`\"name\"`) и без
				// баланса скобок, поэтому единственный надёжный признак — её
				// НАЧАЛО, по нему и режем.
				visibleTail, _ := visibleTailR429(fullOutput, emittedLen)
				tail = visibleTail
				if jsonMode {
					tail = stripJSONFence(tail)
				}
				// R113: хвост тоже чистим от разметки вызова.
				tail = stripToolCallMarkup(tail)
			}
			if strings.TrimSpace(tail) == "" {
				// Остаток был только разметкой вызова — видимый текст отдаём в
				// финальном чанке, иначе клиент не получит ничего (R424).
				msg["content"] = content
			} else {
				emitContentDelta(tail)
				msg["content"] = ""
			}
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_text_response_streamed"),
				"model", modelName, "emitted_len", emittedLen, "tail_len", len(tail),
				"tools_hold", toolsHold, "json_mode", jsonMode)
		} else {
			// R424 (2026-11-03): КОРОТКИЙ ОТВЕТ ЦЕЛИКОМ В ОКНЕ УДЕРЖАНИЯ.
			//
			// ЖИВОЙ ДЕФЕКТ (воспроизведён прогоном Cline CLI 3.0.74 через
			// балансер, сессия conv_1793730119921_sd9evze). Tools-путь держит
			// последние toolsStreamHoldback=32 байта вывода, чтобы не отдать
			// клиенту половину маркера вызова. Если весь видимый ответ короче
			// окна (у нас — "FINAL: needle-7f3a", 18 байт), за ход не уходит
			// НИ ОДНОЙ дельты: cppworker писал streamed_content_bytes=0,
			// final_content_len=18. Дальше балансер по умолчанию вычищает
			// content из done-чанка (LB_OLLAMA_DONE_CONTENT=strip), и клиент,
			// читающий ответ дельтами, не получает ничего: Cline CLI падал с
			// «text part <id> not found» — ровно тот симптом, на который
			// жаловался оператор.
			//
			// R248 закрыл ту же дырку для случая «есть tool_calls»; здесь — для
			// обычного короткого ответа. Порядок как в соседних ветках: сначала
			// дельта, затем финальный чанк БЕЗ дубля content (клиент, читающий
			// done, тоже получает текст — но ровно один раз).
			if content != "" && streamContent {
				emitContentDelta(content)
				msg["content"] = ""
			} else {
				msg["content"] = content
			}
			logger.Get().Debugw("writeChatStreamResponseWithTools: text response (no tool_calls)",
				"model", modelName, "content_len", len(content), "json_mode", jsonMode,
				"stream_content", streamContent, "emitted_len", emittedLen)
		}
		// R394 (2026-11-04): причина остановки — с учётом исчерпанного
		// клиентского лимита. R320 ставил "length" только при незакрытом блоке
		// размышлений; если блок закрыт, а видимый ответ оборван на n_predict,
		// он давал "stop" — по контракту Ollama это «модель закончила», и Cline
		// закрывал ход, не получив продолжения («флаг завершения раньше ответа»).
		finalChunk["done_reason"] = toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
			len(toolCalls), params.NPredict, budgetTokens)
	}

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
	logger.Get().Infow("/api/chat stream response summary",
		"model", modelName,
		"path", "/api/chat",
		// R476 (2026-11-07): id, который выдал балансер — по нему ход находится
		// в логе балансера одной строкой `request trace`. Без него разбор
		// дефекта сводился к сопоставлению по времени.
		"request_id", getRequestID(r),
		"streamed_content_bytes", streamedBytes,
		// R429: позиция разбора в буфере — отдельным полем. Раньше она печаталась
		// как streamed_content_bytes и расходилась с реально отданным текстом
		// (пропущенные маркеры размышлений, хвост из финального чанка).
		"emitted_len", emittedLen,
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
	)

	doneJSON, _ := json.Marshal(finalChunk)
	sw.Writef("%s\n", doneJSON)
	sw.Flush()
}
