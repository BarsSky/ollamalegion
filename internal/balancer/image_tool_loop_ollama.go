// image_tool_loop_ollama.go — R86 (2026-10-06): инструмент генерации изображений
// на поверхности OLLAMA `/api/chat`.
//
// ЗАЧЕМ. R84/R85 объявляли инструменты только на `/v1/chat/completions`. Живой
// вопрос оператора: «при запросе через Open WebUI модель не видела инструменты» —
// Open WebUI по умолчанию подключается как Ollama-сервер и ходит на
// `POST /api/chat`, а не на OpenAI-путь. Инструменты там не объявлялись вообще,
// поэтому модель не могла ни посмотреть каталог, ни нарисовать картинку.
//
// ЧТО ЗДЕСЬ: тот же цикл, что в image_tool_loop.go, но в терминах Ollama:
//   - объявление: `tools: [{type:"function", function:{...}}]` — тот же формат,
//     что у OpenAI, и cppworker его понимает (docs/api.md §Tool Calling);
//   - ответ: NDJSON (`{"model":...,"message":{...},"done":false}`), НЕ SSE;
//   - tool-результат: `{"role":"tool","content":"<JSON-строка>"}` (в Ollama-формате
//     у tool-сообщения нет tool_call_id — связь идёт по порядку и по имени);
//   - клиенту: нестриминг — один JSON-объект; стрим — синтезированные NDJSON-чанки
//     (как и на OpenAI-пути: turn 1 обязан быть нестриминговым, чтобы увидеть
//     вызов целиком).
//
// ГРАНИЦЫ. Модель и бэкенд выбирает штатный handleChat (preflight n_ctx,
// авто-загрузка текстовой модели, admission-очередь). Цикл включается ПОСЛЕ этого
// и только если инструмент реально объявлен; иначе путь не меняется ни на байт.
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
	"ollama-loadbalancer/pkg/types"
)

// ollamaChatPath — путь Ollama-чата на cppworker.
const ollamaChatPath = "/api/chat"

// imageToolLoopArgsOllama — входные данные Ollama-цикла.
type imageToolLoopArgsOllama struct {
	// bodyBuf — тело запроса клиента (уже с объявленными инструментами).
	bodyBuf []byte
	model   string
	// backendID — ID бэкенда (нужен авто-reload'у n_ctx и метрикам).
	backendID string
	// backendURL — базовый URL cppworker выбранного бэкенда ВМЕСТЕ со схемой
	// (так его отдаёт Proxy.backendHTTPAddrByID: "http://host:port"). Схему НЕ
	// добавляем повторно — на живом стенде склейка дала "http://http//host:port".
	backendURL string
	client     *http.Client
	target     *imageToolTarget
}

// injectImageToolOllama — объявить наши инструменты в теле /api/chat и вернуть
// имена добавленных (пусто = ничего не добавлено, путь не меняется).
func injectImageToolOllama(body []byte, target *imageToolTarget) ([]byte, []string) {
	if target == nil {
		return body, nil
	}
	out, ok, added := injectImageToolsOllama(body, imageToolOpenAITools(target))
	if !ok {
		return body, nil
	}
	return out, added
}

// runImageToolLoopOllama — цикл на поверхности /api/chat.
//
// true — ответ клиенту уже записан (штатный прокси-путь не нужен).
// false — цикл не справился с ПЕРВЫМ шагом, и запрос надо обслужить обычным путём
// (мы ещё ничего не отправили клиенту).
func (lr *LlamaCppRouter) runImageToolLoopOllama(w http.ResponseWriter, r *http.Request, a imageToolLoopArgsOllama) bool {
	clientStream := isStreamingFromBody(r.URL.Path, a.bodyBuf)
	cfg := lr.proxy.imageToolSettings()

	// --- turn 1: нестриминговый, чтобы увидеть вызов целиком ---
	body1, err := forceNonStream(a.bodyBuf)
	if err != nil {
		logger.Get().Warnw("image tool (ollama): не удалось подготовить запрос", "error", err)
		return false
	}
	raw1, status1, err := lr.postOllamaChat(r, a, body1)
	if err != nil {
		// Сетевую ошибку отдаём штатным путём: он умеет 503 + Retry-After.
		logger.Get().Warnw("image tool (ollama): turn 1 не удался, отдаём обычным путём",
			"backend", a.backendURL, "error", err)
		return false
	}
	if status1 >= 400 {
		// n_ctx-переполнение у tools-запроса лечится auto-reload'ом, а не 413.
		if nctxErr := ParseCppWorkerError(raw1, status1, a.backendID); nctxErr != nil {
			if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
				lr.proxy.handleNCtxReload(r.Context(), w, r, a.backendID, a.model, nctxErr, a.bodyBuf)
				return true
			}
		}
		lr.writeOllamaChatResponse(w, a, raw1, status1, clientStream)
		return true
	}

	message, done := ollamaChatMessage(raw1)
	if message == nil {
		// Не разобрали ответ — не рискуем: отдаём байты как есть.
		lr.writeOllamaChatResponse(w, a, raw1, status1, clientStream)
		return true
	}
	calls := extractImageToolCalls(message)
	if len(calls) == 0 {
		// Артефакт шаблона Qwen3 (живой случай 2026-10-06): когда инструменты
		// объявлены, но модель их не вызвала, в content попадает пустой массив
		// вызовов — клиент видит «[]» вместо ответа. Убираем мусор, сам ответ не
		// трогаем.
		//
		// ДИАГНОСТИКА: если в content ЕСТЬ маркер вызова ([TOOL_CALLS]/<tool_call>/
		// <|python_tag|>), значит детектор не распознал формат — это дефект, а не
		// «модель не вызвала инструмент». Пишем превью, чтобы формат можно было
		// починить по факту, а не по догадке.
		if content, _ := message["content"].(string); strings.Contains(content, "[TOOL_CALLS]") ||
			strings.Contains(content, "<tool_call>") || strings.Contains(content, "<|python_tag|>") {
			logger.Get().Warnw("image tool (ollama): в ответе есть маркер вызова, но вызов НЕ распознан",
				"content_head", shortForLog(content), "content_len", len(content))
		}
		lr.writeOllamaChatResponse(w, a, stripEmptyToolCallArtifact(raw1), status1, clientStream)
		return true
	}
	_ = done

	// --- исполнение вызовов (лимит считается по генерациям, см. OpenAI-путь) ---
	results := make([]map[string]interface{}, 0, len(calls))
	generations := 0
	for _, call := range calls {
		if !call.isListCall() {
			if generations >= cfg.MaxCalls {
				results = append(results, map[string]interface{}{
					"status": "error",
					"error":  fmt.Sprintf("лимит генераций на один запрос (%d) исчерпан", cfg.MaxCalls),
				})
				continue
			}
			generations++
		}
		results = append(results, lr.executeOneImageToolCallOllama(r, a, call))
	}

	// --- turn 2: финальный ответ модели без наших инструментов ---
	body2, err := buildOllamaFollowUpBody(a.bodyBuf, message, calls, results)
	if err != nil {
		logger.Get().Errorw("image tool (ollama): не удалось собрать follow-up", "error", err)
		lr.writeOllamaToolFallback(w, a, message, results, clientStream)
		return true
	}
	body2, _ = setStreamFlag(body2, false)
	raw2, status2, err := lr.postOllamaChat(r, a, body2)
	if err != nil || status2 >= 400 {
		logger.Get().Warnw("image tool (ollama): turn 2 не удался — отдаём ссылку напрямую",
			"error", err, "status", status2)
		lr.writeOllamaToolFallback(w, a, message, results, clientStream)
		return true
	}

	raw2 = ensureImageMarkdownOllama(raw2, results)
	lr.writeOllamaChatResponse(w, a, raw2, status2, clientStream)
	return true
}

// executeOneImageToolCallOllama — исполнение одного вызова (генерация или каталог).
func (lr *LlamaCppRouter) executeOneImageToolCallOllama(r *http.Request, a imageToolLoopArgsOllama, call imageToolCall) map[string]interface{} {
	args, err := parseImageToolArgsFor(call.Arguments, call.isListCall())
	if err != nil {
		logger.Get().Warnw("image tool (ollama): некорректные аргументы вызова",
			"call", call.ID, "tool", call.Name, "args", call.Arguments, "error", err)
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}

	if call.isListCall() {
		logger.Get().Infow("image tool (ollama): модель запросила каталог моделей",
			"call", call.ID, "family", args.Family)
		res := lr.proxy.imageToolCatalogResult(r.Context(), args.Family)
		res["tool_call_id"] = call.ID
		// ФИЛЬТР НЕ ДАЛ НИЧЕГО (живой случай 2026-10-06): модель вызвала каталог с
		// family="stable-diffusion", получила пустой список и дальше придумала имя
		// модели. Отвечаем явной ошибкой с перечнем доступных имён — модель
		// исправляется на следующем turn (в том числе если это был вызов каталога
		// вместе с генерацией по выдуманному имени).
		if status, _ := res["status"].(string); status == "empty" {
			out := map[string]interface{}{
				"status":       "error",
				"error":        res["summary"],
				"tool_call_id": call.ID,
			}
			if avail, ok := res["availableModels"]; ok {
				out["availableModels"] = avail
			}
			return out
		}
		return res
	}

	logger.Get().Infow("image tool (ollama): генерация по вызову модели",
		"backend", a.target.BackendID, "model", a.target.Model,
		"requested_model", args.Model, "prompt_len", len(args.Prompt), "call", call.ID)
	// Промпт в лог (кратко): живой случай 2026-10-06 — генерация ушла с
	// prompt_len=6, и клиент получил красный квадрат вместо картинки. Без текста
	// промпта в логе причину не видно.
	logger.Get().Infow("image tool (ollama): промпт вызова",
		"call", call.ID, "prompt", shortForLog(args.Prompt),
		"negative_prompt", shortForLog(args.NegativePrompt),
		"width", args.Width, "height", args.Height, "steps", args.Steps)

	res, err := lr.proxy.generateImageForTool(r.Context(), a.target, args)
	if err != nil {
		logger.Get().Warnw("image tool (ollama): генерация не удалась", "error", err)
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}
	if url, _ := res["url"].(string); url != "" {
		public := imageToolPublicURL(r, url)
		res["url"] = public
		res["markdown"] = "![" + shortPrompt(fmt.Sprint(res["promptUsed"])) + "](" + public + ")"
		delete(res, "promptUsed")
	}
	res["tool_call_id"] = call.ID
	return res
}

// postOllamaChat — один вызов модели на /api/chat (NDJSON-ответ читаем целиком).
func (lr *LlamaCppRouter) postOllamaChat(r *http.Request, a imageToolLoopArgsOllama, body []byte) ([]byte, int, error) {
	if a.backendURL == "" {
		return nil, 0, fmt.Errorf("пустой адрес бэкенда")
	}
	// Адрес приходит СО СХЕМОЙ (Proxy.backendHTTPAddrByID → "http://host:port"):
	// добавлять "http://" здесь нельзя — получается "http://http//host:port".
	endpoint := strings.TrimRight(a.backendURL, "/") + ollamaChatPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Токен бэкенда: тот же приоритет, что у остальных server-server вызовов
	// (запись бэкенда → env-фолбэк). Без него cppworker с включённой авторизацией
	// ответил бы 401, и цикл молча ушёл бы на штатный путь.
	if token := lr.proxy.cppWorkerAPITokenFor(a.backendURL); token != "" {
		req.Header.Set(types.HeaderXAPIToken, token)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	} else {
		req.Header.Set("X-Forwarded-For", lr.proxy.getClientRealIP(r))
	}
	req.Header.Set("X-Real-IP", lr.proxy.getClientRealIP(r))

	client := a.client
	if client == nil {
		client = lr.proxy.streamingClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, imageToolUpstreamLimitBytes))
	if readErr != nil {
		return raw, resp.StatusCode, fmt.Errorf("read upstream body: %w", readErr)
	}
	return raw, resp.StatusCode, nil
}

// cppWorkerAPITokenFor — токен cppworker по адресу бэкенда (host:port).
//
// Нужен потому, что цикл работает с ГОТОВЫМ адресом, а не с ID бэкенда: ID
// теряется при выборе адреса в handleChat. Ищем бэкенд по эффективному адресу;
// если не нашли — берём первый токен из Auth.Tokens (в bundled-стеке он один на
// все сервисы) — ровно так же, как это делает nctx_reload_handlers.go.
func (p *Proxy) cppWorkerAPITokenFor(backendURL string) string {
	if p == nil {
		return ""
	}
	if backendURL != "" {
		for _, b := range p.GetAllBackends() {
			if p.backendHTTPAddrByID(b.ID) == backendURL {
				if b.CppWorkerApiToken != "" {
					return b.CppWorkerApiToken
				}
				break
			}
		}
	}
	if p.config != nil && len(p.config.Auth.Tokens) > 0 {
		return p.config.Auth.Tokens[0]
	}
	return ""
}

// ollamaChatMessage — message из NDJSON-ответа /api/chat.
//
// cppworker пишет NDJSON даже для stream=false (одна строка + done), но
// полагаться на «ровно одну строку» нельзя: берём ПЕРВУЮ строку с непустым
// message и, если встретили {"error":...}, отдаём её как есть (вызывающий увидит
// пустой message и просто проксирует ответ).
func ollamaChatMessage(raw []byte) (map[string]interface{}, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			continue
		}
		if msg, ok := doc["message"].(map[string]interface{}); ok && msg != nil {
			done, _ := doc["done"].(bool)
			return msg, done
		}
	}
	return nil, false
}

// buildOllamaFollowUpBody — тело turn 2 в формате Ollama.
//
// Отличия от OpenAI-варианта:
//   - message ассистента кладём КАК ЕСТЬ (у Ollama уже правильная форма:
//     role/content/tool_calls) — синтез не нужен, кроме случая «вызов пришёл
//     JSON-ом в content», где tool_calls надо собрать, иначе модель не увидит
//     своего вызова;
//   - tool-сообщение: {"role":"tool","content":"<json>"} — в Ollama-формате у
//     tool-сообщения нет tool_call_id;
//   - наши инструменты убираются из tools (иначе turn 2 может зациклиться на
//     каталоге), инструменты клиента остаются.
func buildOllamaFollowUpBody(body []byte, assistant map[string]interface{}, calls []imageToolCall, results []map[string]interface{}) ([]byte, error) {
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
	// ВАЖНО ПРО ФОРМУ АРГУМЕНТОВ (R86-follow-up, 2026-10-06). На OpenAI-пути
	// tool_calls[].function.arguments — СТРОКА с JSON. На Ollama-пути cppworker
	// ждёт ОБЪЕКТ: см. cmd/cppworker/handlers_chat.go:22-31 — там прямо записано,
	// что нативные Ollama-клиенты «шлют и ждут arguments как JSON-ОБЪЕКТ», а со
	// строкой второй шаг падал с 400 `cannot unmarshal object into ... Arguments of
	// type string`.
	//
	// Раньше здесь лежал ответ движка КАК ЕСТЬ (со строкой), и второй turn на
	// Ollama-поверхности получал историю в чужой форме: модель либо путалась, либо
	// отвечала текстом вместо финального ответа — ровно то, что отличало
	// /api/chat от /v1/chat/completions в живых проверках. Теперь ВСЕГДА собираем
	// каноничную Ollama-форму (arguments — объект), а исходную строку используем
	// только как запасной вариант, если собрать не удалось.
	synth := synthesizeOllamaToolCalls(calls)
	if len(synth) > 0 {
		assistantMsg["tool_calls"] = synth
	} else if rawCalls, ok := assistant["tool_calls"]; ok && rawCalls != nil {
		assistantMsg["tool_calls"] = rawCalls
	}
	messages = append(messages, assistantMsg)

	for i := range calls {
		payload := map[string]interface{}{"status": "error", "error": "результат не получен"}
		if i < len(results) && results[i] != nil {
			payload = results[i]
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			raw = []byte(`{"status":"error","error":"marshal tool result"}`)
		}
		messages = append(messages, map[string]interface{}{
			"role":    "tool",
			"content": string(raw),
		})
	}
	doc["messages"] = messages

	if tools, ok := doc["tools"].([]interface{}); ok {
		kept := make([]interface{}, 0, len(tools))
		for _, t := range tools {
			if isImageToolName(openAIToolName(t)) {
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) == 0 {
			delete(doc, "tools")
		} else {
			doc["tools"] = kept
		}
	}
	return json.Marshal(doc)
}

// synthesizeOllamaToolCalls — каноничная форма вызовов для истории диалога.
func synthesizeOllamaToolCalls(calls []imageToolCall) []interface{} {
	out := make([]interface{}, 0, len(calls))
	for _, c := range calls {
		name := c.Name
		if name == "" {
			name = "generate_image"
		}
		entry := map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": decodeArgumentsForHistory(c.Arguments),
			},
		}
		if c.ID != "" {
			entry["id"] = c.ID
		}
		out = append(out, entry)
	}
	return out
}

// decodeArgumentsForHistory — arguments в виде объекта, если он разбирается
// (модели в истории привычнее видеть объект, а не строку с JSON).
func decodeArgumentsForHistory(raw string) interface{} {
	if doc, err := decodeToolArguments(raw); err == nil {
		return doc
	}
	return raw
}

// writeOllamaChatResponse — отдать клиенту буферизованный ответ в его формате.
func (lr *LlamaCppRouter) writeOllamaChatResponse(w http.ResponseWriter, a imageToolLoopArgsOllama, raw []byte, status int, clientStream bool) {
	if !clientStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return
	}
	synthesizeOllamaChatNDJSON(w, raw, a.model, status)
}

// synthesizeOllamaChatNDJSON — превратить буферизованный ответ /api/chat в
// NDJSON-поток для клиента, просившего стрим.
//
// ЗАЧЕМ: turn 1 обязан быть нестриминговым (решение принимается по целому ответу),
// а клиент мог просить stream=true. Отдаём две строки: сообщение целиком и
// done:true — этого достаточно любому Ollama-клиенту (текст приходит одним
// куском), а терять `done` нельзя: Open WebUI ждёт завершения потока.
func synthesizeOllamaChatNDJSON(w http.ResponseWriter, raw []byte, model string, status int) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Ответ не разобрали — отдаём как одну строку, чтобы клиент не завис.
		_, _ = w.Write(ensureTrailingNewline(raw))
		Flush(w)
		return
	}
	if m, _ := doc["model"].(string); m != "" {
		model = m
	}
	if model != "" {
		doc["model"] = model
	}
	doneReason, _ := doc["done_reason"].(string)
	if doneReason == "" {
		doneReason = "stop"
	}

	msgLine := map[string]interface{}{}
	for k, v := range doc {
		if k == "done" || k == "done_reason" {
			continue
		}
		msgLine[k] = v
	}
	msgLine["done"] = false
	out, err := json.Marshal(msgLine)
	if err != nil {
		_, _ = w.Write(ensureTrailingNewline(raw))
		Flush(w)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", out)

	final := map[string]interface{}{
		"model":       model,
		"created_at":  doc["created_at"],
		"message":     map[string]interface{}{"role": "assistant", "content": ""},
		"done":        true,
		"done_reason": doneReason,
	}
	if eval, ok := doc["eval_count"]; ok {
		final["eval_count"] = eval
	}
	if dur, ok := doc["total_duration"]; ok {
		final["total_duration"] = dur
	}
	fin, err := json.Marshal(final)
	if err != nil {
		Flush(w)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", fin)
	Flush(w)
}

// ensureTrailingNewline — NDJSON-строка обязана заканчиваться переводом строки.
func ensureTrailingNewline(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		return raw
	}
	out := make([]byte, 0, len(raw)+1)
	out = append(out, raw...)
	return append(out, '\n')
}

// writeOllamaToolFallback — что отдать клиенту, если финальный ответ модели не
// получился: картинка УЖЕ сгенерирована, терять её нельзя.
func (lr *LlamaCppRouter) writeOllamaToolFallback(w http.ResponseWriter, a imageToolLoopArgsOllama, assistant map[string]interface{}, results []map[string]interface{}, clientStream bool) {
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
		} else if summary, _ := res["summary"].(string); summary != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(summary)
		} else if e, _ := res["error"].(string); e != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("Не удалось сгенерировать изображение: " + e)
		}
	}
	payload := map[string]interface{}{
		"model":       a.model,
		"created_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"message":     map[string]interface{}{"role": "assistant", "content": sb.String()},
		"done":        true,
		"done_reason": "stop",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"message":{"role":"assistant","content":""},"done":true}`)
	}
	lr.writeOllamaChatResponse(w, a, raw, http.StatusOK, clientStream)
}

// stripEmptyToolCallArtifact — убрать «пустой вызов» из NDJSON-ответа модели.
//
// ПОЧЕМУ ЭТО НУЖНО (живой случай 2026-10-06): у Qwen3 chat-шаблон при объявленных
// инструментах дописывает в ответ маркер вызова, и если модель НЕ вызвала
// инструмент, в content остаётся мусор от шаблона: «[]», «[TOOL_CALLS]»,
// «[TOOL_CALLS]=[]» (варианты зависят от сборки шаблона). Клиент видит эту строку
// вместо ответа. Настоящий вызов эти строки не содержат (его распознал бы
// extractImageToolCalls), поэтому чистим content только тогда, когда после
// удаления маркеров и скобок в нём не остаётся ничего осмысленного.
func stripEmptyToolCallArtifact(raw []byte) []byte {
	const marker = "[TOOL_CALLS]"
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
			continue
		}
		msg, _ := doc["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		content, _ := msg["content"].(string)
		if !isEmptyToolCallArtifact(content, marker) {
			continue
		}
		msg["content"] = ""
		doc["message"] = msg
		out, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		lines[i] = string(out)
		changed = true
	}
	if !changed {
		return raw
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// isEmptyToolCallArtifact — остаётся ли от content что-то, кроме маркера вызова,
// скобок, знаков «=»/«:» и пробелов.
func isEmptyToolCallArtifact(content, marker string) bool {
	s := strings.ReplaceAll(content, marker, "")
	s = strings.TrimSpace(s)
	// После снятия маркера допустимы только пустые скобки/разделители: "[]", "=[]",
	// "[]:", "[ ]" и т.п. Всё, что содержит буквы или цифры, — настоящий ответ.
	for _, r := range s {
		switch r {
		case '[', ']', '=', ':', ' ', '\t', '\n', '\r':
			continue
		default:
			return false
		}
	}
	return strings.ContainsAny(s, "[]")
}

// ensureImageMarkdownOllama — добавить ссылку на картинку в NDJSON-ответ /api/chat,
// если модель её не вставила (та же логика, что ensureImageMarkdown для OpenAI).
func ensureImageMarkdownOllama(raw []byte, results []map[string]interface{}) []byte {
	var missing []string
	for _, res := range results {
		md, _ := res["markdown"].(string)
		url, _ := res["url"].(string)
		if md == "" || url == "" {
			continue
		}
		if strings.Contains(string(raw), url) {
			continue
		}
		missing = append(missing, md)
	}
	if len(missing) == 0 {
		return raw
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Ищем ПОСЛЕДНЮЮ строку с непустым content: в NDJSON их может быть несколько,
	// а добавлять текст надо в тот же ответ, который увидит клиент.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			continue
		}
		msg, _ := doc["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		content, _ := msg["content"].(string)
		content = strings.TrimRight(content, " \t\n")
		if content != "" {
			content += "\n\n"
		}
		content += strings.Join(missing, "\n\n")
		msg["content"] = content
		doc["message"] = msg
		out, err := json.Marshal(doc)
		if err != nil {
			return raw
		}
		lines[i] = string(out)
		return []byte(strings.Join(lines, "\n") + "\n")
	}
	return raw
}
