package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ---------- Read-only endpoints ----------

// handleTags — агрегирует список моделей со всех llama.cpp бэкендов.
//
// Возвращает ОБЪЕДИНЕНИЕ (дедуплицированное по имени):
//  1. Loaded models из метрик (быстро, in-memory)
//  2. Available models (cppworker's /api/models/files, все .gguf на диске)
//
// Round 19 hotfix (2026-08-03): раньше fallbacks (cppworker /api/tags) запускались
// ТОЛЬКО когда loaded=0. В результате если 1+ модель загружена — клиенту возвращался
// только loaded список, БЕЗ on-disk моделей. OpenWebUI не видел остальные .gguf
// файлы, которые можно подгрузить lazy-load'ом.
//
// Теперь fallbacks запускаются ВСЕГДА (best-effort, ранний выход если уже нашли
// что-то). Источники объединяются, дедуп по name.
func (lr *LlamaCppRouter) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
		return
	}

	// 1) Loaded models из метрик всех llama.cpp бэкендов (быстро, in-memory).
	//
	// Round 19 hotfix (2026-08-03): читаем из `llamaMetrics[id]` (куда пишет
	// llamaCppMetricsPoller), а не из `metrics[id].LlamaCpp` (куда пишет
	// Ollama-agent). Без этого fix'а — `LoadedModels` всегда пустой для бэкендов
	// без agent, и loaded модели пропадают из /api/tags.
	lr.proxy.metricsMgr.mu.RLock()
	uniqueModels := make(map[string]OllamaTag)
	for _, b := range backends {
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.id]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			if _, exists := uniqueModels[m.Name]; !exists {
				uniqueModels[m.Name] = OllamaTag{
					Name:  m.Name,
					Model: m.Name,
					Size:  0,
				}
			}
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// 2) ВСЕГДА опрашиваем /api/models/files на каждом бэкенде чтобы получить
	//    полный список .gguf на диске (включая выгруженные). Round 19 hotfix:
	//    убрал условие `if len(uniqueModels) == 0` — раньше on-disk список
	//    возвращался ТОЛЬКО когда loaded=0, скрывая остальные модели от клиента.
	//    Дедупликация по имени сохраняется (map).
	for _, b := range backends {
		files, err := lr.fetchLlamaCppFiles(b.host, b.port)
		if err != nil {
			logger.Get().Debugw("handleTags: /api/models/files fetch failed (non-fatal)",
				"backend", b.id, "host", b.host, "port", b.port, "error", err)
			continue
		}
		for _, f := range files {
			// f.Name includes .gguf extension, strip for Ollama convention
			name := f.Name
			if strings.HasSuffix(strings.ToLower(name), ".gguf") {
				name = name[:len(name)-5]
			}
			if _, exists := uniqueModels[name]; !exists {
				uniqueModels[name] = OllamaTag{
					Name:       name,
					Model:      name,
					Size:       f.SizeBytes,
					ModifiedAt: f.ModifiedAt,
				}
			}
		}
	}

	// 3) Final fallback: если по-прежнему пусто (все бэкенды недоступны), пробуем
	//    cppworker's /api/tags — он делает то же что и files, но с другим форматом.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			tags, err := lr.fetchLlamaCppTags(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleTags: final fallback cppworker /api/tags failed",
					"backend", b.id, "host", b.host, "port", b.port, "error", err)
				continue
			}
			for _, t := range tags {
				if _, exists := uniqueModels[t.Name]; !exists {
					uniqueModels[t.Name] = t
				}
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	models := make([]OllamaTag, 0, len(uniqueModels))
	for _, m := range uniqueModels {
		models = append(models, m)
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})

	logger.Get().Infow("handleTags: returning models",
		"count", len(models),
	)

	writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: models})
}

// fetchLlamaCppModels — запрашивает /v1/models у llama.cpp бэкенда
// и парсит OpenAI-совместимый ответ в список OllamaTag.
func (lr *LlamaCppRouter) fetchLlamaCppModels(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Data))
	for _, d := range result.Data {
		tags = append(tags, OllamaTag{
			Name:  d.ID,
			Model: d.ID,
			Size:  0,
		})
	}
	return tags, nil
}

// fetchLlamaCppFiles — запрашивает /api/models/files у cppworker.
// Возвращает список ВСЕХ .gguf файлов на диске (включая выгруженные).
// Round 19 hotfix: используется как primary source для on-disk моделей.
//
// Преимущества перед /api/tags:
//   - быстрее (только fs.ReadDir, без загрузки метаданных модели)
//   - всегда возвращает актуальный список (не зависит от того, загружена модель или нет)
//   - не зависает если какая-то модель в состоянии loading
type llamaCppFileEntry struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}

func (lr *LlamaCppRouter) fetchLlamaCppFiles(host string, port int) ([]llamaCppFileEntry, error) {
	url := fmt.Sprintf("http://%s:%d/api/models/files", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Files []struct {
			Name       string `json:"name"`
			SizeBytes  int64  `json:"sizeBytes"`
			ModifiedAt string `json:"modifiedAt"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	entries := make([]llamaCppFileEntry, 0, len(result.Files))
	for _, f := range result.Files {
		var modTime time.Time
		if f.ModifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, f.ModifiedAt); err == nil {
				modTime = t
			}
		}
		entries = append(entries, llamaCppFileEntry{
			Name:       f.Name,
			SizeBytes:  f.SizeBytes,
			ModifiedAt: modTime,
		})
	}
	return entries, nil
}

// fetchLlamaCppTags — запрашивает Ollama-совместимый /api/tags у cppworker.
// В отличие от /v1/models, отдаёт ВСЕ .gguf файлы на диске (включая выгруженные),
// с полным Ollama-форматом: name, model, size, digest, modified_at, details.
func (lr *LlamaCppRouter) fetchLlamaCppTags(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model,omitempty"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest,omitempty"`
			ModifiedAt string `json:"modified_at,omitempty"`
			Details    struct {
				Format          string `json:"format,omitempty"`
				Family          string `json:"family,omitempty"`
				ParameterSize   string `json:"parameter_size,omitempty"`
				QuantizationLvl string `json:"quantization_level,omitempty"`
			} `json:"details,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Models))
	for _, m := range result.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		// Убираем расширение .gguf для консистентности с Ollama-форматом.
		displayName := strings.TrimSuffix(name, ".gguf")

		details := map[string]interface{}{}
		if m.Details.Format != "" {
			details["format"] = m.Details.Format
		}
		if m.Details.Family != "" {
			details["family"] = m.Details.Family
		}
		if m.Details.ParameterSize != "" {
			details["parameter_size"] = m.Details.ParameterSize
		}
		if m.Details.QuantizationLvl != "" {
			details["quantization_level"] = m.Details.QuantizationLvl
		}

		var modTime time.Time
		if m.ModifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, m.ModifiedAt); err == nil {
				modTime = t
			} else {
				logger.Get().Debugw("fetchLlamaCppTags: failed to parse modified_at",
					"value", m.ModifiedAt, "error", err)
			}
		}

		tags = append(tags, OllamaTag{
			Name:       displayName,
			Model:      displayName,
			Size:       m.Size,
			Digest:     m.Digest,
			ModifiedAt: modTime,
			Details:    details,
		})
	}
	return tags, nil
}

// handleModels — возвращает список моделей в нативном формате cppworker'а.
// Агрегируем по всем llama.cpp бэкендам (с дедупликацией по path).
func (lr *LlamaCppRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count":  0,
			"models": []interface{}{},
		})
		return
	}

	type cppModel struct {
		Name          string `json:"name"`
		Path          string `json:"path,omitempty"`
		State         string `json:"state,omitempty"`
		SizeBytes     int64  `json:"sizeBytes,omitempty"`
		NLayers       int    `json:"nLayers,omitempty"`
		NHeads        int    `json:"nHeads,omitempty"`
		NEmbd         int    `json:"nEmbd,omitempty"`
		NVocab        int    `json:"nVocab,omitempty"`
		ContextSize   int    `json:"contextSize,omitempty"`
		GPULayers     int    `json:"gpuLayers,omitempty"`
		ActiveQueries int    `json:"activeQueries,omitempty"`
		TotalQueries  int    `json:"totalQueries,omitempty"`
		LoadedAt      string `json:"loadedAt,omitempty"`
		Backend       string `json:"backend,omitempty"`
	}
	type cppResponse struct {
		Count  int        `json:"count"`
		Models []cppModel `json:"models"`
	}

	merged := cppResponse{Models: []cppModel{}}
	seen := make(map[string]bool)

	for _, b := range backends {
		resp, err := lr.fetchLlamaCppModelsNative(b.host, b.port)
		if err != nil {
			logger.Get().Warnw("handleModels: fetch failed",
				"backend", b.id, "host", b.host, "port", b.port, "error", err)
			continue
		}
		for _, m := range resp.Models {
			key := m.Path
			if key == "" {
				key = m.Name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged.Models = append(merged.Models, cppModel{
				Name:          m.Name,
				Path:          m.Path,
				State:         m.State,
				SizeBytes:     m.SizeBytes,
				NLayers:       m.NLayers,
				NHeads:        m.NHeads,
				NEmbd:         m.NEmbd,
				NVocab:        m.NVocab,
				ContextSize:   m.ContextSize,
				GPULayers:     m.GPULayers,
				ActiveQueries: m.ActiveQueries,
				TotalQueries:  m.TotalQueries,
				LoadedAt:      m.LoadedAt,
				Backend:       b.id,
			})
		}
	}
	merged.Count = len(merged.Models)

	logger.Get().Infow("handleModels: returning models",
		"count", merged.Count,
		"backends_queried", len(backends),
	)

	writeJSON(w, http.StatusOK, merged)
}

// fetchLlamaCppModelsNative — запрашивает нативный /api/models у cppworker.
func (lr *LlamaCppRouter) fetchLlamaCppModelsNative(host string, port int) (*cppWorkerModelsNative, error) {
	url := fmt.Sprintf("http://%s:%d/api/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result cppWorkerModelsNative
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// handlePS — возвращает список запущенных моделей на llama.cpp бэкендах (из метрик).
func (lr *LlamaCppRouter) handlePS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaPSResponse{Models: []OllamaProcess{}})
		return
	}

	lr.proxy.metricsMgr.mu.RLock()
	var allProcesses []OllamaProcess
	for _, b := range backends {
		// Round 19 hotfix: читаем из llamaMetrics (cppworker-poller), не metrics[id].LlamaCpp (Ollama-agent).
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.id]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			allProcesses = append(allProcesses, OllamaProcess{
				Name:     m.Name,
				Model:    m.Name,
				Size:     0,
				SizeVRAM: 0,
			})
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	writeJSON(w, http.StatusOK, OllamaPSResponse{Models: allProcesses})
}

// handleVersion — возвращает версию балансировщика.
func (lr *LlamaCppRouter) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"version":       "ollamalegion-1.0.0",
		"llamaVersions": make(map[string]string),
	}

	writeJSON(w, http.StatusOK, response)
}

// handleOpenAIModels — возвращает список моделей в OpenAI-формате
// {"object":"list","data":[{"id":"...","object":"model","created":...,"owned_by":"ollamalegion"}]}
// Используется OpenWebUI при обращении к /openai/v1/models.
//
// Round 19 hotfix (2026-08-03): баг был в том, что fallback на /v1/models и
// /api/models/files срабатывал ТОЛЬКО когда loaded=0. Если хоть 1 модель загружена —
// клиент видел только её. Теперь объединяем loaded + on-disk ВСЕГДА, как в handleTags.
func (lr *LlamaCppRouter) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	uniqueModels := make(map[string]bool)

	// 1) Loaded models из метрик всех llama.cpp бэкендов.
	//    Round 19 hotfix: читаем из llamaMetrics (cppworker-poller), не metrics[id].LlamaCpp (Ollama-agent).
	lr.proxy.metricsMgr.mu.RLock()
	for _, b := range backends {
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.id]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			uniqueModels[m.Name] = true
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// 2) ВСЕГДА опрашиваем /api/models/files чтобы получить .gguf на диске.
	//    Round 19 hotfix: убрал `if len(uniqueModels) == 0` guard.
	for _, b := range backends {
		files, err := lr.fetchLlamaCppFiles(b.host, b.port)
		if err != nil {
			logger.Get().Debugw("handleOpenAIModels: /api/models/files fetch failed (non-fatal)",
				"backend", b.id, "error", err)
			continue
		}
		for _, f := range files {
			name := f.Name
			if strings.HasSuffix(strings.ToLower(name), ".gguf") {
				name = name[:len(name)-5]
			}
			uniqueModels[name] = true
		}
	}

	// 3) Final fallback: cppworker /v1/models (только loaded), затем /api/tags.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			models, err := lr.fetchLlamaCppModels(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleOpenAIModels: final fallback /v1/models failed",
					"backend", b.id, "error", err)
				continue
			}
			for _, m := range models {
				uniqueModels[m.Name] = true
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(uniqueModels))
	for name := range uniqueModels {
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  now,
			"owned_by": "ollamalegion",
		})
	}
	sort.Slice(data, func(i, j int) bool {
		idI, _ := data[i]["id"].(string)
		idJ, _ := data[j]["id"].(string)
		return idI < idJ
	})

	logger.Get().Infow("handleOpenAIModels: returning models",
		"count", len(data),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}
