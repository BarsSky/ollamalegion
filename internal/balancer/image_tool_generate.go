// image_tool_generate.go — R637 (2026-11-19): генерация: generateImageForTool и первый URL ответа.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"time"
)

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
//
// R85: если выбранная модель ещё не в VRAM, она поднимается ЗДЕСЬ (до гейта и до
// генерации), потому что воркер отвечает 409 model_not_loaded и сам не грузит.
// Гейт берётся ПОСЛЕ загрузки: до неё свободная VRAM считается без веса
// поднимаемой модели, и «влезает/не влезает» было бы посчитано неверно.
func (p *Proxy) generateImageForTool(ctx context.Context, target *imageToolTarget, args imageToolArgs) (map[string]interface{}, error) {
	cfg := p.imageToolSettings()
	// ЗАЩИТА ОТ ВЫРОЖДЕННОГО ПРОМПТА (живой случай 2026-10-06): модель прислала
	// prompt_len=6, и клиент получил красный квадрат — SD на бессмысленном промпте
	// даёт именно мусорную заливку. Лучше честная ошибка, чем «успешная» картинка-
	// заглушка: пользователь не примет квадрат за результат, а модель получит
	// текст, который можно исправить следующим вызовом.
	if err := validateImageToolPrompt(args.Prompt); err != nil {
		return nil, err
	}
	resolved := p.resolveImageToolTarget(ctx, target, args.Model)
	model := resolved.Model

	var loadElapsed time.Duration
	loadedNow := false
	substitutedFrom := ""
	needLoad := !resolved.Loaded && model != ""
	if needLoad && !cfg.AllowLoad {
		return nil, fmt.Errorf(
			i18n.T("balancer.image_tool.not_loaded_allow_load_off"),
			model, imageToolEnvAllowLoad)
	}
	if needLoad && resolved.NotFound {
		// ИМЯ-АРТЕФАКТ (живой случай 2026-10-06): модель передаёт
		// "model":"stable-diffusion.cpp" — это не имя модели на воркере, а
		// заученный ярлык движка. Раньше такой вызов просто отказывал, и
		// пользователь не получал картинку. Если в VRAM уже есть готовая
		// модель — рисуем ею (пользователь просил картинку, а не конкретный
		// файл), а подмену честно сообщаем в результате. Если готовой модели
		// нет — отдаём список доступных имён, чтобы модель исправилась.
		loaded := p.loadedImageModelForTarget(target)
		if loaded == nil {
			return nil, errors.New(p.imageToolUnknownModelError(ctx, model))
		}
		substitutedFrom = model
		resolved = *loaded
		model = loaded.Model
		// Подставленная модель УЖЕ в VRAM: грузить её не нужно (иначе мы бы
		// «перезагружали» то, что и так работает, и теряли бы время).
		needLoad = false
	}
	if needLoad {
		elapsed, err := p.ensureImageModelLoaded(ctx, resolved.BackendID, model)
		loadElapsed = elapsed
		// ФАКТ загрузки фиксируем ФЛАГОМ, а не значением часов: см.
		// imageToolLoadFields — иначе быстрая загрузка, уложившаяся в один тик
		// монотонных часов, объявляла бы себя «загрузки не было».
		loadedNow = err == nil
		if err != nil {
			return nil, err
		}
	}

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
	gate := res.beforeGeneration(ctx, resolved.BackendID)
	if !gate.Allowed {
		// R119 (2026-10-15): раньше здесь был обычный fmt.Errorf("генерация
		// недоступна: %s", gate.Message) — код причины (insufficient_vram /
		// gpu_busy), числа (needed/free/headroom) и подсказка OOM-лестницы
		// ТЕРЯЛИСЬ. Результат уходил модели как {"status":"error","error":"<текст>"},
		// и она не могла объяснить пользователю, чего не хватает и что сделать —
		// хотя соседний путь (прямой image-API) отвечает
		// `503 {code, hint, freeMb, requiredMb}`.
		//
		// Теперь отказ структурирован: машинный код + числа + подсказка доезжают
		// до модели (image_tool_gate.go).
		logger.Get().Warnw(i18n.T("balancer.image_tool.gate_denied_warn"),
			"backend", resolved.BackendID, "model", model, "code", gate.Code,
			"required_mb", gate.Verdict.RequiredMB, "free_mb", gate.Verdict.FreeMB,
			"headroom_mb", gate.Verdict.HeadroomMB, "retry_after_sec", gate.RetryAfterSec)
		return nil, imageToolGateDenied(gate, model)
	}
	defer gate.Release()

	brief := types.ImageRequestBrief{
		BackendID: resolved.BackendID,
		Surface:   imageToolSurface,
		Path:      imageToolPath,
		Model:     model,
		Prompt:    args.Prompt,
		Width:     args.Width,
		Height:    args.Height,
		Steps:     args.Steps,
	}
	handle := res.imageRequests().begin(resolved.BackendID, brief)

	started := time.Now()
	resp, err := p.proxyRequestToBackend(req, resolved.BackendID, cfg.Timeout)
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, http.StatusBadGateway, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("image backend request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, truncated, err := readImageToolResponse(resp.Body)
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("read image backend response: %w", err)
	}
	if truncated {
		// R120: раньше здесь был молчаливый обрез на limit байт, и json.Unmarshal
		// ниже сообщал «invalid JSON: unexpected end of JSON input» — вина
		// выглядела как ошибка воркера, хотя тело обрезал балансер.
		err := imageToolResponseTooLargeError(imageToolResponseLimitBytes)
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode,
			imageToolErrResponseTooLarge, err.Error(), 0)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		msg := upstreamErrorMessage(raw, resp.StatusCode)
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", msg, 0)
		return nil, fmt.Errorf(i18n.T("balancer.image_tool.engine_http_error"), resp.StatusCode, msg)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_bad_response", err.Error(), 0)
		return nil, fmt.Errorf("image backend returned invalid JSON: %w", err)
	}
	url := firstImageURL(doc)
	if url == "" {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_no_image", "no image in response", 0)
		return nil, errors.New(i18n.T("balancer.image_tool.engine_no_image"))
	}

	handle.finish(types.ImageRequestStatusOK, resp.StatusCode, "", "", 1)
	out := map[string]interface{}{
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
		// modelState — где модель оказалась к моменту генерации: модель обязана
		// понимать, что картинка сделана только что поднятой моделью, а не той,
		// что была в VRAM раньше.
		"modelState": imageStateLoaded,
		"backendId":  resolved.BackendID,
	}
	// Поля о загрузке присутствуют ТОЛЬКО когда загрузка была (их наличие —
	// ответ на вопрос «картинку сделала поднятая сейчас модель или та, что уже
	// была в VRAM»): см. imageToolLoadFields.
	for k, v := range imageToolLoadFields(loadedNow, loadElapsed) {
		out[k] = v
	}
	// Подмена модели: модель назвала имя, которого нет на воркере (например
	// заученное "stable-diffusion.cpp"), и мы нарисовали уже загруженной моделью.
	// Сообщаем честно — и модели, и пользователю в её ответе.
	if substitutedFrom != "" {
		out["requestedModel"] = substitutedFrom
		out["modelNote"] = fmt.Sprintf(
			"модели %q нет на image-воркере; использована уже загруженная модель %q. "+
				"Актуальные имена моделей возвращает list_image_models.",
			substitutedFrom, model)
	}
	return out, nil
}

// imageToolResponseLimitBytes и чтение ответа воркера с обнаружением обрезки —
// см. image_tool_response.go (R120: ложный «invalid JSON» на живом прогоне).
