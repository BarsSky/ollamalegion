package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// handleRuntimeConfig — агрегирует реальные параметры загруженных моделей
// со всех llama.cpp бэкендов через cppworker-эндпоинт
// `/api/v1/cppworker/config/runtime`.
//
// Назначение: WebUI на вкладке GGUF Models показывает default-значения
// (из /api/v1/cppworker/config) и runtime-значения (из этого endpoint'а)
// рядом: "default: 8192, runtime: 32768 (gemma-4)". Это позволяет оператору
// видеть, что модель реально загружена с другими параметрами (например,
// после auto-reload через balancer), и при необходимости применить новые
// defaults через WebUI.
//
// Если бэкендов нет — возвращаем 200 с пустым массивом (не 503) —
// это состояние "всё ok, просто ничего не загружено".
func (lr *LlamaCppRouter) handleRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"loaded_models": []interface{}{},
			"count":         0,
			"backends":      []string{},
		})
		return
	}

	// Структура одного элемента runtime-конфига (соответствует тому, что
	// отдаёт cppworker /api/v1/cppworker/config/runtime + добавляем backend).
	type runtimeModel struct {
		Name               string                 `json:"name"`
		Path               string                 `json:"path,omitempty"`
		State              string                 `json:"state,omitempty"`
		Architecture       string                 `json:"architecture,omitempty"`
		NLayers            int                    `json:"n_layers"`
		NHeads             int                    `json:"n_heads"`
		NKvHeads           int                    `json:"n_kv_heads"`
		NEmbd              int                    `json:"n_embd"`
		NVocab             int                    `json:"n_vocab"`
		ContextSize        int                    `json:"context_size"`
		GGUFContextLength  int                    `json:"gguf_context_length,omitempty"`
		SizeBytes          int64                  `json:"size_bytes"`
		LoadedAt           string                 `json:"loaded_at,omitempty"`
		GPUCount           int                    `json:"gpu_count,omitempty"`
		GPULayers          int                    `json:"gpu_layers"`
		TensorSplit        map[string]interface{} `json:"tensor_split,omitempty"`
		BatchSize          int                    `json:"batch_size,omitempty"`
		FlashAttnType      int                    `json:"flash_attn_type,omitempty"`
		NUMA               bool                   `json:"numa,omitempty"`
		UseMmap            bool                   `json:"use_mmap,omitempty"`
		ActiveQueries      int                    `json:"active_queries,omitempty"`
		TotalQueries       int64                  `json:"total_queries,omitempty"`
		LastUsedAt         string                 `json:"last_used_at,omitempty"`
		Backend            string                 `json:"backend"`
	}

	// Параллельно опрашиваем все llama.cpp бэкенды.
	type backendResult struct {
		backendID string
		models    []runtimeModel
		err       error
	}

	results := make(chan backendResult, len(backends))
	var wg sync.WaitGroup
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, b := range backends {
		wg.Add(1)
		go func(bi backendInfo) {
			defer wg.Done()
			url := fmt.Sprintf("http://%s:%d/api/v1/cppworker/config/runtime", bi.host, bi.port)
			resp, err := httpClient.Get(url)
			if err != nil {
				results <- backendResult{backendID: bi.id, err: fmt.Errorf("http get: %w", err)}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				results <- backendResult{
					backendID: bi.id,
					err:       fmt.Errorf("status %d", resp.StatusCode),
				}
				return
			}
			var parsed struct {
				LoadedModels []runtimeModel `json:"loaded_models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				results <- backendResult{backendID: bi.id, err: fmt.Errorf("decode: %w", err)}
				return
			}
			// Проставляем backend в каждой модели для агрегации.
			for i := range parsed.LoadedModels {
				parsed.LoadedModels[i].Backend = bi.id
			}
			results <- backendResult{backendID: bi.id, models: parsed.LoadedModels}
		}(b)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Собираем и дедуплицируем по path (если модель загружена на нескольких
	// бэкендах с одинаковым path — берём первый, кто ответил).
	merged := make([]runtimeModel, 0, len(backends)*2)
	seen := make(map[string]bool)
	backendIDs := make([]string, 0, len(backends))
	failedBackends := make(map[string]error)

	for res := range results {
		backendIDs = append(backendIDs, res.backendID)
		if res.err != nil {
			failedBackends[res.backendID] = res.err
			logger.Get().Warnw("handleRuntimeConfig: fetch failed",
				"backend", res.backendID, "error", res.err)
			continue
		}
		for _, m := range res.models {
			key := m.Path
			if key == "" {
				key = m.Name + "@" + res.backendID
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, m)
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Backend != merged[j].Backend {
			return merged[i].Backend < merged[j].Backend
		}
		return merged[i].Name < merged[j].Name
	})

	logger.Get().Infow("handleRuntimeConfig: returning aggregated runtime config",
		"models", len(merged),
		"backends_queried", len(backendIDs),
		"backends_failed", len(failedBackends),
	)

	resp := map[string]interface{}{
		"loaded_models": merged,
		"count":         len(merged),
		"backends":      backendIDs,
	}
	if len(failedBackends) > 0 {
		failList := make([]map[string]string, 0, len(failedBackends))
		for id, err := range failedBackends {
			failList = append(failList, map[string]string{
				"backend": id,
				"error":   err.Error(),
			})
		}
		resp["failed_backends"] = failList
	}
	writeJSON(w, http.StatusOK, resp)
}