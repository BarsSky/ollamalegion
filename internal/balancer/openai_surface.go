package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// openAISurface — R-Image (2026-09-27): HTTP-поверхность балансера для
// OpenAI-стиля на отдельном порту (по умолчанию 18079).
//
// Что делает:
//  1. CORS + OPTIONS. Клиенты бьют именно сюда (web-клиенты, кнопка «Connect»
//     в SillyTavern дергает OPTIONS /v1/images/generations), а не в sd-server,
//     поэтому CORS обязан отдавать наш слой.
//  2. Ollama-нативные пути (/api/chat, /api/generate, /api/tags, /api/pull …)
//     на этой поверхности НЕ обслуживаются — клиент получает понятную ошибку
//     со ссылкой на универсальный порт 18080. Проксирование этих путей сюда —
//     задел на будущее (плана §3.0), сейчас не приоритет.
//  3. Всё остальное делегируется в тот же *Proxy, что обслуживает 18080, —
//     состояние (бэкенды, сессии, очередь, скоринг, метрики) ОБЩЕЕ, поэтому
//     распределение нагрузки и сессии не дублируются.
//
// Строгость OpenAI-стиля обеспечивается пометкой surfaceOpenAI: на этой
// поверхности parseRequestBody НЕ ставит stream=true по умолчанию (стриминг —
// только по явному полю stream в теле запроса).
type openAISurface struct {
	proxy *Proxy
}

// NewOpenAISurface создаёт обёртку-поверхность вокруг существующего прокси.
func NewOpenAISurface(proxy *Proxy) *openAISurface {
	return &openAISurface{proxy: proxy}
}

// ServeHTTP — точка входа OpenAI-порта.
func (s *openAISurface) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w, r)

	// Preflight: отвечаем сразу, не доходя до бэкендов.
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent) // 204, как у sd-server
		return
	}

	if isOllamaAPIPath(r.URL.Path) {
		logger.Get().Infow("openai surface: ollama-native path rejected",
			"path", r.URL.Path, "hint_port", 18080)
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":   "not_supported_on_openai_surface",
			"message": "Ollama-native endpoints are not served on the OpenAI port",
			"path":    r.URL.Path,
			"hint":    "use the universal proxy port (default 18080) for /api/* Ollama endpoints",
		})
		return
	}

	s.proxy.ServeHTTP(w, withRequestSurface(r, surfaceOpenAI))
}

// setCORSHeaders проставляет CORS-заголовки.
//
// Логика как у sd-server (Origin-or-*), но с корректной комбинацией
// Allow-Credentials: со звёздочкой credentials не выставляем (так требует
// спецификация CORS), с конкретным Origin — эхо + credentials.
func setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Add("Vary", "Origin")
	}
	h.Set("Access-Control-Allow-Methods", "*")
	h.Set("Access-Control-Allow-Headers", "*")
	h.Set("Access-Control-Max-Age", "86400")
}
