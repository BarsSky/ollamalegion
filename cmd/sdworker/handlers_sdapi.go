// handlers_sdapi.go — A1111 (Stable Diffusion WebUI) совместимый API.
//
// ЗАЧЕМ ЭТО ОБЯЗАТЕЛЬНО (§12.1): у SillyTavern есть РОДНОЙ источник
// «stable-diffusion.cpp server» (sd_sdcpp_url), а Open WebUI и LibreChat ходят
// через A1111-путь. Без /sdapi/v1/* отваливается половина клиентов.
//
// ВАЖНО ПРО SEED: в A1111-контракте seed — обычное поле, и -1 значит
// «случайный» (движок это соблюдает). Поэтому здесь seed НЕ инжектится в prompt
// через <sd_cpp_extra_args> — только резолвится в положительное число (у движка
// подтверждён integer overflow при seed = -1, см. §6.1.2 исследования).
package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/sdbackend"
)

// sdapiTxt2ImgRequest — поддерживаемые поля POST /sdapi/v1/txt2img.
//
// Неизвестные поля (enable_hr, hr_upscaler, denoising_strength и т.п.) молча
// игнорируются: движок их частично поддерживает, но Phase 3 не обещает hires —
// а 400 на незнакомое поле сломало бы клиентов, которые шлют полный набор
// A1111-параметров всегда.
type sdapiTxt2ImgRequest struct {
	Prompt         string   `json:"prompt"`
	NegativePrompt string   `json:"negative_prompt"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	Steps          int      `json:"steps"`
	CFGScale       float64  `json:"cfg_scale"`
	Seed           *int64   `json:"seed"`
	BatchSize      int      `json:"batch_size"`
	BatchCount     int      `json:"n_iter"`
	ClipSkip       int      `json:"clip_skip"`
	SamplerName    string   `json:"sampler_name"`
	Scheduler      string   `json:"scheduler"`
	Model          string   `json:"override_settings_sd_model_checkpoint"`
	Lora           []sdapiLora `json:"lora"`
	OutputFormat   string   `json:"output_format"`
}

// sdapiLora — структурированная LoRA A1111.
//
// ПОЧЕМУ СТРУКТУРА, А НЕ <lora:...> В ПРОМПТЕ: sd.cpp намеренно НЕ парсит
// prompt-теги ни в одном из трёх API (examples/server/api.md, «Global LoRA rule»).
type sdapiLora struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	Multiplier float64 `json:"multiplier"`
	Weight     float64 `json:"weight"`
	IsHighNoise bool   `json:"is_high_noise"`
}

// handleSDAPITxt2Img — POST /sdapi/v1/txt2img (синхронно).
func (a *App) handleSDAPITxt2Img(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var req sdapiTxt2ImgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt_required", "prompt is required")
		return
	}

	provided := req.Seed != nil
	var seedVal int64
	if provided {
		seedVal = *req.Seed
	}
	// batch_size * n_iter: A1111 позволяет комбинацию; движок принимает одно
	// число (batch_count), поэтому перемножаем и зажимаем в 1..8.
	batch := req.BatchSize
	if batch <= 0 {
		batch = 1
	}
	if req.BatchCount > 1 {
		batch *= req.BatchCount
	}

	model := a.resolveModel(req.Model)
	profile := a.svc.Runner.ProfileFor(model)

	norm, err := sdbackend.NormalizeGeneration(sdbackend.GenerationRequest{
		Prompt:         req.Prompt,
		NegativePrompt: req.NegativePrompt,
		Width:          req.Width,
		Height:         req.Height,
		Steps:          req.Steps,
		CFGScale:       req.CFGScale,
		Seed:           seedVal,
		SeedProvided:   provided,
		Sampler:        req.SamplerName,
		Scheduler:      req.Scheduler,
		BatchCount:     batch,
		ClipSkip:       req.ClipSkip,
		OutputFormat:   req.OutputFormat,
		Lora:           convertLora(req.Lora),
	}, profile, a.limitsFor())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_generation_params", err.Error())
		return
	}

	// injectSeedInPrompt=false: A1111-путь передаёт seed обычным полем и
	// движок его читает (-1 уже не используется — мы подставили положительный).
	res, err := a.svc.Runner.GenerateSync(r.Context(), model, false, false, norm)
	if err != nil {
		status, code := statusForError(err)
		if status == http.StatusTooManyRequests {
			setRetryAfter(w, 5)
		}
		writeJSONError(w, status, code, err.Error())
		return
	}

	images := make([]string, 0, len(res.Images))
	for _, img := range res.Images {
		images = append(images, img.B64JSON)
	}

	params := map[string]any{
		"prompt":          norm.Prompt,
		"negative_prompt": norm.NegativePrompt,
		"width":           norm.Width,
		"height":          norm.Height,
		"steps":           norm.Steps,
		"cfg_scale":       norm.CFGScale,
		"seed":            norm.Seed,
		"batch_size":      norm.BatchCount,
		"sampler_name":    norm.Sampler,
		"scheduler":       norm.Scheduler,
		"clip_skip":       norm.ClipSkip,
	}
	// info — JSON-СТРОКА (так у A1111; LibreChat/SillyTavern её парсят).
	info := map[string]any{
		"seed":           norm.Seed,
		"all_seeds":      []int64{norm.Seed},
		"width":          norm.Width,
		"height":         norm.Height,
		"steps":          norm.Steps,
		"cfg_scale":      norm.CFGScale,
		"sampler_name":   norm.Sampler,
		"scheduler":      norm.Scheduler,
		"sd_model_name":  model,
		"model":          model,
		"output_format":  res.OutputFormat,
		"infotexts":      []string{},
		"worker_notes":   norm.Notes,
		"duration_ms":    res.Duration.Milliseconds(),
	}
	infoJSON, _ := json.Marshal(info)

	writeJSON(w, http.StatusOK, map[string]any{
		"images":     images,
		"parameters": params,
		"info":       string(infoJSON),
	})
}

// convertLora — A1111 LoRA → движковые LoraRef.
func convertLora(in []sdapiLora) []sdbackend.LoraRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]sdbackend.LoraRef, 0, len(in))
	for _, l := range in {
		mult := l.Multiplier
		if mult == 0 {
			mult = l.Weight
		}
		if mult == 0 {
			mult = 1.0
		}
		path := l.Path
		if path == "" {
			path = l.Name
		}
		if path == "" {
			continue
		}
		out = append(out, sdbackend.LoraRef{Path: path, Multiplier: mult, IsHighNoise: l.IsHighNoise})
	}
	return out
}

// ============================================================
// Заглушки A1111 (§12.4 п.6)
// ============================================================
//
// ПОЧЕМУ ЗАГЛУШКИ, А НЕ РЕАЛЬНЫЕ ОПЕРАЦИИ: у sd-server нет ни смены модели
// (POST /sdapi/v1/options не зарегистрирован — модель одна на процесс и
// меняется только рестартом), ни прерывания генерации. Клиенты
// (SillyTavern, Open WebUI) вызывают эти эндпоинты при подключении и при
// смене модели, и падение с 404/500 ломает им UI. Возвращаем «пусто/ок».

// handleSDAPIOptions — POST /sdapi/v1/options: принимаем sd_model_checkpoint.
//
// ВАЖНО: НЕ добавляем forge_preset в GET-ответ — SillyTavern по его наличию
// переключается на Forge-ветку и начинает дёргать несуществующие ручки
// (§12.3 ловушка №11).
func (a *App) handleSDAPIOptions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		model := a.svc.Sup.CurrentModel()
		if model == "" {
			model = a.resolveModel("")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"samples_format":     "png",
			"sd_model_checkpoint": model,
		})
	case http.MethodPost:
		// Смена чекпойнта невозможна без рестарта процесса. Отвечаем 200
		// (клиент не должен падать), а фактическую смену выполняет
		// POST /api/image/models/load со своим именем модели.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requested, _ := body["sd_model_checkpoint"].(string)
		current := a.svc.Sup.CurrentModel()
		if requested != "" && requested != current {
			appLog().Warnw("sdapi/options: модель не переключена (hot-swap в sd-server отсутствует)",
				"requested", requested, "current", current,
				"hint", "используйте POST /api/image/models/load для смены модели (kill+spawn)")
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "detail": "options accepted; model switch requires process restart (POST /api/image/models/load)"})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or POST")
	}
}

// handleSDAPIProgress — GET /sdapi/v1/progress.
//
// Прогресс шагов sd-server НЕ отдаёт (только статус джобы), поэтому progress
// всегда 0 при нулевом job_count. Если у нас есть активная генерация —
// сообщаем её наличие, чтобы UI мог показать «идёт работа».
func (a *App) handleSDAPIProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	jobs := 0
	for _, j := range a.svc.Runner.List() {
		if j.State == sdbackend.JobStateQueued || j.State == sdbackend.JobStateProcessing {
			jobs++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"progress": 0.0,
		"state": map[string]any{
			"job_count":      jobs,
			"job_no":         0,
			"sampling_step":  0,
			"sampling_steps": 0,
			"skipped":        false,
			"interrupted":    false,
		},
		"current_image": nil,
		"textinfo":      nil,
	})
}

// handleSDAPIInterrupt — POST /sdapi/v1/interrupt → 204.
//
// ЧЕСТНО: mid-flight cancel у sd-server НЕ поддержан
// (features_by_mode.img_gen.cancel_generating == false). Отменяем то, что
// успеваем: джобы в состоянии queued.
func (a *App) handleSDAPIInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	cancelled := 0
	for _, j := range a.svc.Runner.List() {
		if j.State != sdbackend.JobStateQueued {
			continue
		}
		if err := a.svc.Runner.Cancel(j.ID); err == nil {
			cancelled++
		}
	}
	appLog().Infow("sdapi/interrupt: queued jobs cancelled",
		"cancelled", cancelled,
		"note", "генерация в полёте прервана быть не может (sd-server: cancel_generating=false)")
	w.WriteHeader(http.StatusNoContent)
}

// handleSDAPISdVae — GET /sdapi/v1/sd-vae → [].
func (a *App) handleSDAPISdVae(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// handleSDAPISdModules — GET /sdapi/v1/sd-modules → [].
func (a *App) handleSDAPISdModules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// handleSDAPISdModels — GET /sdapi/v1/sd-models.
//
// Движок отдаёт одну запись с placeholder-хешами; мы отдаём то же, но с нашими
// именами моделей — чтобы SillyTavern/Open WebUI видели осмысленный список.
func (a *App) handleSDAPISdModels(w http.ResponseWriter, r *http.Request) {
	names := a.svc.Registry.Names()
	current := a.svc.Sup.CurrentModel()
	out := make([]map[string]any, 0, len(names)+1)
	for _, name := range names {
		title := name
		if name == current {
			title = name + " [loaded]"
		}
		out = append(out, map[string]any{
			"title":      title,
			"model_name": name,
			"filename":   name,
			// Placeholder-хеши: A1111-клиенты их только отображают, но
			// отсутствие ключа ломает часть парсеров (nil-hash).
			"hash":   "8888888888",
			"sha256": "8888888888888888888888888888888888888888888888888888888888888888",
			"config": nil,
		})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{
			"title": "no-models", "model_name": "no-models", "filename": "",
			"hash": "8888888888",
			"sha256": "8888888888888888888888888888888888888888888888888888888888888888",
			"config": nil,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSDAPIProxyToEngine — проксирование discovery-списков движка.
//
// ПОЧЕМУ ПРОКСИ, А НЕ КОПИЯ: списки сэмплеров/планировщиков/апскейлеров
// зависят от сборки sd.cpp (например webp/upscaler-модели) — единственный
// источник правды это сам движок. Если процесс не поднят — отдаём пустой
// массив: клиент не должен падать из-за 503 на discovery-пути.
func (a *App) handleSDAPIProxyToEngine(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
			return
		}
		caps := a.svc.Sup.Capabilities()
		if caps == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		switch kind {
		case "samplers":
			out := make([]map[string]any, 0, len(caps.Samplers))
			for _, s := range caps.Samplers {
				out = append(out, map[string]any{"name": s, "aliases": []string{s}, "options": map[string]any{}})
			}
			writeJSON(w, http.StatusOK, out)
		case "schedulers":
			out := make([]map[string]any, 0, len(caps.Schedulers))
			for _, s := range caps.Schedulers {
				out = append(out, map[string]any{"name": s, "label": s})
			}
			writeJSON(w, http.StatusOK, out)
		case "loras":
			out := make([]map[string]any, 0, len(caps.Loras))
			for _, l := range caps.Loras {
				out = append(out, map[string]any{"name": l.Name, "path": l.Path, "alias": l.Name})
			}
			writeJSON(w, http.StatusOK, out)
		case "upscalers":
			out := make([]map[string]any, 0, len(caps.Upscalers))
			for _, u := range caps.Upscalers {
				out = append(out, map[string]any{
					"name": u.Name, "model_name": nil, "model_path": nil,
					"model_url": nil, "scale": 4,
				})
			}
			writeJSON(w, http.StatusOK, out)
		default:
			writeJSON(w, http.StatusOK, []any{})
		}
	}
}
