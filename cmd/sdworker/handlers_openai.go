// handlers_openai.go — OpenAI-совместимая поверхность (/v1/images/*, /v1/models).
//
// ЗАЧЕМ: 18079 (OpenAI-поверхность балансера) маршрутизирует /v1/images/* в
// image-бэкенд. Клиенты (Open WebUI, LobeChat, Cherry Studio, AnythingLLM,
// n8n, OpenAI SDK) ждут конверт {created, data:[{b64_json|url}]} и НЕ понимают
// native-ответ sd-server.
//
// Ключевая ловушка (§12.3 п.1): OpenAI-ветка самого sd-server поле `seed` НЕ
// читает и берёт default_gen_params.seed = 42 — без подстановки все картинки
// одинаковые. Мы резолвим seed у себя и передаём его через
// <sd_cpp_extra_args>{"seed":N}</sd_cpp_extra_args>.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// openAIImageRequest — тело POST /v1/images/generations.
//
// Поля, которых нет в структуре (quality, style, user, background, moderation),
// сознательно НЕ объявлены: encoding/json их молча игнорирует — это ровно то
// поведение, которое нужно (§12.4 п.5).
type openAIImageRequest struct {
	Prompt           string `json:"prompt"`
	Model            string `json:"model"`
	N                int    `json:"n"`
	Size             string `json:"size"`
	ResponseFormat   string `json:"response_format"`
	OutputFormat     string `json:"output_format"`
	OutputCompression *int  `json:"output_compression"`
	Seed             *int64 `json:"seed"`
	Steps            int    `json:"steps"`
	NegativePrompt   string `json:"negative_prompt"`
	// Часть клиентов (и наш балансер в Phase 4) шлют эти поля напрямую,
	// хотя формально они не в спецификации OpenAI Images.
	Width  int     `json:"width"`
	Height int     `json:"height"`
	CFGScale float64 `json:"cfg_scale"`
	Sampler  string  `json:"sampler"`
	Scheduler string `json:"scheduler"`
}

// openAIImageData — один элемент data[].
type openAIImageData struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
	// RevisedPrompt — поле спецификации; sd-server его не даёт, но клиенты
	// (Open WebUI) читают его без проверки на nil, поэтому пустая строка лучше
	// отсутствующего ключа.
	RevisedPrompt string `json:"revised_prompt"`
}

// handleOpenAIImagesGenerations — POST /v1/images/generations (синхронно).
func (a *App) handleOpenAIImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var req openAIImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json",
			"invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "prompt_required", "prompt is required")
		return
	}

	// size: "auto"/""/WxH. Ошибка формата — 400 с понятным текстом (а не
	// молчаливая подмена размера, из-за которой клиент не понимает, почему
	// картинка не та).
	w0, h0, err := sdbackend.ParseSize(req.Size)
	if err != nil {
		// Часть клиентов шлют "1024x1024 " с пробелами или "512×512" (юникод ×).
		if w1, h1, ok := parseSizeFallback(req.Size); ok {
			w0, h0 = w1, h1
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_size", err.Error())
			return
		}
	}
	// Явные width/height имеют приоритет над size (наш расширенный контракт).
	if req.Width > 0 {
		w0 = req.Width
	}
	if req.Height > 0 {
		h0 = req.Height
	}

	model := a.resolveModel(req.Model)
	profile := a.svc.Runner.ProfileFor(model)

	provided := req.Seed != nil
	var seedVal int64
	if provided {
		seedVal = *req.Seed
	}
	norm, err := sdbackend.NormalizeGeneration(sdbackend.GenerationRequest{
		Prompt:            req.Prompt,
		NegativePrompt:    req.NegativePrompt,
		Width:             w0,
		Height:            h0,
		Steps:             req.Steps,
		CFGScale:          req.CFGScale,
		Seed:              seedVal,
		SeedProvided:      provided,
		Sampler:           req.Sampler,
		Scheduler:         req.Scheduler,
		BatchCount:        req.N,
		OutputFormat:      req.OutputFormat,
		OutputCompression: req.OutputCompression,
	}, profile, a.limitsFor())
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_generation_params", err.Error())
		return
	}

	urlMode := strings.EqualFold(strings.TrimSpace(req.ResponseFormat), "url")
	// injectSeedInPrompt=true: OpenAI-ветка движка seed не читает (ловушка №1).
	res, err := a.svc.Runner.GenerateSync(r.Context(), model, urlMode, true, norm)
	if err != nil {
		status, code := statusForError(err)
		if status == http.StatusTooManyRequests {
			setRetryAfter(w, 5)
		}
		writeOpenAIError(w, status, code, err.Error())
		return
	}

	data := make([]openAIImageData, 0, len(res.Images))
	for _, img := range res.Images {
		d := openAIImageData{B64JSON: img.B64JSON, URL: img.URL}
		data = append(data, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created":       time.Now().Unix(),
		"output_format": res.OutputFormat,
		"data":          data,
		// Не по спецификации, но полезно и безвредно: клиент видит, что мы
		// поправили (steps/n/batch/size) и какой seed реально использован.
		"seed":  res.Seed,
		"model": model,
	})
}

// handleOpenAIModels — GET /v1/models.
//
// Отдаём алиасы, которые ожидают клиенты: sd-cpp-local (так модель зовёт сам
// sd-server), dall-e-2/dall-e-3 (legacy-нода n8n фильтрует по префиксу dall-),
// плюс имена наших bundle'ов. gpt-image-* НЕ отдаём: клиенты по этому префиксу
// переключаются в режим «ответ ссылкой», а мы отдаём b64.
func (a *App) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	ids := []string{"sd-cpp-local", "dall-e-2", "dall-e-3"}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, name := range a.svc.Registry.Names() {
		if !seen[name] {
			ids = append(ids, name)
			seen[name] = true
		}
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  a.startedAt.Unix(),
			"owned_by": "local",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleNotImplemented — внятная 501 вместо пустого 404 от mux.
func (a *App) handleNotImplemented(feature string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		msg := feature + " не поддерживается этим воркером (Phase 3 поддерживает только txt2img/generations)"
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", msg)
			return
		}
		writeJSONError(w, http.StatusNotImplemented, "not_implemented", msg)
	}
}

// resolveModel — имя модели для запроса.
//
// OpenAI-клиенты присылают dall-e-2/dall-e-3/sd-cpp-local (мы сами их
// рекламируем в /v1/models) — в реестре таких нет. Если загружена модель или в
// реестре ровно одна — используем её; иначе пустая строка и понятная ошибка
// на уровне раннера.
func (a *App) resolveModel(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if _, ok := a.svc.Registry.Profile(requested); ok {
			return requested
		}
	}
	if cur := a.svc.Sup.CurrentModel(); cur != "" {
		return cur
	}
	names := a.svc.Registry.Names()
	if len(names) == 1 {
		return names[0]
	}
	if len(names) > 0 {
		return names[0]
	}
	return requested
}

// limitsFor — актуальные лимиты движка (nil = константы воркера).
func (a *App) limitsFor() *sdbackend.CapLimits {
	caps := a.svc.Sup.Capabilities()
	if caps == nil {
		return nil
	}
	return &caps.Limits
}

// parseSizeFallback — «512×512» (юникод), «512 x 512» и т.п.
func parseSizeFallback(size string) (int, int, bool) {
	s := strings.ToLower(strings.TrimSpace(size))
	s = strings.ReplaceAll(s, "×", "x")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" || s == "auto" {
		return 0, 0, false
	}
	w, h, err := sdbackend.ParseSize(s)
	if err != nil {
		return 0, 0, false
	}
	return w, h, true
}
