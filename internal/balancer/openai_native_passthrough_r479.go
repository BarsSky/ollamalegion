// openai_native_passthrough_r479.go — R479 (2026-11-09): OpenAI-поверхность
// обслуживает и Ollama-бэкенды.
//
// ЖИВОЙ ДЕФЕКТ (аудит типов бэкендов, P0). `/v1/*` жёстко помечался как
// `llama_cpp` (`proxy.go`, determineRequestBackendType) → диспетчер оставлял
// только LlamaCppRouter → `getLlamaCppBackends()` фильтровал пул по типу →
// на кластере без cppworker клиент получал `503 no llama.cpp backend available`,
// хотя Ollama умеет `/v1/chat/completions` нативно. То же на смешанном кластере,
// когда модель лежит только на Ollama-ноде.
//
// ЧТО ЗДЕСЬ. Если LlamaCppRouter не нашёл cppworker-бэкенд под модель, запрос
// уходит в Ollama как есть (тот же путь `/v1/...`, то же тело, тот же формат
// ответа) — без трансляции и без «умных» постпроцессингов cppworker-пути.
// Слот занимаем через штатный менеджер, чтобы ёмкость учитывалась.
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// openAIStreamClientR479 — клиент без общего таймаута: стрим длится минутами,
// ограничение ставим на ожидание заголовков (как у основного транспорта).
func (p *Proxy) openAIStreamClientR479() *http.Client {
	if p.streamingClient != nil {
		return p.streamingClient
	}
	return &http.Client{Timeout: 0}
}

// ollamaBackendsForOpenAI — здоровые Ollama-бэкенды, которым можно отдать
// запрос, в порядке предпочтения:
//  1. где модель уже загружена (обычно это «свой» узел);
//  2. больше свободных слотов;
//  3. меньше активных запросов.
func (p *Proxy) ollamaBackendsForOpenAI(model string) []*BackendState {
	candidates := p.getAllActiveBackendsByType(types.BackendTypeOllama)
	type scored struct {
		state       *BackendState
		modelLoaded bool
		free        int
		active      int
	}
	list := make([]scored, 0, len(candidates))
	for _, st := range candidates {
		if st == nil || st.Backend == nil {
			continue
		}
		status := st.Backend.Status
		if status != types.StatusHealthy && string(status) != "degraded" {
			continue
		}
		// R304: донор держит только память мастера — инференс туда не отправляем.
		if p.cannotHostModelR304(st.Backend.ID) {
			continue
		}
		loaded := false
		if p.metricsMgr != nil {
			p.metricsMgr.mu.RLock()
			if m, ok := p.metricsMgr.metrics[st.Backend.ID]; ok && m != nil {
				for _, rm := range m.Ollama.RunningModels {
					if rm.Name == model {
						loaded = true
						break
					}
				}
			}
			p.metricsMgr.mu.RUnlock()
		}
		free, _ := p.GetBackendFreeSlots(st.Backend.ID)
		list = append(list, scored{state: st, modelLoaded: loaded, free: free, active: st.Backend.ActiveRequests})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].modelLoaded != list[j].modelLoaded {
			return list[i].modelLoaded
		}
		if list[i].free != list[j].free {
			return list[i].free > list[j].free
		}
		return list[i].active < list[j].active
	})
	out := make([]*BackendState, 0, len(list))
	for _, s := range list {
		out = append(out, s.state)
	}
	return out
}

// proxyOpenAINativeToOllama — отдать OpenAI-запрос напрямую Ollama-бэкенду.
//
// Возвращает true, если ответ уже отдан клиенту (успех или ответ апстрима),
// false — если подходящего бэкенда нет (тогда вызывающий идёт прежним путём:
// ожидание cppworker-бэкенда и 503 с числами).
func (p *Proxy) proxyOpenAINativeToOllama(w http.ResponseWriter, r *http.Request, bodyBuf []byte) bool {
	if p == nil || r == nil || w == nil {
		return false
	}
	// Только пути, которые Ollama поддерживает нативно. /v1/messages и
	// /v1/responses — чужие форматы, их в Ollama-бэкенд не отдаём (там нет
	// трансляции, и «как есть» вернуло бы ошибку формата).
	if !ollamaSupportsOpenAIPathR479(r.URL.Path) {
		return false
	}
	model := modelFromOpenAIBodyR479(bodyBuf)
	candidates := p.ollamaBackendsForOpenAI(model)
	if len(candidates) == 0 {
		return false
	}
	for _, st := range candidates {
		backendID := st.Backend.ID
		if !p.tryAcquireSlot(backendID) {
			continue
		}
		served := p.forwardOpenAIToOllamaR479(w, r, st, bodyBuf)
		p.releaseSlot(backendID)
		if served {
			return true
		}
		// Ошибка транспорта на этом узле — пробуем следующий, как и остальной
		// балансер: один недоступный бэкенд не должен ломать поверхность.
	}
	return false
}

// forwardOpenAIToOllamaR479 — один запрос к конкретному Ollama-бэкенду.
func (p *Proxy) forwardOpenAIToOllamaR479(w http.ResponseWriter, r *http.Request, st *BackendState, bodyBuf []byte) bool {
	target := fmt.Sprintf("http://%s:%d%s", st.Backend.Host, p.getBackendPort(st.Backend), r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(bodyBuf))
	if err != nil {
		logger.Get().Warnw("openai passthrough: запрос не построен", "backend", st.Backend.ID, "error", err)
		return false
	}
	// Заголовки клиента переносим (Authorization для Ollama Cloud, X-API-Token,
	// User-Agent), кроме hop-by-hop; X-Request-ID ставим свой, чтобы ход
	// оставался связанным.
	for key, values := range r.Header {
		switch strings.ToLower(key) {
		case "host", "content-length", "connection", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	stampUpstreamRequestIDFromRequestR376(req, r)
	req.Header.Set("X-Forwarded-For", p.getClientRealIP(r))
	req.Header.Set("X-Real-IP", p.getClientRealIP(r))

	SetStageR477(r.Context(), stageUpstreamR477)
	client := p.openAIStreamClientR479()
	resp, err := client.Do(req)
	if err != nil {
		logger.Get().Warnw("openai passthrough: апстрим недоступен",
			"backend", st.Backend.ID, "path", r.URL.Path, "error", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	SetStageR477(r.Context(), stageStreamR477)
	SetTransportR484(r.Context(), transportStreamR484)
	w.Header().Set("X-Backend-ID", st.Backend.ID)
	// Ollama отдаёт chunked NDJSON/SSE; буферизация на прокси-слое испортила бы
	// интерактивность (та же причина, что и у основного транспорта).
	w.Header().Set("X-Accel-Buffering", "no")
	for key, values := range resp.Header {
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		case "x-request-id":
			// Свой id уже выставлен в ответ; апстримный — отдельным полем.
			if len(values) > 0 && w.Header().Get("X-Upstream-Request-Id") == "" {
				w.Header().Set("X-Upstream-Request-Id", values[0])
			}
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	logger.Get().Infow("openai passthrough: запрос обслужен Ollama-бэкендом",
		"backend", st.Backend.ID, "path", r.URL.Path, "status", resp.StatusCode,
		"model", modelFromOpenAIBodyR479(bodyBuf))

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			SetStageR477(r.Context(), stageClientR477)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return true // клиент отвалился: запрос уже отдан, ретрай бессмыслен
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	return true
}

// modelFromOpenAIBodyR479 — имя модели из тела (пустая строка, если не разобрали:
// без модели выбор бэкенда идёт по загрузке слота, это допустимо).
func modelFromOpenAIBodyR479(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// ollamaSupportsOpenAIPathR479 — пути OpenAI-поверхности, которые Ollama
// обслуживает нативно и в том же формате.
func ollamaSupportsOpenAIPathR479(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/models":
		return true
	}
	return false
}
