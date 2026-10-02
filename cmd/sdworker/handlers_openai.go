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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// maxEditsRequestBytes — лимит тела multipart-запроса /v1/images/edits.
//
// ПОЧЕМУ ЛИМИТ ОБЯЗАТЕЛЕН: это единственный эндпоинт, куда клиент шлёт
// БИНАРНЫЕ картинки (base64 в теле в 1.33 раза больше исходника). Без
// MaxBytesReader один злой/багованный клиент (цикл загрузки) съедает память
// воркера вместе с загруженной моделью. 32 MB с запасом хватает на пару
// изображений 4096x4096 (PNG ~8-12 MB каждое) и маску.
const maxEditsRequestBytes = 32 << 20

// editsFormMemoryBytes — сколько multipart держим в памяти до выгрузки в
// temp-файлы (stdlib сам решает; нам важно не удвоить лимит тела в heap).
const editsFormMemoryBytes = 8 << 20

// openAIImageRequest — тело POST /v1/images/generations.
//
// Поля, которых нет в структуре (quality, style, user, background, moderation),
// сознательно НЕ объявлены: encoding/json их молча игнорирует — это ровно то
// поведение, которое нужно (§12.4 п.5).
type openAIImageRequest struct {
	Prompt            string `json:"prompt"`
	Model             string `json:"model"`
	N                 int    `json:"n"`
	Size              string `json:"size"`
	ResponseFormat    string `json:"response_format"`
	OutputFormat      string `json:"output_format"`
	OutputCompression *int   `json:"output_compression"`
	Seed              *int64 `json:"seed"`
	Steps             int    `json:"steps"`
	NegativePrompt    string `json:"negative_prompt"`
	// Часть клиентов (и наш балансер в Phase 4) шлют эти поля напрямую,
	// хотя формально они не в спецификации OpenAI Images.
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	CFGScale  float64 `json:"cfg_scale"`
	Sampler   string  `json:"sampler"`
	Scheduler string  `json:"scheduler"`
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

// handleOpenAIImagesEdits — POST /v1/images/edits (img2img / inpaint, multipart).
//
// КОНТРАКТ (OpenAI Images Edits + то, что реально шлют клиенты):
//
//	prompt              — обязателен (пустой → 400);
//	image[]             — предпочтительное поле, НЕСКОЛЬКО файлов (OpenAI так
//	                      и определяет edits: несколько входных картинок);
//	image               — legacy-поле с одним файлом (старые SDK/обёртки);
//	mask                — опционально (PNG с альфой; inpaint);
//	n                   — число картинок (движок: batch_count, clamp 1..8);
//	size                — "WxH"; если нет — ГЕОМЕТРИЯ ПЕРВОГО изображения;
//	output_format       — png|jpeg|webp; output_compression — 0..100;
//	strength (наш доп.) — [0,1], сила денойза.
//
// У движка init_image ОДИН, поэтому из image[] берём ПЕРВОЕ изображение, а
// остальные отмечаем в notes (клиент видит, что именно мы сделали, вместо
// молчаливой потери картинок).
//
// SEED: в OpenAI-запросе поля seed нет вовсе, поэтому он резолвится на нашей
// стороне ровно как для /v1/images/generations (ловушка №1: без этого движок
// возьмёт default_gen_params.seed = 42 и все картинки будут одинаковыми), и
// инжектится в prompt через <sd_cpp_extra_args> — тот же путь, что у
// generations (injectSeedInPrompt=true).
func (a *App) handleOpenAIImagesEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "multipart/form-data") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_content_type",
			"POST /v1/images/edits expects multipart/form-data (fields: prompt, image[], mask, n, size, output_format)")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEditsRequestBytes)
	if err := r.ParseMultipartForm(editsFormMemoryBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the "+strconv.Itoa(maxEditsRequestBytes/(1<<20))+" MB limit "+
					"(send a smaller init image, e.g. 1024x1024 PNG)")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_multipart", "cannot parse multipart form: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	if strings.TrimSpace(r.FormValue("prompt")) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "prompt_required", "prompt is required")
		return
	}

	images, err := multipartImagePayloads(r, "image[]", "image")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_image", "cannot read image: "+err.Error())
		return
	}
	if len(images) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "image_required",
			"an input image is required: send image[] (preferred) or image as a file part (PNG/JPEG/WebP)")
		return
	}
	mask := ""
	if m, err := multipartImagePayload(r, "mask"); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_mask", "cannot read mask: "+err.Error())
		return
	} else {
		mask = m
	}

	// size: если клиент его не прислал — берём геометрию первого изображения
	// (иначе img2img молча сгенерировал бы 512x512 из дефолта профиля).
	w0, h0 := 0, 0
	if size := strings.TrimSpace(r.FormValue("size")); size != "" {
		sw, sh, serr := sdbackend.ParseSize(size)
		if serr != nil {
			if fw, fh, ok := parseSizeFallback(size); ok {
				sw, sh = fw, fh
			} else {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_size", serr.Error())
				return
			}
		}
		w0, h0 = sw, sh
	}
	if w0 <= 0 || h0 <= 0 {
		if iw, ih, ierr := sdbackend.ImageSizeFromBase64(images[0]); ierr == nil {
			w0, h0 = iw, ih
		}
	}

	var notes []string
	if len(images) > 1 {
		notes = append(notes, "получено изображений: "+strconv.Itoa(len(images))+
			"; sd.cpp принимает одно init_image — использовано первое")
	}
	if mask != "" {
		notes = append(notes, "mask передан как mask_image (inpaint)")
	}

	strength, err := formStrength(r.FormValue("strength"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_strength", err.Error())
		return
	}

	provided, seedVal := formSeed(r.FormValue("seed"))

	model := a.resolveModel(r.FormValue("model"))
	profile := a.svc.Runner.ProfileFor(model)

	norm, err := sdbackend.NormalizeGeneration(sdbackend.GenerationRequest{
		Prompt:            r.FormValue("prompt"),
		NegativePrompt:    r.FormValue("negative_prompt"),
		Width:             w0,
		Height:            h0,
		Steps:             formInt(r.FormValue("steps")),
		CFGScale:          formFloat(r.FormValue("cfg_scale")),
		Seed:              seedVal,
		SeedProvided:      provided,
		Sampler:           r.FormValue("sampler"),
		Scheduler:         r.FormValue("scheduler"),
		BatchCount:        formInt(r.FormValue("n")),
		OutputFormat:      r.FormValue("output_format"),
		OutputCompression: formIntPtr(r.FormValue("output_compression")),
		InitImage:         images[0],
		MaskImage:         mask,
		Strength:          strength,
	}, profile, a.limitsFor())
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_generation_params", err.Error())
		return
	}
	norm.Notes = append(norm.Notes, notes...)

	// injectSeedInPrompt=true: OpenAI-поверхность движка seed не читает —
	// путь ровно тот же, что у /v1/images/generations (ловушка №1).
	res, err := a.svc.Runner.GenerateSync(r.Context(), model, false, true, norm)
	if err != nil {
		writeEditsError(w, true, err)
		return
	}

	data := make([]openAIImageData, 0, len(res.Images))
	for _, img := range res.Images {
		data = append(data, openAIImageData{B64JSON: img.B64JSON, URL: img.URL})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created":       time.Now().Unix(),
		"output_format": res.OutputFormat,
		"data":          data,
		"seed":          res.Seed,
		"model":         model,
		"notes":         norm.Notes,
	})
}

// multipartImagePayloads — base64-строки картинок из multipart-полей (файлы,
// а при их отсутствии — значения форм).
//
// ПОЧЕМУ ОБА ВАРИАНТА: OpenAI SDK и curl-примеры шлют файлы, но часть
// обёрток (и наши же тесты/скрипты) кладут data-URL прямо в form-value.
// Отклонять второе — терять клиентов на ровном месте.
func multipartImagePayloads(r *http.Request, names ...string) ([]string, error) {
	var out []string
	for _, name := range names {
		files := multipartFiles(r, name)
		for _, fh := range files {
			f, err := fh.Open()
			if err != nil {
				return nil, err
			}
			raw, err := io.ReadAll(io.LimitReader(f, maxEditsRequestBytes+1))
			_ = f.Close()
			if err != nil {
				return nil, err
			}
			if len(raw) == 0 {
				return nil, errors.New("empty file part " + name)
			}
			out = append(out, base64.StdEncoding.EncodeToString(raw))
		}
	}
	if len(out) == 0 {
		for _, name := range names {
			if v := strings.TrimSpace(r.FormValue(name)); v != "" {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// multipartImagePayload — один payload (первый) из перечисленных полей.
func multipartImagePayload(r *http.Request, names ...string) (string, error) {
	vals, err := multipartImagePayloads(r, names...)
	if err != nil {
		return "", err
	}
	if len(vals) == 0 {
		return "", nil
	}
	return vals[0], nil
}

// multipartFiles — файлы поля name (nil-safe: без ParseMultipartForm паника).
func multipartFiles(r *http.Request, name string) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	return r.MultipartForm.File[name]
}

// formInt — int из form-value (пусто/мусор → 0: «не задано»).
func formInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// formFloat — float из form-value (пусто/мусор → 0: «не задано»).
func formFloat(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return f
}

// formIntPtr — *int из form-value (пусто → nil = не передавать движку).
func formIntPtr(v string) *int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// formStrength — strength из form-value (пусто → nil = дефолт движка).
//
// В отличие от A1111-поля denoising_strength здесь НЕ зажимаем, а отклоняем:
// strength в OpenAI-edits — наше расширение (клиенты OpenAI его не шлют), а
// движок значение вне [0,1] отвергает — честная 400 понятнее тихой подмены.
func formStrength(v string) (*float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, errors.New("strength is not a number: " + v)
	}
	if err := sdbackend.ValidateStrength(f); err != nil {
		return nil, err
	}
	return &f, nil
}

// formSeed — seed из form-value (наше расширение; в OpenAI-схеме его нет).
func formSeed(v string) (bool, int64) {
	v = strings.TrimSpace(v)
	if v == "" {
		return false, 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, 0
	}
	return true, n
}

// writeEditsError — ошибка img2img/inpaint: OOM на encode получает 503 + hint.
//
// ПОЧЕМУ ОТДЕЛЬНО ОТ writeOpenAIError: у движка НЕТ авто-retry при OOM на
// кодировании стартового изображения (в отличие от decode VAE), поэтому
// клиент/оператор обязан увидеть не «generation failed», а конкретное «похоже
// на нехватку памяти» + что включить (--vae-tiling/--vae-conv-direct/размер).
func writeEditsError(w http.ResponseWriter, openAI bool, err error) {
	status, code := statusForError(err)
	hint := ""
	if sdbackend.LooksLikeEncodeOOM(err.Error()) {
		status, code = http.StatusServiceUnavailable, "engine_out_of_memory"
		hint = sdbackend.Img2ImgMemoryHint
	}
	if status == http.StatusTooManyRequests {
		setRetryAfter(w, 5)
	}
	if openAI {
		writeOpenAIErrorHint(w, status, code, err.Error(), hint)
		return
	}
	body := map[string]any{"error": err.Error(), "code": code}
	if hint != "" {
		body["hint"] = hint
	}
	writeJSON(w, status, body)
}

// writeOpenAIErrorHint — OpenAI-конверт ошибки + hint внутри error.
//
// hint ВНУТРИ error (как у гейта балансера): SDK читают message/type/code и
// лишний ключ игнорируют, а оператор/curl подсказку видят.
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, message, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, message)
		return
	}
	kind := "invalid_request_error"
	switch {
	case status >= 500:
		kind = "server_error"
	case status == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    code,
			"hint":    hint,
		},
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
