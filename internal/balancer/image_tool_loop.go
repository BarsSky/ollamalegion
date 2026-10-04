// image_tool_loop.go — R84 (2026-10-03): серверное исполнение вызова
// generate_image в /v1/chat/completions.
//
// СХЕМА (один запрос клиента = до двух обращений к модели):
//
//	turn 1: запрос С инструментом, но в нестриминговом режиме — нужно увидеть
//	        вызов целиком и решить, наш ли он;
//	   нет вызова → отдаём клиенту ответ как есть (в SSE, если он просил стрим);
//	   есть вызов  → генерируем картинку(и), добавляем в диалог assistant
//	                 (tool_calls) + tool(результат), убираем СВОЙ инструмент;
//	turn 2: повторный запрос к модели — она пишет финальный ответ со ссылкой на
//	        картинку; отдаём клиенту в запрошенном им режиме (стрим остаётся
//	        стримом).
//
// ПОЧЕМУ TURN 1 НЕСТРИМИНГОВЫЙ. Решение «исполнять ли вызов» принимается по
// целиком собранному ответу; разбирать SSE-поток на лету и потом «отменять»
// уже отправленное клиенту нельзя. Буферизация не съедает UX: cppworker и так
// держит окно holdback на tools-пути (tools_stream_r83.go), а второй turn идёт
// обычным потоком. Клиент, который просил стрим, получает SSE и в этом случае —
// мы синтезируем чанки из буфера.
package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// imageToolUpstreamLimitBytes — предел чтения буферизованного ответа модели
// (turn 1 и нес
// триминговый turn 2). 16 МБ: ответ на чат с запасом, но не «сколько
// пришло».
const imageToolUpstreamLimitBytes = 16 << 20

// imageToolLoopArgs — входные данные цикла.
type imageToolLoopArgs struct {
	// bodyBuf — тело запроса клиента УЖЕ с добавленным инструментом.
	bodyBuf []byte
	// originalBody — тело без инструмента: используется, если вызов не наш и
	// дальше работает обычный путь (сейчас не требуется, оставлено для ясности).
	model     string
	backendID string
	targetURL string
	client    *http.Client
	target    *imageToolTarget
}

// runImageToolLoop обрабатывает запрос с объявленным инструментом.
// true — ответ клиенту уже записан (штатный путь handler'а не нужен).
func (lr *LlamaCppRouter) runImageToolLoop(w http.ResponseWriter, r *http.Request, a imageToolLoopArgs) bool {
	clientStream := isStreamingFromBody(r.URL.Path, a.bodyBuf)
	cfg := imageToolSettings()

	// --- turn 1: нестриминговый, чтобы увидеть вызов целиком ---
	body1, err := forceNonStream(a.bodyBuf)
	if err != nil {
		logger.Get().Warnw("image tool: не удалось подготовить запрос", "error", err)
		return false
	}
	resp1, raw1, err := lr.postChatUpstream(r, a.targetURL, body1, a.client)
	if err != nil {
		logger.Get().Warnw("image tool: turn 1 не удался, отдаём обычным путём", "error", err)
		return false // штатный путь сам обработает ошибку (503/Retry-After)
	}
	if resp1.StatusCode >= 400 {
		// Ошибку модели отдаём как есть, но перехватываем n_ctx-переполнение —
		// у tools-запроса промпт длиннее, и это ровно тот случай, когда нужен
		// auto-reload, а не 413 клиенту.
		if nctxErr := ParseCppWorkerError(raw1, resp1.StatusCode, a.backendID); nctxErr != nil {
			if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
				lr.proxy.handleNCtxReload(r.Context(), w, r, a.backendID, a.model, nctxErr, a.bodyBuf)
				return true
			}
		}
		lr.writeBufferedChatResponse(w, r, a.model, resp1.StatusCode, raw1, clientStream)
		return true
	}

	message, finishReason := firstChoiceMessage(raw1)
	calls := extractImageToolCalls(message)
	if len(calls) == 0 {
		// Обычный ответ (возможно, с инструментами КЛИЕНТА — их не трогаем).
		lr.writeBufferedChatResponse(w, r, a.model, resp1.StatusCode, raw1, clientStream)
		return true
	}

	// --- исполнение вызовов ---
	results := make([]map[string]interface{}, 0, len(calls))
	for i, call := range calls {
		if i >= cfg.MaxCalls {
			// Больше MaxCalls картинок на запрос не генерируем: это защита от
			// «модель решила нарисовать десять вариантов» на слабой GPU.
			results = append(results, map[string]interface{}{
				"status": "error",
				"error":  fmt.Sprintf("лимит генераций на один запрос (%d) исчерпан", cfg.MaxCalls),
			})
			continue
		}
		res := lr.executeOneImageToolCall(r, a, call)
		results = append(results, res)
	}

	// --- turn 2: финальный ответ модели без нашего инструмента ---
	body2, err := buildFollowUpBody(a.bodyBuf, message, calls, results)
	if err != nil {
		logger.Get().Errorw("image tool: не удалось собрать follow-up", "error", err)
		lr.writeImageToolFallback(w, a.model, message, results, clientStream)
		return true
	}
	// turn 2 ВСЕГДА нестриминговый: картинка уже сгенерирована, и ссылку на неё
	// надо гарантированно вставить в ответ. Полагаться на то, что модель сама
	// вставит markdown, нельзя — на живом стенде она написала «Вот рыжий кот» и
	// ссылку потеряла. Клиенту, просившему стрим, ответ отдадим синтезированными
	// SSE-чанками (картинка и так приходит после минутной генерации).
	body2, _ = setStreamFlag(body2, false)
	resp2, raw2, err := lr.postChatUpstream(r, a.targetURL, body2, a.client)
	if err != nil || resp2.StatusCode >= 400 {
		logger.Get().Warnw("image tool: turn 2 не удался — отдаём ссылку напрямую",
			"error", err, "status", statusOf(resp2))
		lr.writeImageToolFallback(w, a.model, message, results, clientStream)
		return true
	}

	// Ссылку на картинку добавляем САМИ, если модель её не вставила: пользователь
	// должен увидеть изображение независимо от того, послушалась ли модель.
	raw2 = ensureImageMarkdown(raw2, results)
	lr.writeBufferedChatResponse(w, r, a.model, resp2.StatusCode, raw2, clientStream)
	_ = finishReason
	return true
}

// ensureImageMarkdown — добавить в финальный ответ markdown-картинки, которых в
// нём нет.
//
// ЧТО СЧИТАЕМ «УЖЕ ЕСТЬ»: ссылку на файл (публичный URL или имя файла). Модель
// могла вставить markdown сама — тогда второй раз не добавляем.
func ensureImageMarkdown(raw []byte, results []map[string]interface{}) []byte {
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	choices, _ := doc["choices"].([]interface{})
	if len(choices) == 0 {
		return raw
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if message == nil {
		return raw
	}
	content, _ := message["content"].(string)

	var missing []string
	for _, res := range results {
		md, _ := res["markdown"].(string)
		url, _ := res["url"].(string)
		if md == "" || url == "" {
			continue
		}
		if strings.Contains(content, url) {
			continue // модель уже вставила эту картинку
		}
		missing = append(missing, md)
	}
	if len(missing) == 0 {
		return raw
	}
	content = strings.TrimRight(content, " \t\n")
	if content != "" {
		content += "\n\n"
	}
	content += strings.Join(missing, "\n\n")
	message["content"] = content
	choice["message"] = message
	choices[0] = choice
	doc["choices"] = choices
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// executeOneImageToolCall — генерация по одному вызову; ошибка становится
// содержимым tool-сообщения (модель обязана объяснить её пользователю, а не
// молча упасть).
func (lr *LlamaCppRouter) executeOneImageToolCall(r *http.Request, a imageToolLoopArgs, call imageToolCall) map[string]interface{} {
	args, err := parseImageToolArgs(call.Arguments)
	if err != nil {
		logger.Get().Warnw("image tool: некорректные аргументы вызова",
			"call", call.ID, "args", call.Arguments, "error", err)
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}
	logger.Get().Infow("image tool: генерация по вызову модели",
		"backend", a.target.BackendID, "model", a.target.Model,
		"prompt_len", len(args.Prompt), "call", call.ID)

	res, err := lr.proxy.generateImageForTool(r.Context(), a.target, args)
	if err != nil {
		logger.Get().Warnw("image tool: генерация не удалась", "error", err)
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}
	// Ссылку переписываем на публичный маршрут балансера: адрес воркера
	// клиенту недоступен, а имя файла случайно — перебором не угадать.
	if url, _ := res["url"].(string); url != "" {
		public := imageToolPublicURL(r, url)
		res["url"] = public
		res["markdown"] = "![" + shortPrompt(fmt.Sprint(res["promptUsed"])) + "](" + public + ")"
		delete(res, "promptUsed")
	}
	res["tool_call_id"] = call.ID
	return res
}

// postChatUpstream — один вызов модели (turn).
func (lr *LlamaCppRouter) postChatUpstream(r *http.Request, targetURL string, body []byte, client *http.Client) (*http.Response, []byte, error) {
	if client == nil {
		client = lr.proxy.streamingClient
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	} else {
		req.Header.Set("X-Forwarded-For", lr.proxy.getClientRealIP(r))
	}
	req.Header.Set("X-Real-IP", lr.proxy.getClientRealIP(r))

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, imageToolUpstreamLimitBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return resp, raw, fmt.Errorf("read upstream body: %w", readErr)
	}
	return resp, raw, nil
}

// forceNonStream / setStreamFlag — управление флагом stream в теле запроса.
func forceNonStream(body []byte) ([]byte, error) { return setStreamFlag(body, false) }

func setStreamFlag(body []byte, stream bool) ([]byte, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, fmt.Errorf("parse body: %w", err)
	}
	doc["stream"] = stream
	return json.Marshal(doc)
}

// firstChoiceMessage — message первого выбора и причина завершения.
func firstChoiceMessage(raw []byte) (map[string]interface{}, string) {
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, ""
	}
	choices, _ := doc["choices"].([]interface{})
	if len(choices) == 0 {
		return nil, ""
	}
	choice, _ := choices[0].(map[string]interface{})
	if choice == nil {
		return nil, ""
	}
	reason, _ := choice["finish_reason"].(string)
	message, _ := choice["message"].(map[string]interface{})
	return message, reason
}

// buildFollowUpBody — тело turn 2: исходные сообщения + assistant(tool_calls) +
// tool(результаты), и БЕЗ нашего инструмента (иначе модель может вызывать его
// бесконечно).
func buildFollowUpBody(body []byte, assistant map[string]interface{}, calls []imageToolCall, results []map[string]interface{}) ([]byte, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	messages, _ := doc["messages"].([]interface{})

	assistantMsg := map[string]interface{}{"role": "assistant"}
	if content, _ := assistant["content"].(string); content != "" {
		assistantMsg["content"] = content
	} else {
		assistantMsg["content"] = ""
	}
	if rawCalls, ok := assistant["tool_calls"]; ok {
		assistantMsg["tool_calls"] = rawCalls
	} else {
		// Вызов пришёл JSON-ом в content — синтезируем каноничную форму, чтобы
		// модель увидела свой вызов в истории так, как её учили.
		synth := make([]interface{}, 0, len(calls))
		for _, c := range calls {
			synth = append(synth, map[string]interface{}{
				"id":   c.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      "generate_image",
					"arguments": c.Arguments,
				},
			})
		}
		assistantMsg["tool_calls"] = synth
	}
	messages = append(messages, assistantMsg)

	for i, c := range calls {
		payload := map[string]interface{}{"status": "error", "error": "результат не получен"}
		if i < len(results) && results[i] != nil {
			payload = results[i]
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			raw = []byte(`{"status":"error","error":"marshal tool result"}`)
		}
		toolMsg := map[string]interface{}{
			"role":         "tool",
			"tool_call_id": c.ID,
			"content":      string(raw),
		}
		messages = append(messages, toolMsg)
	}
	doc["messages"] = messages

	// Наш инструмент убираем, инструменты клиента оставляем.
	if tools, ok := doc["tools"].([]interface{}); ok {
		kept := make([]interface{}, 0, len(tools))
		for _, t := range tools {
			if openAIToolName(t) == "generate_image" {
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) == 0 {
			delete(doc, "tools")
			delete(doc, "tool_choice")
		} else {
			doc["tools"] = kept
		}
	}
	return json.Marshal(doc)
}

// writeBufferedChatResponse — отдать буферизованный ответ клиенту в его формате.
func (lr *LlamaCppRouter) writeBufferedChatResponse(w http.ResponseWriter, r *http.Request, model string, status int, raw []byte, clientStream bool) {
	lr.proxy.addModelCapabilitiesHeaders(w, model)
	if !clientStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return
	}
	synthesizeChatSSE(w, raw, model, status)
}

// writeImageToolFallback — что отдать клиенту, если финальный ответ модели не
// получился: картинка УЖЕ сгенерирована, терять её нельзя. Складываем текст
// turn 1 (если был) и markdown-ссылки на готовые изображения.
func (lr *LlamaCppRouter) writeImageToolFallback(w http.ResponseWriter, model string, assistant map[string]interface{}, results []map[string]interface{}, clientStream bool) {
	var sb strings.Builder
	if assistant != nil {
		if content, _ := assistant["content"].(string); content != "" {
			sb.WriteString(content)
		}
	}
	for _, res := range results {
		if md, _ := res["markdown"].(string); md != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(md)
		} else if e, _ := res["error"].(string); e != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("Не удалось сгенерировать изображение: " + e)
		}
	}
	content := sb.String()
	payload := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-tool-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       map[string]interface{}{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	}
	lr.writeBufferedChatResponse(w, nil, model, http.StatusOK, raw, clientStream)
}

// synthesizeChatSSE — превратить буферизованный OpenAI-ответ в SSE-поток.
//
// ЗАЧЕМ: turn 1 всегда нестриминговый (нужно решение по целому ответу), а клиент
// мог просить стрим. Отдаём чанки в каноничной форме: role → content →
// tool_calls (инструменты КЛИЕНТА, если модель вызвала их) → finish_reason →
// [DONE]. Дробить content не нужно: клиенты принимают контент одним чанком.
func synthesizeChatSSE(w http.ResponseWriter, raw []byte, model string, status int) {
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		http.Error(w, "invalid upstream response", http.StatusBadGateway)
		return
	}
	id, _ := doc["id"].(string)
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	created := time.Now().Unix()
	if v, ok := doc["created"].(float64); ok && v > 0 {
		created = int64(v)
	}
	if m, _ := doc["model"].(string); m != "" {
		model = m
	}
	choices, _ := doc["choices"].([]interface{})
	var message map[string]interface{}
	finish := "stop"
	if len(choices) > 0 {
		if c, ok := choices[0].(map[string]interface{}); ok {
			message, _ = c["message"].(map[string]interface{})
			if fr, _ := c["finish_reason"].(string); fr != "" {
				finish = fr
			}
		}
	}

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(status)

	chunk := func(delta map[string]interface{}, finishReason interface{}) {
		payload := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		out, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
		Flush(w)
	}

	chunk(map[string]interface{}{"role": "assistant"}, nil)
	if message != nil {
		if content, _ := message["content"].(string); content != "" {
			chunk(map[string]interface{}{"content": content}, nil)
		}
		if calls, ok := message["tool_calls"]; ok {
			chunk(map[string]interface{}{"tool_calls": calls}, nil)
		}
	}
	chunk(map[string]interface{}{}, finish)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	Flush(w)
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
