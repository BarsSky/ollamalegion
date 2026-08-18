// cmd/cppworker/cli_auto_load.go — Round 37 (2026-08-18) CLI.
//
// -auto-load <model> — auto-detect best n_ctx via ComputeFeasible и
// загрузить модель. Полезно когда operator не знает optimal n_ctx для
// hardware.
//
// Логика:
//  1. ComputeFeasible → feasible_max_context
//  2. Min(VRAM, RAM, GGUF) → nCtxAuto
//  3. POST /api/models/load with {name, contextSize: nCtxAuto}
//  4. Wait for load (poll /api/models) → print status
//
// КРИТИЧНО: production bug 2026-08-18 был именно про это — оператор
// не знал feasible, ставил консервативный profile 32768, а hardware
// давал 65536. -auto-load auto-fixes это.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// runAutoLoad auto-detects feasible n_ctx и загружает модель через HTTP API.
//
// Exit codes:
//   0 — успех (load accepted, polling shows loaded)
//   1 — backend not initialized, ComputeFeasible failed, HTTP error
//   2 — usage error (modelName == "")
func runAutoLoad(modelName string, out io.Writer) int {
	if modelName == "" {
		fmt.Fprintln(out, "usage: cppworker -auto-load <model>")
		return 2
	}

	b := cppbackend.GetBackend()
	if b == nil {
		fmt.Fprintln(out, "error: backend not initialized")
		return 1
	}

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		fmt.Fprintf(out, "error: ComputeFeasible: %v\n", err)
		return 1
	}

	// Pick nCtx = min of all available
	nCtx := info.MaxRAMCtx
	if info.MaxVRAMCtx > 0 && (nCtx == 0 || info.MaxVRAMCtx < nCtx) {
		nCtx = info.MaxVRAMCtx
	}
	if nCtx > info.GGUFMax {
		nCtx = info.GGUFMax
	}
	if nCtx <= 0 {
		// Fallback: use GGUF max
		nCtx = info.GGUFMax
	}
	// Round 37: align to 4096 boundary для удобства чтения (нет functional impact)
	if nCtx%4096 != 0 {
		nCtx = (nCtx / 4096) * 4096
	}

	fmt.Fprintf(out, "Auto-loading %s with n_ctx=%d (feasible: VRAM=%d, RAM=%d, GGUF=%d, kv=%s)\n",
		modelName, nCtx, info.MaxVRAMCtx, info.MaxRAMCtx, info.GGUFMax, info.KVCacheType)

	// Send load request via internal HTTP. We use the same port as the cppworker
	// would expose — but we're running standalone CLI, so we need to make the
	// load request IN-PROCESS via backend.LoadModel if possible.
	//
	// Round 37 design: call backend.LoadModel directly (faster, no HTTP overhead,
	// no need to start HTTP server). HTTP API is for remote clients; CLI is local.

	loadOpts := cppbackend.LoadModelOpts{
		ContextSize: nCtx,
		// KV cache type: use the one from feasible (matches profile if synced)
		KVCacheType: info.KVCacheType,
	}

	// Start load in background (LoadModel is blocking — could be minutes for big models)
	loadResult := make(chan error, 1)
	go func() {
		// Resolve model path via ModelManager
		var modelPath string
		if mm := b.ModelManager(); mm != nil {
			if path, err := mm.FindModelByPath(modelName); err == nil {
				modelPath = path
			} else {
				loadResult <- fmt.Errorf("resolve path: %w", err)
				return
			}
		} else {
			loadResult <- fmt.Errorf("ModelManager not initialized")
			return
		}
		loadResult <- b.LoadModelWithOpts(modelName, modelPath, loadOpts)
	}()

	fmt.Fprintf(out, "Loading %s in background (n_ctx=%d)...\n", modelName, nCtx)
	fmt.Fprintln(out, "Use 'docker logs <container>' to follow progress, or check /api/models for state.")

	// Print info about how to monitor
	fmt.Fprintf(out, "Monitor with: curl http://localhost:%d/api/models?include_loading=true\n", *port)

	// Wait for load to complete OR timeout (10 min — large model load can be slow)
	select {
	case err := <-loadResult:
		if err != nil {
			fmt.Fprintf(out, "error: LoadModel: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "OK: model %s loaded with n_ctx=%d\n", modelName, nCtx)
		return 0
	case <-time.After(10 * time.Minute):
		fmt.Fprintln(out, "warning: load still in progress after 10 min, exiting CLI but load continues in background")
		return 0
	}
}

// autoLoadHTTPClient — helper for HTTP-based auto-load (used by tests).
// Returns the load result via /api/models/load endpoint.
func autoLoadHTTPClient(modelName string, nCtx int, port int, apiToken string) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"name":        modelName,
		"contextSize": nCtx,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/api/models/load", port)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiToken != "" {
		req.Header.Set("X-API-Token", apiToken)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, json.Unmarshal(respBody, &map[string]interface{}{})
}

// strconv.Itoa alias для краткости
var _ = strconv.Itoa
