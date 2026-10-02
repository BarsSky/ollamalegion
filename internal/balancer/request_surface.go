package balancer

import (
	"context"
	"net/http"
	"strings"
)

// R-Image (2026-09-27): «поверхность» запроса — какой HTTP-слушатель балансера
// его обслужил. Нужна, потому что один экземпляр *Proxy обслуживает два порта
// с разной семантикой:
//
//   - 18080 (surfaceLegacy) — универсальный прокси: прежнее поведение,
//     включая legacy auto-stream (Stream: true по умолчанию) и Ollama-трансляции;
//   - 18079 (surfaceOpenAI) — OpenAI-поверхность: строгий OpenAI-стиль
//     (стриминг ТОЛЬКО по явному полю stream), A1111-прогон /sdapi/v1/*
//     для image-бэкендов, а Ollama-нативные пути отвечают понятной ошибкой
//     со ссылкой на 18080.
//
// Состояние (бэкенды, сессии, очередь, скоринг, метрики) — ОБЩЕЕ: оба слушателя
// держат указатель на один *Proxy, поэтому распределение нагрузки и сессии
// не дублируются.
type requestSurface int

const (
	// surfaceLegacy — порт 18080 (универсальный прокси).
	surfaceLegacy requestSurface = iota
	// surfaceOpenAI — порт 18079 (OpenAI-поверхность).
	surfaceOpenAI
)

// String — для логов.
func (s requestSurface) String() string {
	switch s {
	case surfaceOpenAI:
		return "openai"
	default:
		return "legacy"
	}
}

type surfaceCtxKey struct{}
type parsedReqCtxKey struct{}

// withRequestSurface помечает запрос как пришедший на конкретную поверхность.
func withRequestSurface(r *http.Request, s requestSurface) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), surfaceCtxKey{}, s))
}

// requestSurfaceOf возвращает поверхность запроса (по умолчанию — legacy,
// то есть прежнее поведение: важно для тестов и внутренних вызовов).
func requestSurfaceOf(r *http.Request) requestSurface {
	if r == nil {
		return surfaceLegacy
	}
	if v, ok := r.Context().Value(surfaceCtxKey{}).(requestSurface); ok {
		return v
	}
	return surfaceLegacy
}

// withParsedRequest кладёт уже разобранное тело запроса в контекст, чтобы
// determineRequestBackendType и другие потребители не парсили body повторно.
func withParsedRequest(r *http.Request, p *parsedRequest) *http.Request {
	if r == nil || p == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), parsedReqCtxKey{}, p))
}

// parsedRequestOf возвращает ранее разобранное тело запроса, если оно есть.
func parsedRequestOf(r *http.Request) *parsedRequest {
	if r == nil {
		return nil
	}
	if v, ok := r.Context().Value(parsedReqCtxKey{}).(*parsedRequest); ok {
		return v
	}
	return nil
}

// requestModelHint — имя модели из уже разобранного тела запроса.
//
// Быстрый путь: тело разобрано в ServeHTTP и положено в контекст (withParsedRequest).
// Медленный путь (slot-handler, тестовый хелпер): разбираем здесь —
// parseRequestBody восстанавливает r.Body, поэтому повторный вызов безопасен.
func (p *Proxy) requestModelHint(r *http.Request) string {
	if parsed := parsedRequestOf(r); parsed != nil {
		return parsed.Model
	}
	return p.parseRequestBody(r).Model
}

// imageModelPrefixes — явные префиксы имени модели, означающие «это image-модель».
// R-Image (2026-09-27): маршрутизация по ЯВНОМУ признаку — префикс модели,
// endpoint или поле capability. Никакой классификации по тексту промпта.
var imageModelPrefixes = []string{"sd:", "sd_cpp:", "image:", "img/", "diffusion:"}

// hasImageModelPrefix — true, если имя модели явно помечено как image-модель.
func hasImageModelPrefix(model string) bool {
	if model == "" {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, p := range imageModelPrefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// isImageEndpointPath — пути, которые однозначно относятся к генерации изображений:
//
//   - /v1/images/*   — OpenAI Images (generations / edits / variations);
//   - /sdapi/v1/*    — A1111 WebUI API (его используют SillyTavern, LibreChat SD tool,
//     Open WebUI в automatic1111-режиме; sd-server отдаёт этот API нативно);
//   - /api/image/*   — наш нативный контракт image-воркера.
func isImageEndpointPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/v1/images/"):
		return true
	case strings.HasPrefix(path, "/sdapi/v1/"):
		return true
	case path == "/api/image" || strings.HasPrefix(path, "/api/image/"):
		return true
	}
	return false
}

// isOllamaAPIPath — Ollama-нативные пути API. На OpenAI-поверхности (18079) они
// не обслуживаются: клиенту возвращается понятная ошибка со ссылкой на 18080.
// Это осознанное решение первого этапа; проксирование этих путей на 18079 —
// задел на будущее (см. plans/2026-09-27-image-generation-backend-plan.md §3.0).
//
// NB: не путать с isOllamaNativePath (llamacpp_native_path.go) — там про
// трансляцию тела в OpenAI-формат для cppworker, а не про поверхность.
func isOllamaAPIPath(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	// /api/v1/* — это management-плоскость балансера (не Ollama), её форвардим
	// на API-сервер и на OpenAI-поверхности.
	if strings.HasPrefix(path, "/api/v1/") {
		return false
	}
	// Наши image-эндпоинты (/api/image/*) — тоже не Ollama.
	if path == "/api/image" || strings.HasPrefix(path, "/api/image/") {
		return false
	}
	return true
}
