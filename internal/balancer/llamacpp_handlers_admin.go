package balancer

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ---------- Model operation endpoints ----------

func (lr *LlamaCppRouter) handleShow(w http.ResponseWriter, r *http.Request) {
	// Round 22 (2026-08-03): /api/show — read/mgmt endpoint, skip warmup и
	// VRAM check. Используем findModelOnAnyBackendNoVRAMCheck.
	model := lr.proxy.parseRequestBody(r).Model
	backendID := lr.proxy.findModelOnAnyBackendNoVRAMCheck(model, types.BackendTypeLlamaCpp)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleCreate(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handlePull(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleDelete(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleCopy(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handlePush(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

// ConvertOpenAIResponseToOllama конвертирует ответ llama.cpp (OpenAI-формат) в Ollama формат
func ConvertOpenAIResponseToOllama(lcppResp *LlamaCppChatResponse) map[string]interface{} {
	ollamaResp := map[string]interface{}{
		"model":      lcppResp.Model,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}

	if len(lcppResp.Choices) > 0 {
		choice := lcppResp.Choices[0]
		// Приоритет: Message → Delta
		if choice.Message.Content != "" || len(choice.Message.ToolCalls) > 0 {
			msg := map[string]interface{}{
				"role":    choice.Message.Role,
				"content": choice.Message.Content,
			}
			if len(choice.Message.ToolCalls) > 0 {
				msg["tool_calls"] = choice.Message.ToolCalls
			}
			ollamaResp["message"] = msg
		}
		if choice.Delta.Content != "" || len(choice.Delta.ToolCalls) > 0 {
			msg := map[string]interface{}{
				"role":    choice.Delta.Role,
				"content": choice.Delta.Content,
			}
			if len(choice.Delta.ToolCalls) > 0 {
				msg["tool_calls"] = choice.Delta.ToolCalls
			}
			ollamaResp["message"] = msg
		}
		ollamaResp["done"] = choice.FinishReason == "stop" || choice.FinishReason == "tool_calls"
		if choice.FinishReason != "" {
			ollamaResp["done_reason"] = choice.FinishReason
		}
	}

	if lcppResp.Usage.TotalTokens > 0 {
		ollamaResp["eval_count"] = lcppResp.Usage.CompletionTokens
		ollamaResp["prompt_eval_count"] = lcppResp.Usage.PromptTokens
	}

	return ollamaResp
}

// formatLlamaCppModelsList строит список моделей из всех llama.cpp бэкендов
func (lr *LlamaCppRouter) formatLlamaCppModelsList() map[string]interface{} {
	var wg sync.WaitGroup
	type modelInfo struct {
		Name       string `json:"name"`
		ModifiedAt string `json:"modified_at"`
		Size       int64  `json:"size"`
	}

	backends := lr.getLlamaCppBackends()
	results := make(chan modelInfo, len(backends)*10)

	for _, b := range backends {
		wg.Add(1)
		go func(bi backendInfo) {
			defer wg.Done()
			lr.proxy.metricsMgr.mu.RLock()
			metrics, ok := lr.proxy.metricsMgr.metrics[bi.id]
			lr.proxy.metricsMgr.mu.RUnlock()
			if !ok {
				return
			}
			for _, m := range metrics.LlamaCpp.LoadedModels {
				results <- modelInfo{
					Name:       m.Name,
					ModifiedAt: time.Now().Format(time.RFC3339),
					Size:       0,
				}
			}
		}(b)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	unique := make(map[string]modelInfo)
	for mi := range results {
		if _, exists := unique[mi.Name]; !exists {
			unique[mi.Name] = mi
		}
	}

	models := make([]modelInfo, 0, len(unique))
	for _, m := range unique {
		models = append(models, m)
	}

	return map[string]interface{}{
		"models": models,
	}
}

// encodeJSON helper для encode
func encodeJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
