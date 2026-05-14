package balancer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// ---------- Targeted routing endpoints ----------

func (or *OllamaRouter) handleShow(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	backendID := or.findBackendWithModel(model)
	if backendID == "" {
		backendID = or.selectAnyHealthy()
	}

	or.proxy.proxyRequest(w, r, backendID)
}

func (or *OllamaRouter) handleCreate(w http.ResponseWriter, r *http.Request) {
	backendID := or.selectBackendByResources(r)
	or.proxy.proxyRequest(w, r, backendID)
}

func (or *OllamaRouter) handlePull(w http.ResponseWriter, r *http.Request) {
	backendID := or.selectBackendByResources(r)
	or.proxy.proxyRequest(w, r, backendID)
}

func (or *OllamaRouter) handleDelete(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	targets := or.findBackendsWithModel(model)
	if len(targets) == 0 {
		http.Error(w, `{"error":"model not found on any backend"}`, http.StatusNotFound)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var wg sync.WaitGroup
	var firstResp *http.Response
	var firstErr error
	var mu sync.Mutex
	var gotResult bool

	for _, backendID := range targets {
		wg.Add(1)
		go func(id string, bodyCopy []byte) {
			defer wg.Done()
			cloneReq := r.Clone(r.Context())
			cloneReq.Body = io.NopCloser(bytes.NewBuffer(bodyCopy))
			resp, err := or.proxyHTTP(cloneReq, id)
			mu.Lock()
			defer mu.Unlock()
			if !gotResult && err == nil && resp != nil && resp.StatusCode == http.StatusOK {
				firstResp = resp
				gotResult = true
			} else if resp != nil {
				resp.Body.Close()
			}
			if firstErr == nil && err != nil {
				firstErr = err
			}
		}(backendID, bodyBytes)
	}
	wg.Wait()

	if firstResp != nil {
		copyResponse(w, firstResp)
		return
	}

	http.Error(w, `{"error":"failed to delete model"}`, http.StatusInternalServerError)
}

func (or *OllamaRouter) handleCopy(w http.ResponseWriter, r *http.Request) {
	body := or.readBody(r)
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	json.Unmarshal(body, &req)

	sourceBackend := or.findBackendWithModel(req.Source)
	if sourceBackend == "" {
		http.Error(w, `{"error":"source model not found"}`, http.StatusNotFound)
		return
	}

	r.Body = io.NopCloser(bytes.NewBuffer(body))
	or.proxy.proxyRequest(w, r, sourceBackend)
}

func (or *OllamaRouter) handlePush(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	backendID := or.findBackendWithModel(model)
	if backendID == "" {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		return
	}

	or.proxy.proxyRequest(w, r, backendID)
}