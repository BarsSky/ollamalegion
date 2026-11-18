package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"sync"
	"time"
)

// writeToolCallsStream отправляет SSE-поток с tool_calls (без content).
// Формат: один chunk с delta.tool_calls, затем chunk с finish_reason="tool_calls".
//
// ВАЖНО: каждый tool_call эмитится с полем `index` — это требование OpenAI
// streaming spec. Без `index` Cline/Roo не могут правильно склеить
// инкрементальные deltas аргументов и интерпретируют tool_call как
// невалидный ("Invalid API Response").
//
// Параметр includeUsage включает финальный usage-чанк (после finish_reason
// но до [DONE]) — соответствует OpenAI stream_options.include_usage=true.
func writeToolCallsStream(w http.ResponseWriter, r *http.Request, modelName, chatID string, created int64, calls []openAIToolCall, reasoning string, prompt string, includeUsage bool) {
	// R493 (этап 7): поток пишется через общий writer — обрыв клиента виден в
	// диагностике (debug/last-stream) и в RecordStreamBreak, а не терялся.
	sw, ok := newStreamWriterR493(w, r, "writeToolCallsStream", modelName, "text/event-stream")
	if !ok {
		return
	}
	sw.WriteHeader(http.StatusOK)
	sw.Flush()

	// R216 (2026-10-21): размышления уходят ОТДЕЛЬНЫМ delta.reasoning_content
	// ПЕРЕД tool_calls. Раньше они просто терялись: клиент видел вызов
	// инструмента без объяснения, а в логах воркера reasoning_len был непустым.
	roleSent := false
	if strings.TrimSpace(reasoning) != "" {
		reasoningChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"role":              "assistant",
						"reasoning_content": reasoning,
					},
					"finish_reason": nil,
				},
			},
		}
		reasoningJSON, _ := json.Marshal(reasoningChunk)
		sw.Writef("data: %s\n\n", reasoningJSON)
		sw.Flush()
		roleSent = true
	}

	// OpenAI streaming spec требует поле `index` для каждого tool_call,
	// чтобы клиент мог склеивать инкрементальные deltas по index.
	// helper indexedToolCalls добавляет индексы 0..N-1.
	indexed := indexedToolCalls(calls)

	// Chunk 1: роль ассистента (без content, с tool_calls)
	firstDelta := map[string]interface{}{
		"content":    nil,
		"tool_calls": indexed,
	}
	if !roleSent {
		firstDelta["role"] = "assistant"
	}
	firstChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         firstDelta,
				"finish_reason": nil,
			},
		},
	}
	jsonData, _ := json.Marshal(firstChunk)
	sw.Writef("data: %s\n\n", jsonData)
	sw.Flush()

	// Chunk 2: завершение с finish_reason="tool_calls"
	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	sw.Writef("data: %s\n\n", stopJSON)
	sw.Flush()

	// Chunk 3 (опционально): usage блок, если includeUsage=true.
	// Для tool_calls output completion=JSON аргументы функций.
	// Тут output = serialized tool_calls (см. caller), используем как есть.
	if includeUsage {
		writeOpenAIUsageChunk(sw, chatID, created, modelName, prompt, reasoning+serializeToolCallsForUsage(indexed), includeUsage)
	}

	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
}

// serializeToolCallsForUsage склеивает tool_calls в строку для подсчёта
// completion_tokens. Подсчёт токенов на JSON-сериализации близок к реальному,
// т.к. модель сгенерировала этот JSON-текст.
func serializeToolCallsForUsage(calls []map[string]interface{}) string {
	b, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeStaticTextStream отправляет SSE-поток с фиксированным текстом
// (используется когда tools запросили буферизированный ответ).
//
// R216 (2026-10-21): reasoning отделяется от видимого ответа. До правки сюда
// передавался result.Output целиком, поэтому модель с `<think>…</think>`
// отдавала размышления в delta.content: OpenWebUI показывал их как тело ответа
// (без закрывающего маркера — он оставался в тексте), а при наличии tool_calls
// размышления терялись совсем. Теперь reasoning уходит отдельным
// delta.reasoning_content ПЕРЕД content.
//
// Параметр includeUsage добавляет финальный usage-чанк перед [DONE]
// (OpenAI stream_options.include_usage=true). Используется Cline/Roo для
// корректного трекинга контекстного окна.
func writeStaticTextStream(w http.ResponseWriter, r *http.Request, modelName, chatID string, created int64, reasoning, content, prompt string, includeUsage bool) {
	// R493 (этап 7): общий writer — обрыв клиента виден диагностике.
	sw, ok := newStreamWriterR493(w, r, "writeStaticTextStream", modelName, "text/event-stream")
	if !ok {
		return
	}
	sw.WriteHeader(http.StatusOK)
	sw.Flush()

	// R219 (2026-10-21): НЕ подставляем reasoning в content, если видимого
	// ответа нет.
	//
	// R216 добавил сюда R91-фолбэк («незакрытый reasoning → content»), и это
	// воспроизвело ровно ту жалобу оператора, ради которой R216 и делался: при
	// обрезанной генерации одни и те же размышления приходили И в
	// reasoning_content, И в content. На буферизированной ветке caller уже
	// разложил вывод по каналам (SplitReasoningContentFromPrompt); если content
	// пуст — значит видимого ответа действительно нет, и честный ответ клиенту:
	// reasoning_content есть, content пуст.
	content = stripToolCallMarkup(cleanFinalContent(content))
	if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) == "" {
		logger.Get().Warnw(i18n.T("cppworker.handlers_openai.static_text_stream_empty"),
			"model", modelName)
	}

	roleSent := false
	if strings.TrimSpace(reasoning) != "" {
		reasoningChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"role":              "assistant",
						"reasoning_content": reasoning,
					},
					"finish_reason": nil,
				},
			},
		}
		reasoningJSON, _ := json.Marshal(reasoningChunk)
		sw.Writef("data: %s\n\n", reasoningJSON)
		sw.Flush()
		roleSent = true
	}

	// Отправляем видимый контент одним chunk
	delta := map[string]interface{}{"content": content}
	if !roleSent {
		delta["role"] = "assistant"
	}
	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": nil,
			},
		},
	}
	jsonData, _ := json.Marshal(chunk)
	sw.Writef("data: %s\n\n", jsonData)
	sw.Flush()

	// Сигнал завершения
	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	sw.Writef("data: %s\n\n", stopJSON)
	sw.Flush()

	// Финальный usage chunk (если include_usage=true) — перед [DONE].
	// OpenAI stream_options.include_usage=true → клиент получает prompt/completion/total_tokens.
	writeOpenAIUsageChunk(sw, chatID, created, modelName, prompt, reasoning+content, includeUsage)

	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
}

// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
//
// КРИТИЧЕСКИ ВАЖНО: финальный SSE чанк ДОЛЖЕН содержать полный output в delta.content,
// а не пустую строку. Иначе OpenWebUI после done:true видит пустой content и завершает
// сессию ("один источник найден, ответа нет"). Также поддерживаем детекцию tool_calls
// в выходном тексте — если модель сгенерировала <tool_call>...</tool_call> (Hermes/Gemma-4/Qwen),
// эмитим tool_calls в финальном чанке и обнуляем content.
func writeOpenAIChatStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, includeUsage bool, tools []openAITool) {
	// Round 15 (2026-07-29): debug-логирование для диагностики missing usage chunk.
	// 2026-06-24: preflight n_ctx check BEFORE WriteHeader(200).
	// Once WriteHeader(200) is called, we can't change status code mid-stream.
	// The HTTP client would see "200 OK" with whatever body we wrote after
	// (which it would interpret as malformed SSE response).
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		// Returns *PromptExceedsNCtxError → HTTP 413 with actionable advice.
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// R493 (этап 7): единый писатель потока (заголовки, статус, учёт байт/ошибок).
	sw, ok := newStreamWriterR493(w, r, "writeOpenAIChatStream", modelName, "text/event-stream")
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	sw.WriteHeader(http.StatusOK)
	// Round 32 (2026-08-09): emit "thinking" SSE comment immediately после headers.
	// C-bridge prefill (особенно для reasoning моделей типа gemma-4) занимает 5-30s
	// на больших prompts. Без этой строки клиент получает headers + 0 байт данных
	// в течение всего prefill. С этой строкой клиент получает валидный SSE event
	// сразу — connection "alive" с точки зрения EventSource клиента (Cline/OpenWebUI).
	// Это не ускоряет сам prefill, но РАДИКАЛЬНО улучшает UX: пользователь видит
	// что запрос "пошёл" вместо "зависшего спиннера на 8+ секунд".
	// SSE comments (lines starting with `:`) per RFC spec §4.4 — клиенты игнорируют
	// но используют как keep-alive signal.
	prefillStartTime := time.Now()
	sw.Writef(": prefill_started_at=%s\n\n", prefillStartTime.Format(time.RFC3339Nano))
	sw.Flush()

	ctx := r.Context()
	// R63 (2026-09-15): AbortWatcher per-infer flag теперь создаётся внутри
	// GenerateStream (inference.go:521) — там где доступен abortFlag
	// возвращаемый C-bridge. Удаляем старый per-model watcher.
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// 2026-06-25: snapshot для /v1/chat/completions streaming.
	var streamErr error
	defer func() {
		// Round 5 Fix 3: проброс r в новую context-aware версию —
		// при client_disconnect err==nil, без ctx этого не видно.
		recordLastPromptFromErrorWithContext(modelName, "/v1/chat/completions", prompt, &params, false, streamErr, r)
	}()

	// Round 6 #7: configurable heartbeat via env. Default 15s for OpenAI SSE.
	keepaliveInterval := getHeartbeatInterval(15 * time.Second)

	// === 2026-06-26 BUGFIX: safeStreamWriter ===
	//
	// Раньше использовались локальные safeFprintf/safeFlush, которые
	// НЕ проверяли ctx.Done() и НЕ возвращали write error. После обрыва
	// клиента cppworker продолжал писать в мёртвый socket, получая
	// "broken pipe" без логирования (issue «обрыв ответа без каких-либо
	// ошибок»). safeStreamWriter:
	//   - проверяет ctx.Done() перед каждой операцией;
	//   - логирует первый write error;
	//   - записывает snapshot в /api/v1/cppworker/debug/last-stream;
	//   - потокобезопасен.
	// (писатель уже создан выше — этап 7 свёл создание в одну точку)

	// === 2026-06-26 BUGFIX: явный stopCh для keepalive goroutine ===
	//
	// Старая версия использовала defer close(tokenDone) — это создавало
	// race: горутина могла выйти по `<-ctx.Done()` ДО того, как defer
	// main-функции выполнится, и если бы потом defer пытался закрыть уже
	// закрытый канал, был бы panic. Также при коротких стримах (<15s)
	// defer мог закрыть tokenDone ДО того, как горутина успеет среагировать.
	// Решение: явный stopCh, который закрывается в defer с wg.Wait().
	stopCh := make(chan struct{})
	var keepaliveWG sync.WaitGroup

	// Накапливаем полный output параллельно для финального чанка + tool_calls detection.
	// Это критично: иначе финальный SSE чанк имел бы пустой delta.content,
	// и OpenWebUI считал бы ответ пустым после done:true.
	var outputBuf strings.Builder

	// 2026-07-01: reasoning-парсер. Разделяем токены на (reasoning, content) и
	// эмитим отдельный `delta.reasoning_content` в SSE — это позволяет
	// OpenWebUI/Cline корректно отображать think-блок и видимый ответ раздельно.
	//
	// R108 (2026-10-13): разбор БЕЗУСЛОВНЫЙ — по тегам формата, не по имени
	// модели. Раньше стоял гейт IsReasoningEnabledForRequest(modelName) с
	// «ленивым» auto-detect: до момента обнаружения тега токены уходили клиенту
	// как content, и первые символы маркера (`<thi`, `<|channel>`) утекали в
	// видимый ответ. Парсер сам удерживает неполный тег в хвосте
	// (trailingTagPrefixLen), поэтому утечки больше нет. См. «Политика отдачи
	// reasoning» в reasoning_content.go.
	rsParser := NewReasoningStreamState()
	// R216: если шаблон открыл блок размышлений в ПРОМПТЕ, вывод начинается внутри него.
	rsParser.SetImpliedOpen(PromptOpensReasoning(prompt))
	rsSentHeader := false
	// Автодетект — только ради per-model состояния (дефолты n_predict на будущих
	// запросах), на разбор он не влияет.
	rsAutoEnableDone := false

	keepaliveWG.Add(1)
	go func() {
		defer keepaliveWG.Done()
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				if !sw.Writef(": keepalive\n\n") || !sw.Flush() {
					// Client disconnected — выходим.
					return
				}
			}
		}
	}()

	// Гарантируем остановку keepalive goroutine и ожидание её завершения
	// при любом выходе (нормальный return, panic, disconnect).
	defer func() {
		close(stopCh)
		keepaliveWG.Wait()
	}()

	// writeReasoningChunk — низкоуровневый helper, эмитит ОДИН SSE чанк с указанным
	// delta (map[string]interface{}). Возвращает false при обрыве клиента / writer broken.
	writeReasoningChunk := func(delta map[string]interface{}) bool {
		if sw.IsBroken() {
			return false
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		if !sw.Writef("data: %s\n\n", jsonData) {
			return false
		}
		if !sw.Flush() {
			return false
		}
		return true
	}

	callback := func(token string) bool {
		// Безопасная проверка: если контекст отвалился или writer сломан —
		// прекращаем генерацию (callback должен вернуть false).
		if sw.IsBroken() {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		default:
		}
		// Накапливаем output для финального чанка.
		outputBuf.WriteString(token)

		// 2026-07-01: reasoning-парсер — разделяем токены на (reasoning, content).
		// Эмитим отдельный `delta.reasoning_content` в SSE, что позволяет
		// OpenWebUI/Cline корректно отображать think-блок и видимый ответ раздельно.
		//
		// R108 (2026-10-13): безусловно — разбор идёт по тегам формата, а не по
		// имени модели. Раньше стоял гейт IsReasoningEnabledForRequest(modelName) с
		// «ленивым» auto-detect: до обнаружения тега токены уходили клиенту как
		// content, и первые символы маркера (`<thi`, `<|channel>`) утекали в
		// видимый ответ. Парсер сам удерживает неполный тег в хвосте
		// (см. trailingTagPrefixLen). Политика: reasoning_content.go.
		{
			reasoningDelta, contentDelta := rsParser.Feed(token)
			// Round 32 #18 (2026-08-11): workaround для gemma-4 Q4_K_M detokenizer.
			// C-bridge отдаёт literal `\\n` (2 chars) вместо LF (1 char).
			// Заменяем ДО эмита в SSE для правильного отображения.
			reasoningDelta = strings.ReplaceAll(reasoningDelta, `\\n`, "\n")
			contentDelta = strings.ReplaceAll(contentDelta, `\\n`, "\n")
			// R83 (2026-09-26): чистим управляющие символы — клиентские
			// JSON-парсеры падают на них с «Invalid control character».
			reasoningDelta = sanitizeStreamText(reasoningDelta)
			contentDelta = sanitizeStreamText(contentDelta)

			// R108: автодетект остаётся только ради per-model состояния
			// (подходящие дефолты n_predict на будущих запросах) — на разбор и на
			// emit он больше не влияет.
			if !rsAutoEnableDone && reasoningDelta != "" {
				rsAutoEnableDone = true
				if backend != nil && backend.AutoEnableReasoning(modelName) {
					logger.Get().Warnw("reasoning auto-detected mid-stream",
						"handler", "writeOpenAIChatStream",
						"model", modelName,
						"output_len", len(outputBuf.String()),
						"hint", "model emits reasoning tags but is not in reasoning whitelist; "+
							"auto-enabled per-model reasoning defaults. To pre-set, use load-with-params "+
							"enableReasoning=true or set CPPWORKER_REASONING_ARCHS env var.")
				}
			}

			// Header-chunk: первый чанк содержит role=assistant. Отправляется ОДИН раз
			// до первого reasoning/content delta. Это упрощает клиент-парсер.
			if !rsSentHeader {
				rsSentHeader = true
				if !writeReasoningChunk(map[string]interface{}{"role": "assistant"}) {
					return false
				}
			}

			ok := true
			// R114b (2026-10-13): маркеры размышлений не должны утекать в
			// ВИДИМУЮ потоковую дельту. cleanFinalContent → stripOrphanReasoningTags
			// работает только на НЕ-потоковых путях, поэтому одиночный `</think>`
			// (prefill-канал шаблона) уезжал клиенту как текст ответа — живой
			// скриншот: строка ответа начиналась с «Let me retry the command. </think>».
			// Чистим ровно дельту content, reasoning_content не трогаем.
			if contentDelta != "" {
				contentDelta = stripReasoningTagsInStreamDelta(contentDelta)
			}
			// Сначала отправляем reasoning_delta (если есть), затем content_delta.
			// Каждый — отдельный SSE чанк с одним непустым полем.
			if reasoningDelta != "" {
				if !writeReasoningChunk(map[string]interface{}{"reasoning_content": reasoningDelta}) {
					ok = false
				}
			}
			if ok && contentDelta != "" {
				if !writeReasoningChunk(map[string]interface{}{"content": contentDelta}) {
					ok = false
				}
			}
			// Каждый входной token = 1 токен генерации, независимо от того, на какие
			// части он был разделён.
			sw.IncTokens(1)
			return ok
		}
	}
	// writeOpenAIChatStream вызывается только когда tools НЕ заданы (для tools идёт буферизированный путь в handleV1ChatCompletions). Поэтому reload при n_ctx overflow разрешён.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Round 31 #6 (2026-08-09): cancelled response. OpenAI-compatible клиенты
		// строгие к finish_reason, поэтому используем "stop" (safer) + cancelled: true.
		// См. docs/plans/cppworker-abort-api/PLAN.md DECISION Q3.
		if errors.Is(streamErr, bridge.ErrAborted) {
			cancelledChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"delta":         map[string]string{},
						"finish_reason": "stop", // backward-compat с OpenAI
					},
				},
				"cancelled": true,
			}
			cancelledJSON, _ := json.Marshal(cancelledChunk)
			sw.Writef("data: %s\n\n", cancelledJSON)
			sw.Writef("data: [DONE]\n\n")
			sw.Flush()
			return
		}
		// Специальная обработка reload-disabled-for-tools (HTTP 413 в SSE-чанке).
		if rdtErr, ok := streamErr.(*ReloadDisabledForToolsError); ok {
			errChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"delta":         map[string]string{},
						"finish_reason": "error",
					},
				},
				"error":      rdtErr.Error(),
				"code":       "tools_reload_disabled",
				"suggestion": "Reduce the number of tools, chat history length, or increase n_ctx in the model profile.",
			}
			errJSON, _ := json.Marshal(errChunk)
			sw.Writef("data: %s\n\n", errJSON)
			sw.Writef("data: [DONE]\n\n")
			sw.Flush()
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 в SSE-чанке).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"delta":         map[string]string{},
						"finish_reason": "error",
					},
				},
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
			}
			errJSON, _ := json.Marshal(errChunk)
			sw.Writef("data: %s\n\n", errJSON)
			sw.Writef("data: [DONE]\n\n")
			sw.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]string{},
					"finish_reason": "error",
				},
			},
			"error": streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}

	// ============================================================
	// Финальный чанк — содержит ПОЛНЫЙ output или tool_calls.
	// ============================================================
	// Раньше здесь был критичный bug: финальный SSE чанк имел пустой delta.content,
	// OpenWebUI после done:true видел пустой content и завершал сессию.
	fullOutput := outputBuf.String()
	toolCalls := parseToolCallsForRequest(fullOutput, tools)

	// R83/v67 (2026-10-02): финальный сброс reasoning-парсера.
	//
	// Feed() удерживает хвост, который может оказаться неполным think-тегом
	// (иначе фрагменты маркера утекали в видимый ответ: живой дефект
	// «content=<|chann, reasoning=though ...»). После EOS удерживать больше
	// нечего — выпускаем хвост как обычный текст, иначе последние символы
	// ответа потеряются.
	//
	// R108 (2026-10-13): безусловно (разбор — по тегам, не по имени модели).
	{
		rTail, cTail := rsParser.FinalizeForStream()
		rTail = sanitizeStreamText(strings.ReplaceAll(rTail, `\\n`, "\n"))
		cTail = sanitizeStreamText(strings.ReplaceAll(cTail, `\\n`, "\n"))
		// R114b: хвост — тот же видимый канал, что и дельты: одиночный `</think>`,
		// выпущенный Finalize, тоже не должен дойти до клиента.
		cTail = stripReasoningTagsInStreamDelta(cTail)
		if rTail != "" {
			writeReasoningChunk(map[string]interface{}{"reasoning_content": rTail})
		}
		if cTail != "" {
			// R91: если видимого контента за весь поток не было, Finalize отдал
			// reasoning как ответ (см. ContentFromUnclosedReasoning). Логируем —
			// иначе такой случай выглядит как «клиент получил пустой ответ».
			if snap := rsParser.Snapshot(); snap.ContentChars == 0 && snap.ReasoningChars > 0 {
				logger.Get().Warnw(i18n.T("cppworker.handlers_openai.stream_unclosed_reasoning_as_content"),
					"model", modelName, "reasoning_chars", snap.ReasoningChars)
			}
			writeReasoningChunk(map[string]interface{}{"content": cTail})
		}
	}

	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	// Если tool_calls нет И cleaned output пустой → error-чанк.
	if len(toolCalls) == 0 && strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]string{},
					"finish_reason": "error",
				},
			},
			"error": "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}

	finalDelta := map[string]interface{}{}
	finishReason := "stop"

	// R91 (2026-10-08): честная причина остановки.
	//
	// Раньше всегда стояло "stop", в том числе когда генерацию обрезал кап
	// max_tokens. Живой факт: max_tokens=96, completion_tokens=97 (упор в кап),
	// finish_reason="stop" — клиент считает ответ полным, хотя модель оборвалась
	// на середине фразы, а авто-продолжение балансера
	// (LB_AUTO_CONTINUE_ON_TRUNCATION=1) ждёт именно "length".
	// R402 (2026-11-05): бюджет считаем ТОКЕНИЗАТОРОМ по сырому выводу, а не
	// числом callback'ов.
	//
	// ЖИВОЙ ДЕФЕКТ. Здесь стояло `sw.TokensSent() >= NPredict`, но TokensSent —
	// счётчик ВЫЗОВОВ callback'а, а не токенов. R394 на живом прогоне показал:
	// num_predict=16 → eval_count=17 при 14 callback'ах. Когда callback'ов меньше
	// капа, обрезанный ответ уходил с finish_reason="stop": Cline закрывал ход и
	// не продолжал, хотя модель оборвалась на середине фразы. Тот же класс, что
	// R394 на native /api/chat; просто OpenAI-путь тогда остался непочиненным.
	//
	// fullOutput — сырой вывод, включая размышления: бюджет тратится и на них.
	generatedTokens := countTokensSafe(modelName, fullOutput)
	truncated := toolsBudgetExhaustedR394(params.NPredict, generatedTokens)
	// R231: незакрытый блок размышлений — ответ НЕ дописан, даже если кап
	// формально не достигнут (модель открыла <think>/<|channel> и оборвалась).
	// Честный finish_reason=length: клиент (Cline) продолжит, а не сочтёт
	// пустой/обрезанный content полным ответом. См. FinalizeForStream.
	if !truncated && rsParser.Snapshot().Unclosed {
		truncated = true
		logger.Get().Warnw(i18n.T("cppworker.handlers_openai.stream_reasoning_unclosed_length"),
			"model", modelName, "generated_tokens", generatedTokens, "tokens_sent", sw.TokensSent(),
			"reasoning_chars", rsParser.Snapshot().ReasoningChars)
	}

	if len(toolCalls) > 0 {
		// Tool calls обнаружены — эмитим их в финальном чанке с finish_reason="tool_calls".
		// Каждый tool_call должен иметь `index` per OpenAI streaming spec —
		// иначе Cline/Roo/openai-python SDK не могут правильно склеить инкремент.
		finalDelta["role"] = "assistant"
		finalDelta["content"] = nil
		finalDelta["tool_calls"] = indexedToolCalls(toolCalls)
		finishReason = "tool_calls"
	} else {
		// Plain-text ответ.
		// 2026-06-29: токены уже стримились инкрементально через callback
		// (см. тело цикла выше, строки 624-660 — каждый токен улетает отдельным
		// SSE-чанком с delta.content). Финальный чанк содержит только role +
		// finish_reason="stop", иначе клиент видит ДУБЛЬ: сначала N стриминговых
		// чанков с токенами, потом ещё один с ПОЛНЫМ текстом. OpenAI API это
		// допускает (финальный chunk может иметь пустой delta.content).
		finalDelta["role"] = "assistant"
		finalDelta["content"] = nil
	}
	if truncated && len(toolCalls) == 0 {
		finishReason = "length"
		logger.Get().Warnw(i18n.T("cppworker.handlers_openai.stream_truncated_by_max_tokens"),
			"model", modelName, "max_tokens", params.NPredict,
			"generated_tokens", generatedTokens, "tokens_sent", sw.TokensSent())
	}

	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         finalDelta,
				"finish_reason": finishReason,
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	sw.Writef("data: %s\n\n", stopJSON)

	// Финальный usage chunk (если include_usage=true) — перед [DONE].
	// OpenAI stream_options.include_usage=true → клиент получает prompt/completion/total_tokens.
	// Для tool_calls output completion_tokens считаем по JSON-сериализации calls;
	// для текстового output completion_tokens = sw.Tokens() (atomic counter, лояльный).
	if includeUsage {
		var completionText string
		if len(toolCalls) > 0 {
			completionText = serializeToolCallsForUsage(indexedToolCalls(toolCalls))
		} else {
			completionText = fullOutput
		}
		writeOpenAIUsageChunk(sw, chatID, created, modelName, prompt, completionText, true)
	}

	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
}
