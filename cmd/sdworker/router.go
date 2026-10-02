// router.go — HTTP-маршруты sdworker.
package main

import (
	"net/http"

	"ollama-loadbalancer/pkg/observability"
)

// setupRouter собирает mux со ВСЕМИ маршрутами воркера.
//
// Группы путей (важно для балансера): балансер форвардит запрос КАК ЕСТЬ
// (internal/balancer/proxy_request.go), поэтому воркер обязан отдавать ровно те
// пути, которые перечислены в isImageEndpointPath:
//
//	/v1/images/*  — OpenAI Images (Open WebUI, LobeChat, Cherry, n8n, OpenAI SDK)
//	/sdapi/v1/*   — A1111 WebUI API (SillyTavern, LibreChat SD tool, Open WebUI)
//	/api/image/*  — наш нативный контракт (балансер, WebUI, диагностика)
//
// Плюс служебное: /health, /metrics, /images/* (статика для response_format:url).
func (a *App) setupRouter() http.Handler {
	mux := http.NewServeMux()

	// --- служебное ---
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/metrics", a.handleMetrics)
	mux.HandleFunc("/info", a.handleInfo)
	mux.HandleFunc("/api/image/info", a.handleInfo)

	// --- OpenAI-совместимая поверхность ---
	mux.HandleFunc("/v1/images/generations", a.handleOpenAIImagesGenerations)
	// /v1/images/edits (img2img/inpaint, multipart) сознательно НЕ реализован:
	// это отдельная фича (init_image + mask), вне Phase 3. Отдаём понятную
	// ошибку вместо 404 от mux — клиент (Open WebUI) умеет её показать.
	mux.HandleFunc("/v1/images/edits", a.handleNotImplemented("img2img/inpaint (POST /v1/images/edits)"))
	mux.HandleFunc("/v1/images/variations", a.handleNotImplemented("image variations"))
	mux.HandleFunc("/v1/models", a.handleOpenAIModels)

	// --- A1111 WebUI API ---
	mux.HandleFunc("/sdapi/v1/txt2img", a.handleSDAPITxt2Img)
	mux.HandleFunc("/sdapi/v1/img2img", a.handleNotImplemented("img2img (POST /sdapi/v1/img2img)"))
	// Заглушки, которые дёргают SillyTavern и Open WebUI (§12.4 п.6).
	mux.HandleFunc("/sdapi/v1/options", a.handleSDAPIOptions)
	mux.HandleFunc("/sdapi/v1/progress", a.handleSDAPIProgress)
	mux.HandleFunc("/sdapi/v1/interrupt", a.handleSDAPIInterrupt)
	mux.HandleFunc("/sdapi/v1/sd-vae", a.handleSDAPISdVae)
	mux.HandleFunc("/sdapi/v1/sd-modules", a.handleSDAPISdModules)
	// Проксируем на движок то, что он отдаёт сам (списки для UI).
	mux.HandleFunc("/sdapi/v1/samplers", a.handleSDAPIProxyToEngine("samplers"))
	mux.HandleFunc("/sdapi/v1/schedulers", a.handleSDAPIProxyToEngine("schedulers"))
	mux.HandleFunc("/sdapi/v1/loras", a.handleSDAPIProxyToEngine("loras"))
	mux.HandleFunc("/sdapi/v1/upscalers", a.handleSDAPIProxyToEngine("upscalers"))
	mux.HandleFunc("/sdapi/v1/sd-models", a.handleSDAPISdModels)

	// --- Наш нативный контракт ---
	mux.HandleFunc("/api/image/capabilities", a.handleCapabilities)
	mux.HandleFunc("/api/image/models", a.handleListModels)
	mux.HandleFunc("/api/image/models/load", a.handleLoadModel)
	mux.HandleFunc("/api/image/models/unload", a.handleUnloadModel)
	mux.HandleFunc("/api/image/models/reload", a.handleReloadModel)
	mux.HandleFunc("/api/image/models/load/progress", a.handleLoadProgress)
	mux.HandleFunc("/api/image/models/load/progress/stream", a.handleLoadProgressStream)
	mux.HandleFunc("/api/image/generate", a.handleGenerate)
	mux.HandleFunc("/api/image/jobs", a.handleListJobs)
	mux.HandleFunc("/api/image/jobs/", a.handleJobByID) // {id} + /{id}/cancel
	mux.HandleFunc("/api/image/queue", a.handleQueue)

	// --- Статика картинок (response_format:"url") ---
	// http.FileServer безопасен от path traversal (net/http отклоняет «..»),
	// но каталог отдаём только этот — не весь /app.
	mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(a.svc.Store.Dir()))))

	// /api/hf/* НЕ реализуем: HF-загрузка image-bundle'ов — Phase 2
	// (internal/api + internal/cppbackend/hf_downloader.go). Контракт для
	// будущего проксирующего слоя описан в отчёте Phase 3.

	// Порядок middleware повторяет cppworker: recover снаружи (паника в любом
	// слое не должна ронять HTTP-соединение без ответа), затем CORS (OPTIONS →
	// 204 обязателен для SillyTavern «Connect»), затем RequestID и лог.
	return recoverMiddleware(
		corsMiddleware(
			observability.RequestIDMiddleware(
				loggingMiddleware(mux))))
}
