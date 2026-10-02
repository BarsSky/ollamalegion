package balancer

import (
	"net/http"
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

	backendID := ir.selectImageBackend()
	if backendID == "" {
		logger.Get().Warnw("image request: no healthy image backend",
			"path", r.URL.Path, "surface", requestSurfaceOf(r).String())
		ir.writeError(w, r, http.StatusServiceUnavailable, "image_backend_unavailable",
			"no healthy backend of type image_cpp is registered")
		return true
	}

	logger.Get().Infow("image request routed",
		"path", r.URL.Path,
		"backend", backendID,
		"surface", requestSurfaceOf(r).String())

	resp, err := ir.proxy.proxyRequestToBackend(r, backendID, imageRequestTimeout())
	if err != nil {
		logger.Get().Errorw("image request failed",
			"path", r.URL.Path, "backend", backendID, "error", err)
		ir.writeError(w, r, http.StatusBadGateway, "image_backend_error",
			"image backend request failed: "+err.Error())
		return true
	}
	copyResponse(w, resp)
	return true
}

// selectImageBackend — выбор image-бэкенда.
//
// Порядок:
//  1. selectByResources — resource-aware выбор среди image_cpp (учитывает
//     здоровье, лимиты и загрузку; при отсутствии метрик деградирует безопасно);
//  2. selectFreeBackendAny — «любой свободный» image-бэкенд;
//  3. "" → вызывающий отдаёт 503.
//
// Модель пока не участвует в выборе: sd-server всё равно работает с одной
// загруженной моделью на процесс, а учёт моделей image-воркера появится
// в Phase 3 (метрики + профили).
func (ir *ImageRouter) selectImageBackend() string {
	allowed := []types.BackendType{types.BackendTypeImage}
	if id := ir.proxy.selectByResources(allowed); id != "" {
		return id
	}
	return ir.proxy.selectFreeBackendAny(allowed)
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
