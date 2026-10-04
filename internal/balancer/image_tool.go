// image_tool.go — R84 (2026-10-03): инструмент генерации изображений для
// ТЕКСТОВЫХ моделей (cppworker) — объявление и серверное исполнение.
//
// ЗАЧЕМ. Оператор описал сценарий: текстовая модель на cppworker должна уметь
// «нарисовать картинку» по просьбе пользователя/агента, если в кластере поднят
// imageworker. Технически части уже были: cppworker инжектит инструменты клиента
// в system prompt и парсит tool_calls (cmd/cppworker/handlers_openai.go,
// tools_stream_r83.go), балансер умеет детектировать tool_calls (в т.ч. в SSE) и
// маршрутизировать генерацию (image_router.go), а снимки image-бэкендов с
// загруженной моделью уже кэшируются (image_resources.go). Не было ровно двух
// вещей: инструмент никто не ОБЪЯВЛЯЛ модели и никто не ИСПОЛНЯЛ его вызов.
//
// ПОЧЕМУ ИСПОЛНЯЕТ БАЛАНСЕР, А НЕ КЛИЕНТ. Инструмент объявляет прокси, а не
// клиент: клиент (Open WebUI, LibreChat, Cline) про него не знает и исполнить не
// может. Поэтому вызов перехватывается здесь же: генерируем картинку сами,
// подкладываем результат в диалог как tool-сообщение и просим модель закончить
// ответ — пользователь видит текст + markdown-ссылку на изображение.
//
// ГЕЙТ «ЕСТЬ КОМУ ИСПОЛНЯТЬ» (требование оператора: передавать инструмент,
// только когда imageworker поднят):
//   - есть ЗДОРОВЫЙ image_cpp (filterBackendsByType отдаёт только healthy);
//   - его снимок достоверен (contractOK, без lastErr);
//   - в снимке есть ЗАГРУЖЕННАЯ модель (state=loaded) — иначе генерация
//     упёрлась бы в «no image model is loaded» уже после вызова инструмента.
//
// Не выполнено хотя бы одно — инструмент не добавляется вовсе, и в лог уходит
// причина (debug-уровень, чтобы не спамить на каждый чат).
//
// СОВМЕСТИМОСТЬ: без живого image-бэкенда поведение /v1/chat/completions не
// меняется ни на байт (инструмент не добавляется, поток не буферизуется).
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Флаги инструмента (соглашение проекта: LB_* читаются из окружения).
const (
	imageToolEnvEnabled  = "LB_IMAGE_TOOL"             // off | on (по умолчанию on)
	imageToolEnvMaxCalls = "LB_IMAGE_TOOL_MAX_CALLS"   // сколько картинок на один запрос
	imageToolEnvTimeout  = "LB_IMAGE_TOOL_TIMEOUT_SEC" // таймаут одной генерации
	imageToolEnvBaseURL  = "LB_IMAGE_TOOL_BASE_URL"    // внешний адрес балансера для ссылок

	imageToolDefaultMaxCalls   = 2
	imageToolDefaultTimeoutSec = 600
	imageToolMaxCallsLimit     = 8
)

// imageToolSurface — метка поверхности в ленте image-запросов (Monitor).
// Отдельная от openai/legacy: тул-генерации полезно отличать от прямых запросов
// клиентов — иначе в мониторе не понять, откуда взялась картинка.
const imageToolSurface = "chat-tool"

// imageToolPath — путь, которым тул-генерация видна в ленте запросов.
const imageToolPath = "/v1/chat/completions→generate_image"

// imageToolConfig — настройки инструмента.
type imageToolConfig struct {
	Enabled  bool
	MaxCalls int
	Timeout  time.Duration
	// BaseURL — внешний адрес балансера для ссылок на картинки. Пусто = берём
	// Host из запроса клиента (http://<Host>). Нужен, когда балансер стоит за
	// TLS-прокси: тогда схему и хост знает только оператор.
	BaseURL string
}

// imageToolSettings читает настройки из окружения.
func imageToolSettings() imageToolConfig {
	cfg := imageToolConfig{
		Enabled:  true,
		MaxCalls: imageToolDefaultMaxCalls,
		Timeout:  imageToolDefaultTimeoutSec * time.Second,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(imageToolEnvEnabled))) {
	case "0", "false", "no", "off":
		cfg.Enabled = false
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvMaxCalls)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > imageToolMaxCallsLimit {
				n = imageToolMaxCallsLimit
			}
			cfg.MaxCalls = n
		}
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvTimeout)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = time.Duration(n) * time.Second
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv(imageToolEnvBaseURL)), "/")
	return cfg
}

// imageToolTarget — бэкенд и модель, которыми балансер исполнит вызов.
type imageToolTarget struct {
	BackendID string
	Model     string
	// Models — все модели кластера (enum в схеме инструмента).
	Models []string
}

// imageToolTargetFor — гейт доступности: nil, если исполнять некому.
//
// ПОЧЕМУ ЧЕРЕЗ ensureFreshFor, А НЕ metricsSnapshot: решение принимается
// per-request, и протухший снимок стоит дорого в обе стороны — объявим
// инструмент по выгруженной модели и получим ошибку уже после вызова. Догрузка
// ленивая и переиспользует один опрос для подряд идущих запросов (staleAfter=5s).
func (p *Proxy) imageToolTargetFor(ctx context.Context) *imageToolTarget {
	if p == nil {
		return nil
	}
	cfg := imageToolSettings()
	if !cfg.Enabled {
		return nil
	}
	res := p.imageResources()
	if res == nil {
		return nil
	}
	states := p.filterBackendsByType(types.BackendTypeImage)
	if len(states) == 0 {
		logger.Get().Debugw("image tool: не объявляем — нет здоровых image_cpp-бэкендов")
		return nil
	}

	var models []string
	var target *imageToolTarget
	for _, st := range states {
		if st == nil || st.Backend == nil {
			continue
		}
		snap := res.ensureFreshFor(ctx, st.Backend.ID)
		if snap == nil || !snap.contractOK || snap.lastErr != "" {
			logger.Get().Debugw("image tool: снимок бэкенда недостоверен",
				"backend", st.Backend.ID)
			continue
		}
		for _, m := range snap.models {
			if m.Name != "" && !containsString(models, m.Name) {
				models = append(models, m.Name)
			}
		}
		if loaded := snap.loaded(); loaded != nil && target == nil {
			target = &imageToolTarget{BackendID: st.Backend.ID, Model: loaded.Name}
		}
	}
	if target == nil {
		// Требование оператора: инструмент отдаём ТОЛЬКО когда картинку реально
		// можно сделать. Модель может быть на диске, но не в VRAM — тогда
		// генерация сразу вернула бы «no image model is loaded».
		logger.Get().Debugw("image tool: не объявляем — ни на одном image-бэкенде нет загруженной модели")
		return nil
	}
	target.Models = models
	return target
}

// imageToolOpenAI — схема инструмента в форме OpenAI tools[].
func imageToolOpenAI(t *imageToolTarget) map[string]interface{} {
	spec := imagetool.Defaults()
	spec.Sync = true
	if t != nil {
		spec.Models = t.Models
	}
	return imagetool.Build(spec).OpenAIFunction()
}

// injectImageTool — добавить инструмент в тело запроса /v1/chat/completions.
//
// НЕ НАВЯЗЫВАЕМСЯ, ЕСЛИ:
//   - tool_choice="none" — клиент явно запретил инструменты в этом запросе;
//   - клиент САМ объявил generate_image — тогда исполняет его сторона, и
//     перехватывать вызов нельзя (иначе клиент не получит свой tool_call).
//
// Возвращает исходное тело, если ничего не добавлено (байт-в-байт: путь без
// image-бэкенда не должен меняться вообще).
func injectImageTool(body []byte, tool map[string]interface{}) ([]byte, bool, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false, fmt.Errorf("parse chat body: %w", err)
	}
	if tc, ok := doc["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		return body, false, nil
	}
	existing, _ := doc["tools"].([]interface{})
	for _, t := range existing {
		if openAIToolName(t) == imagetool.Name {
			return body, false, nil
		}
	}
	doc["tools"] = append(existing, tool)
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false, fmt.Errorf("marshal chat body with tool: %w", err)
	}
	return out, true, nil
}

// openAIToolName — имя функции в элементе tools[] (формат OpenAI).
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

// imageToolCall — разобранный вызов инструмента.
type imageToolCall struct {
	ID        string
	Arguments string
}

// extractImageToolCalls — вызовы generate_image в ответе модели.
//
// cppworker нормализует вывод в message.tool_calls (в т.ч. для Gemma/Hermes/
// Llama/Mistral форматов), но полагаться только на это нельзя: у части моделей
// вызов приходит JSON-ом в content. Поэтому проверяем оба места.
func extractImageToolCalls(message map[string]interface{}) []imageToolCall {
	var calls []imageToolCall
	if raw, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range raw {
			entry, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := entry["function"].(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			if name != imagetool.Name {
				continue
			}
			args := stringifyToolArguments(fn["arguments"])
			id, _ := entry["id"].(string)
			calls = append(calls, imageToolCall{ID: id, Arguments: args})
		}
	}
	if len(calls) == 0 {
		content, _ := message["content"].(string)
		if content != "" {
			if detected, _, found := detectAndExtractToolCallsFromContent(content); found {
				for _, item := range detected {
					entry, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					fn, ok := entry["function"].(map[string]interface{})
					if !ok {
						continue
					}
					if name, _ := fn["name"].(string); name != imagetool.Name {
						continue
					}
					id, _ := entry["id"].(string)
					calls = append(calls, imageToolCall{ID: id, Arguments: stringifyToolArguments(fn["arguments"])})
				}
			}
		}
	}
	return calls
}

// imageToolArgs — аргументы вызова в терминах движка.
type imageToolArgs struct {
	Prompt         string
	NegativePrompt string
	Width          int
	Height         int
	Steps          int
	CFG            float64
	Seed           *int64
	Model          string
}

// parseImageToolArgs разбирает JSON-аргументы вызова.
func parseImageToolArgs(raw string) (imageToolArgs, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return imageToolArgs{}, fmt.Errorf("arguments не JSON: %w", err)
	}
	args := imageToolArgs{
		Prompt:         stringField(doc, "prompt"),
		NegativePrompt: firstNonEmpty(stringField(doc, "negative_prompt"), stringField(doc, "negativePrompt")),
		Width:          intField(doc, "width"),
		Height:         intField(doc, "height"),
		Steps:          intField(doc, "steps"),
		CFG:            floatField(doc, "cfg"),
		Model:          stringField(doc, "model"),
	}
	if v, ok := doc["seed"]; ok {
		switch t := v.(type) {
		case float64:
			s := int64(t)
			args.Seed = &s
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				args.Seed = &n
			}
		}
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return args, errors.New("в аргументах нет prompt")
	}
	return args, nil
}

func stringField(doc map[string]interface{}, key string) string {
	v, _ := doc[key].(string)
	return strings.TrimSpace(v)
}

func intField(doc map[string]interface{}, key string) int {
	switch t := doc[key].(type) {
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return 0
}

func floatField(doc map[string]interface{}, key string) float64 {
	switch t := doc[key].(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// generateImageForTool — сгенерировать картинку на выбранном image-бэкенде.
//
// Идём НАТИВНЫМ путём воркера (/api/image/generate, sync=true,
// responseFormat=url): он возвращает готовую ссылку /images/<file>, а сам файл
// воркер раздаёт через FileServer. Картинку в base64 в диалог не тащим — она
// раздула бы и ответ, и контекст модели.
//
// Гейт VRAM и лента запросов — те же, что у обычной генерации
// (imageResources.beforeGeneration + imageRequestStore): тул-генерации обязаны
// быть видны в мониторе и обязаны уважать занятость GPU.
func (p *Proxy) generateImageForTool(ctx context.Context, target *imageToolTarget, args imageToolArgs) (map[string]interface{}, error) {
	cfg := imageToolSettings()
	model := firstNonEmpty(args.Model, target.Model)
	payload := map[string]interface{}{
		"prompt":         args.Prompt,
		"sync":           true,
		"responseFormat": "url",
	}
	if model != "" {
		payload["model"] = model
	}
	if args.NegativePrompt != "" {
		payload["negativePrompt"] = args.NegativePrompt
	}
	if args.Width > 0 {
		payload["width"] = args.Width
	}
	if args.Height > 0 {
		payload["height"] = args.Height
	}
	if args.Steps > 0 {
		payload["steps"] = args.Steps
	}
	if args.CFG > 0 {
		payload["cfg"] = args.CFG
	}
	if args.Seed != nil {
		payload["seed"] = *args.Seed
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/image/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res := p.imageResources()
	gate := res.beforeGeneration(ctx, target.BackendID)
	if !gate.Allowed {
		return nil, fmt.Errorf("генерация недоступна: %s", gate.Message)
	}
	defer gate.Release()

	brief := types.ImageRequestBrief{
		BackendID: target.BackendID,
		Surface:   imageToolSurface,
		Path:      imageToolPath,
		Model:     model,
		Prompt:    args.Prompt,
		Width:     args.Width,
		Height:    args.Height,
		Steps:     args.Steps,
	}
	handle := res.imageRequests().begin(target.BackendID, brief)

	started := time.Now()
	resp, err := p.proxyRequestToBackend(req, target.BackendID, cfg.Timeout)
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, http.StatusBadGateway, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("image backend request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageToolResponseLimitBytes))
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("read image backend response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := upstreamErrorMessage(raw, resp.StatusCode)
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", msg, 0)
		return nil, fmt.Errorf("движок отказал (HTTP %d): %s", resp.StatusCode, msg)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_bad_response", err.Error(), 0)
		return nil, fmt.Errorf("image backend returned invalid JSON: %w", err)
	}
	url := firstImageURL(doc)
	if url == "" {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_no_image", "no image in response", 0)
		return nil, errors.New("движок не вернул изображение")
	}

	handle.finish(types.ImageRequestStatusOK, resp.StatusCode, "", "", 1)
	return map[string]interface{}{
		"status":     "ok",
		"url":        url,
		"markdown":   "![" + shortPrompt(args.Prompt) + "](" + url + ")",
		"model":      model,
		"seed":       doc["seed"],
		"width":      doc["width"],
		"height":     doc["height"],
		"steps":      doc["steps"],
		"seconds":    secondsOf(doc["duration_ms"], time.Since(started)),
		"output":     doc["output_format"],
		"generated":  true,
		"promptUsed": args.Prompt,
	}, nil
}

// imageToolResponseLimitBytes — предел чтения ответа воркера: в url-режиме тело
// маленькое (метаданные + ссылка), но лимит нужен, чтобы битый/чужой сервис на
// порту не вылил в память гигабайты.
const imageToolResponseLimitBytes = 1 << 20

// firstImageURL — ссылка на первую картинку в ответе генерации.
func firstImageURL(doc map[string]interface{}) string {
	data, ok := doc["data"].([]interface{})
	if !ok || len(data) == 0 {
		return ""
	}
	item, ok := data[0].(map[string]interface{})
	if !ok {
		return ""
	}
	url, _ := item["url"].(string)
	return strings.TrimSpace(url)
}

// upstreamErrorMessage — короткий текст ошибки из ответа воркера.
func upstreamErrorMessage(raw []byte, status int) string {
	var doc map[string]interface{}
	if json.Unmarshal(raw, &doc) == nil {
		if s, _ := doc["error"].(string); s != "" {
			return s
		}
		if e, ok := doc["error"].(map[string]interface{}); ok {
			if s, _ := e["message"].(string); s != "" {
				return s
			}
		}
		if s, _ := doc["message"].(string); s != "" {
			return s
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return text
}

// secondsOf — длительность в секундах: из duration_ms воркера, иначе из замера.
func secondsOf(v interface{}, fallback time.Duration) float64 {
	if ms, ok := v.(float64); ok && ms > 0 {
		return ms / 1000
	}
	return fallback.Seconds()
}

// shortPrompt — подпись для alt-текста markdown (без переводов строк).
func shortPrompt(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	if s == "" {
		return "generated image"
	}
	return s
}

// imageToolPublicURL — ссылка на картинку, которую отдадим модели и клиенту.
//
// Почему публичный маршрут балансера, а не адрес воркера: воркер живёт на
// внутреннем порту и картинку наружу не отдаёт; маршрут /v1/images/files/{name}
// проксирует её через балансер и, как остальные клиентские поверхности, не
// требует токена (угадать имя файла нельзя — оно случайное).
func imageToolPublicURL(r *http.Request, url string) string {
	name := strings.TrimPrefix(strings.TrimSpace(url), "/images/")
	if name == "" {
		return url
	}
	base := imageToolSettings().BaseURL
	if base == "" {
		host := ""
		if r != nil {
			host = r.Host
		}
		if host == "" {
			host = "localhost"
		}
		base = "http://" + host
	}
	return base + "/v1/images/files/" + name
}
