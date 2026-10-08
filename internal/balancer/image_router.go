package balancer

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ImageRouter — R-Image (2026-09-27): маршрутизатор запросов генерации
// изображений на бэкенды типа image_cpp (stable-diffusion.cpp / sd-server,
// в перспективе — наш image-воркер).
//
// Отличия от LlamaCppRouter (осознанные):
//   - НЕ проходит через llama-специфичные шаги: preflight n_ctx, AutoTune,
//     workload tracking, normalizeOpenAIBody (последний вырезает image_url);
//   - НЕ включает streaming-ветку: генерация изображения — один JSON-ответ
//     (b64) либо бинарный PNG, стримить нечего;
//   - НЕ вызывает warmup/load текстовых моделей;
//   - выбирает бэкенд строго среди image_cpp (фильтр allowedTypes внутри
//     selectBackend/selectByResources уже это умеет).
//
// Пути: /v1/images/*, /sdapi/v1/*, /api/image/* (см. isImageEndpointPath).
type ImageRouter struct {
	proxy *Proxy
}

// NewImageRouter — создание маршрутизатора image-бэкендов.
func NewImageRouter(proxy *Proxy) *ImageRouter {
	return &ImageRouter{proxy: proxy}
}

// BackendType — реализация BackendRouter.
func (ir *ImageRouter) BackendType() types.BackendType {
	return types.BackendTypeImage
}

// Name — реализация BackendRouter.
func (ir *ImageRouter) Name() string {
	return "ImageRouter"
}

// imageRequestTimeout — duration-кап HTTP-клиента для генерации изображения.
//
// Генерация на слабых GPU занимает от секунд до минут (замеры: 9.6 с на
// RTX 3060 / FLUX-schnell 512², 341 с на GTX 1060 / Z-Image 512×1024 20 шагов).
// Поэтому по умолчанию кап НЕ ставится (0 = ждать терминального состояния),
// как и для операций с моделью (см. modelOpHTTPTimeout). Явный кап —
// opt-in оператора через LB_ALLOW_IMAGE_TIMEOUT_SEC.
func imageRequestTimeout() time.Duration {
	return optInTimeoutSeconds("LB_ALLOW_IMAGE_TIMEOUT_SEC", "генерация изображения")
}

// Route — диспетчеризация запроса на image-бэкенд.
// Возвращает true, если путь относится к генерации изображений (даже если
// подходящего бэкенда нет — тогда клиент получает внятную ошибку, а не
// проваливание в текстовый flow).
func (ir *ImageRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	if !isImageEndpointPath(r.URL.Path) {
		return false
	}

	// R84 (2026-10-03): отдача готовых картинок (GET /v1/images/files/{name}) —
	// это НЕ генерация: ни VRAM-гейта, ни записи в ленту image-запросов быть не
	// должно, иначе обычная загрузка картинки портила бы метрики генерации.
	if name, ok := imageFileNameFromPath(r.URL.Path); ok {
		ir.serveImageFile(w, r, name)
		return true
	}

	// Phase 8 (2026-10-03): поток image-запросов для метрик.
	//
	// Считаем ТОЛЬКО генерацию: /api/image/models, load/unload, capabilities —
	// это управление, и подмешивать его в RPS/среднее время генерации значило бы
	// показывать оператору бессмысленные цифры.
	//
	// Метаданные (модель/размер/шаги/промпт) снимаем здесь, ДО проксирования:
	// тело читается один раз и возвращается на место (см. image_request_meta.go).
	store := ir.proxy.imageResources().imageRequests()
	generation := isImageGenerationRequest(r)
	var meta types.ImageRequestBrief
	if generation {
		meta = imageRequestBriefFor(r)
	}

	backendID := ir.selectImageBackend()
	if backendID == "" {
		logger.Get().Warnw("image request: no healthy image backend",
			"path", r.URL.Path, "surface", requestSurfaceOf(r).String())
		// Отказ «нет бэкенда» тоже обязан быть виден в метриках: иначе оператор
		// видит пустой монитор там, где клиенты получают 503.
		store.gateDenied("", "image_backend_unavailable",
			"no healthy backend of type image_cpp is registered")
		ir.writeError(w, r, http.StatusServiceUnavailable, "image_backend_unavailable",
			"no healthy backend of type image_cpp is registered")
		return true
	}

	// R-Image Phase 6 (2026-10-02): VRAM-гейт + ресурсный лок GPU.
	//
	// ТОЛЬКО для запросов ГЕНЕРАЦИИ: GET /api/image/models, /capabilities,
	// load/unload/progress — управление, они не занимают GPU и не должны
	// получать 503 из-за нехватки VRAM (иначе оператор не смог бы даже
	// посмотреть состояние и выгрузить модель).
	//
	// Освобождение лока — в defer: он переживает и ошибку проксирования, и
	// панику (при разворачивании стека defer выполняется). Для асинхронного
	// нативного пути defer становится no-op, потому что владение передаётся
	// сторожу (см. imageResources.watchAsyncGeneration).
	var gate imageGateDecision
	if generation {
		gate = ir.proxy.imageResources().beforeGeneration(r.Context(), backendID)
		if !gate.Allowed {
			logger.Get().Warnw("image generation rejected by gate",
				"path", r.URL.Path,
				"backend", backendID,
				"code", gate.Code,
				"status", gate.Status)
			store.gateDenied(backendID, gate.Code, gate.Message)
			ir.writeGateError(w, r, gate)
			return true
		}
		defer func() { gate.Release() }()
	}

	// Запись «в полёте» появляется ДО обращения к воркеру: долгая генерация
	// (десятки секунд) должна быть видна в ленте сразу, а не только на финише.
	var handle *imageRequestHandle
	if generation {
		handle = store.begin(backendID, meta)
	}

	logger.Get().Infow("image request routed",
		"path", r.URL.Path,
		"backend", backendID,
		"surface", requestSurfaceOf(r).String())

	resp, err := ir.proxy.proxyRequestToBackend(r, backendID, imageRequestTimeout())
	if err != nil {
		logger.Get().Errorw("image request failed",
			"path", r.URL.Path, "backend", backendID, "error", err)
		// R88 (2026-10-08): та же классификация транспортных ошибок, что у
		// текстовой стороны (writeUpstreamError): 503 + Retry-After для
		// «бэкенд недоступен», 504 для таймаута, 502 иначе — с error_type,
		// backend_id, detail и hint. До фикса клиент получал 502 и сырой текст
		// транспорта: «image backend request failed: Post ...: dial tcp ...:
		// connect: connection refused» без единого слова о том, что делать.
		errType := UpstreamErrorType(err)
		status := UpstreamErrorStatus(errType)
		handle.finish(types.ImageRequestStatusFailed, status, "image_"+errType, err.Error(), 0)
		writeUpstreamError(w, backendID, meta.Model, err)
		return true
	}

	// Асинхронная постановка джобы (нативный контракт: 202 + job id): генерация
	// продолжится ПОСЛЕ ответа, поэтому лок GPU передаём сторожу, а не снимаем
	// в defer. Для синхронных поверхностей (/v1/images/*, /sdapi/v1/*) ответ
	// приходит уже с готовой картинкой — лок снимается сразу.
	if handle != nil && gate.holder != nil && isImageAsyncSubmitPath(r) && resp.StatusCode == http.StatusAccepted {
		// Ответ 202 означает «принято в очередь», а не «картинка готова»:
		// исход посчитает сторож (status=finished), поэтому здесь только
		// отмечаем постановку, не закрывая запись.
		handle.markAccepted()
		if res := ir.proxy.imageResources(); res != nil {
			res.watchAsyncGeneration(gate.holder, handle)
		}
	} else if handle != nil {
		code, msg := "", ""
		if resp.StatusCode >= 400 {
			code, msg = peekImageErrorBody(resp)
		}
		status := types.ImageRequestStatusOK
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			status = types.ImageRequestStatusFailed
			if code == "" {
				code = "image_backend_http_" + strconv.Itoa(resp.StatusCode)
			}
		}
		handle.finish(status, resp.StatusCode, code, msg, imageResponseImages(meta, resp.StatusCode))
	}
	copyResponse(w, resp)
	return true
}

// isImageGenerationRequest — запросы, которые РЕАЛЬНО занимают GPU.
//
// Список явный, а не «префикс + POST»: под /sdapi/v1/* есть POST-эндпоинты
// управления (interrupt, options), и гейтить их нельзя — иначе отмена
// генерации требовала бы свободной VRAM, а смена модели — свободного лока.
func isImageGenerationRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/images/generations",
		"/v1/images/edits",
		"/v1/images/variations",
		"/sdapi/v1/txt2img",
		"/sdapi/v1/img2img",
		"/api/image/generate":
		return true
	}
	return false
}

// isImageAsyncSubmitPath — путь, отвечающий 202 на постановку джобы (генерация
// идёт фоном). У /v1/images/* и /sdapi/v1/* ответ синхронный.
func isImageAsyncSubmitPath(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && r.URL.Path == "/api/image/generate"
}

// writeGateError — отказ гейта в форме, ожидаемой клиентом.
//
// Отличие от writeError: несёт HINT (OOM-лестницу или «загрузите модель») и
// Retry-After для 429/503 с ожиданием. Клиент/оператор обязан видеть, что
// делать, а не только «503».
func (ir *ImageRouter) writeGateError(w http.ResponseWriter, r *http.Request, gate imageGateDecision) {
	status := gate.Status
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	if gate.RetryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(gate.RetryAfterSec))
	}

	if strings.HasPrefix(r.URL.Path, "/v1/") {
		// OpenAI-конверт. hint кладём ВНУТРЬ error: SDK читают message/type/code,
		// а оператор и curl видят подсказку; лишние поля OpenAI-схему не ломают.
		body := map[string]interface{}{
			"message": gate.Message,
			"type":    "invalid_request_error",
			"code":    gate.Code,
		}
		if gate.Hint != "" {
			body["hint"] = gate.Hint
		}
		if gate.RetryAfterSec > 0 {
			body["retry_after_seconds"] = gate.RetryAfterSec
		}
		writeJSON(w, status, map[string]interface{}{"error": body})
		return
	}
	// Не-/v1 поверхности (sdapi/v1, /api/image/*): плоский конверт, как у
	// writeError, плюс hint.
	body := map[string]string{
		"error":   gate.Code,
		"message": gate.Message,
	}
	if gate.Hint != "" {
		body["hint"] = gate.Hint
	}
	writeJSON(w, status, body)
}

// selectImageBackend — выбор image-бэкенда.
//
// Порядок:
//  1. selectByResourcesExcluding — resource-aware выбор среди image_cpp, из
//     которого исключены бэкенды, физически не способные обслужить генерацию
//     (пустой каталог моделей, см. backendsWithoutModels);
//  2. selectFreeBackendAny — «любой свободный» image-бэкенд. Он НЕ учитывает
//     исключение сознательно: если моделей нет нигде, честнее выбрать хоть
//     кого-то и получить от гейта внятный image_model_not_loaded с подсказкой,
//     чем отдать «нет бэкенда» и потерять подсказку о причине.
//  3. "" → вызывающий отдаёт 503.
//
// R88 (2026-10-08): учёт моделей появился именно здесь. Живой случай: вторая
// машина (192.168.13.34) зарегистрирована и здорова, но каталог bundle'ов пуст,
// и метрик у неё нет — прежний выбор «по ресурсам» отдавал ей КАЖДЫЙ запрос
// (выглядела свободной), гейт отвечал image_model_not_loaded, и генерация не
// работала целиком, хотя на локальном воркере модель была загружена.
func (ir *ImageRouter) selectImageBackend() string {
	allowed := []types.BackendType{types.BackendTypeImage}
	if id := ir.proxy.selectByResourcesExcluding(ir.backendsWithoutModels(), allowed); id != "" {
		return id
	}
	return ir.proxy.selectFreeBackendAny(allowed)
}

// backendsWithoutModels — image-бэкенды, у которых НЕТ ни загруженной модели, ни
// моделей на диске: такой воркер не может обслужить генерацию НИКОГДА (модель
// взять неоткуда), и выбирать его — гарантированный отказ.
//
// Решение принимается ТОЛЬКО по достоверному снимку: если снимка ещё нет
// (воркер не опрошен) или последний опрос упал, бэкенд НЕ исключается — иначе
// мы бы выкинули исправный узел из-за отсутствия данных.
//
// Возвращает nil, когда исключать нечего (тогда selectByResourcesExcluding
// работает как обычный selectByResources).
func (ir *ImageRouter) backendsWithoutModels() map[string]bool {
	if ir == nil || ir.proxy == nil {
		return nil
	}
	res := ir.proxy.imageResources()
	if res == nil {
		return nil // гейт/лок выключены — выбор не сужаем
	}
	var exclude map[string]bool
	for _, state := range ir.proxy.filterBackendsByType(types.BackendTypeImage) {
		if state == nil || state.Backend == nil {
			continue
		}
		id := state.Backend.ID
		snap := res.metricsSnapshot(id)
		if snap == nil || snap.lastErr != "" {
			continue // данных нет / опрос падал — не судим
		}
		if snap.loaded() != nil || len(snap.modelNames()) > 0 {
			continue // есть чем обслужить
		}
		if exclude == nil {
			exclude = map[string]bool{}
		}
		exclude[id] = true
		logger.Get().Infow("image backend skipped: no models in its catalog",
			"backend", id, "host", state.Backend.Host,
			"hint", "скачайте bundle на этот узел (WebUI → HuggingFace) или выберите другой бэкенд")
	}
	return exclude
}

// imageModelAliases — идентификаторы, которые отдаются в GET /v1/models,
// когда в кластере есть image-бэкенд.
//
// Зачем алиасы:
//   - sd-server сам рапортует модель как "sd-cpp-local" — его и повторяем;
//   - часть клиентов фильтрует список моделей жёстко по префиксу: legacy-нода
//     n8n оставляет только id, начинающиеся с "dall-" (иначе дропдаун пуст),
//     поэтому даём dall-e-2/dall-e-3;
//   - "gpt-image-*" СОЗНАТЕЛЬНО не добавляем: клиенты (Open WebUI, Cherry
//     Studio) по этому префиксу переключаются в режим «ответ придёт ссылкой
//     (url)», а sd-server отдаёт только b64_json.
var imageModelAliases = []string{"sd-cpp-local", "dall-e-2", "dall-e-3"}

// imageModelIDsForModelsList — алиасы для /v1/models, если есть image-бэкенд.
// В «текстовом» кластере возвращает nil: подмешивать dall-e в список моделей
// без единого image-бэкенда нельзя — клиент попробует сгенерировать картинку
// и получит 503.
func (p *Proxy) imageModelIDsForModelsList() []string {
	if p == nil {
		return nil
	}
	if len(p.filterBackendsByType(types.BackendTypeImage)) > 0 ||
		len(p.getAllActiveBackendsByType(types.BackendTypeImage)) > 0 {
		return imageModelAliases
	}
	// Ни одного зарегистрированного image-бэкенда — алиасы не нужны.
	return nil
}

// writeError — ошибка в форме, ожидаемой клиентом.
//
// OpenAI-клиенты (и SDK) ожидают конверт {"error":{"message","type","code"}},
// тогда как sd.cpp отдаёт {"error":"строка"} — часть SDK такое не парсит.
// Для /v1/* отдаём OpenAI-форму, для остальных путей — плоскую.
func (ir *ImageRouter) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeJSON(w, status, map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
				"type":    "invalid_request_error",
				"code":    code,
			},
		})
		return
	}
	writeJSON(w, status, map[string]string{
		"error":   code,
		"message": message,
	})
}
