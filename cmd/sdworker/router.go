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
//	/api/hf/*     — HF-загрузка image-bundle'ов (проксируется балансером
//	                как есть: /api/v1/image/backends/{id}/proxy/api/hf/...)
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
	// /v1/images/edits — img2img/inpaint (multipart: image[]/image + mask):
	// первое изображение уходит как init_image, mask — как mask_image.
	mux.HandleFunc("/v1/images/edits", a.handleOpenAIImagesEdits)
	// /v1/images/variations — R-Image (2026-10-02): реализовано как img2img
	// с пустым промптом и strength по умолчанию 0.5 (см. handleOpenAIImagesVariations).
	mux.HandleFunc("/v1/images/variations", a.handleOpenAIImagesVariations)
	mux.HandleFunc("/v1/models", a.handleOpenAIModels)

	// --- A1111 WebUI API ---
	mux.HandleFunc("/sdapi/v1/txt2img", a.handleSDAPITxt2Img)
	mux.HandleFunc("/sdapi/v1/img2img", a.handleSDAPIImg2Img)
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
	// R-Image Phase 9: удаление bundle с диска (аналог cppworker /api/models/delete) —
	// без него оператор не мог освободить диск из WebUI.
	mux.HandleFunc("/api/image/models/delete", a.handleDeleteModel)
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

	// --- HF-загрузка image-bundle'ов (Phase 4) ---
	//
	// Пути ОБЯЗАНЫ совпадать с тем, что дёргает UI: балансер проксирует их
	// как есть (/api/v1/image/backends/{id}/proxy/api/hf/...), поэтому
	// несогласие имени даёт 404 на воркере — именно так страница «Изображения»
	// и деградировала раньше (см. webui/js/modules/image-page.js:63-76).
	mux.HandleFunc("/api/hf/search", a.handleHFSearch)
	mux.HandleFunc("/api/hf/files", a.handleHFFiles)
	// R-Image (2026-10-03): пред-проверка файла ДО скачивания — читаем заголовок
	// (Range-запрос) и говорим, КАК движок увидит файл: какое семейство он узнаёт
	// по именам тензоров и нужен ли --diffusion-model. Приговоров «движок это не
	// прочитает» нет: «голые» diffusers-имена есть и у сборок под sd.cpp
	// (см. internal/cppbackend/hf_probe.go и docs/image-generation.md §8.2).
	mux.HandleFunc("/api/hf/probe", a.handleHFProbe)
	mux.HandleFunc("/api/hf/download", a.handleHFDownload)
	mux.HandleFunc("/api/hf/bundle", a.handleHFBundle)
	// Алиасы: UI пробует их, если канонический путь недоступен
	// (BUNDLE_DOWNLOAD_PATHS), а /api/image/models/download — наш собственный
	// контракт «скачать модель на бэкенд». Все три ведут в один хендлер, чтобы
	// не разъезжались.
	mux.HandleFunc("/api/hf/bundle/download", a.handleHFBundle)
	mux.HandleFunc("/api/image/models/download", a.handleHFBundle)
	mux.HandleFunc("/api/hf/progress", a.handleHFProgress)
	mux.HandleFunc("/api/hf/downloads", a.handleHFDownloads)
	mux.HandleFunc("/api/hf/cancel", a.handleHFCancel)
	mux.HandleFunc("/api/hf/cleanup", a.handleHFCleanup)

	// Порядок middleware повторяет cppworker: recover снаружи (паника в любом
	// слое не должна ронять HTTP-соединение без ответа), затем CORS (OPTIONS →
	// 204 обязателен для SillyTavern «Connect»), затем RequestID и лог.
	return recoverMiddleware(
		corsMiddleware(
			observability.RequestIDMiddleware(
				loggingMiddleware(mux))))
}
