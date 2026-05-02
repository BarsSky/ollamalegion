// +build ignore

package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	data, err := os.ReadFile("proxy.go")
	if err != nil {
		fmt.Println("Error reading proxy.go:", err)
		os.Exit(1)
	}

	old := `// selectBackend - выбор бэкенда для запроса
func (p *Proxy) selectBackend(model string) string {
	p.mu.RLock()

	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModel(model); backend != "" {
			state := p.backends[backend]
			state.mu.Lock()
			active := state.ActiveReqs
			maxReqs := state.Backend.MaxConcurrentReqs
			state.mu.Unlock()

			loadRatio := 0.0
			if maxReqs > 0 {
				loadRatio = float64(active) / float64(maxReqs)
			}

			if loadRatio < 0.80 {
				p.mu.RUnlock()
				return backend
			}

			logger.Get().Warnw("model-affinity backend overloaded, trying resource-aware fallback",
				"backend", backend, "model", model,
				"load_ratio", loadRatio, "active", active, "max", maxReqs)
		}
	}
	p.mu.RUnlock()

	backend := p.selectByResources()
	if backend == "" {
		logger.Get().Warnw("no available backends - balancer waiting for recovery")
	}
	return backend
}`

	new := `// selectBackend - выбор бэкенда для запроса (5-этапный алгоритм)
func (p *Proxy) selectBackend(model string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// 1. Model Affinity (LOADED only)
	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModel(model); backend != "" {
			state := p.backends[backend]
			state.mu.Lock()
			active := state.ActiveReqs
			maxReqs := state.Backend.MaxConcurrentReqs
			state.mu.Unlock()

			loadRatio := 0.0
			if maxReqs > 0 { loadRatio = float64(active) / float64(maxReqs) }
			if loadRatio < 0.80 { return backend }

			logger.Get().Warnw("model-affinity backend overloaded, trying fallback",
				"backend", backend, "model", model, "load_ratio", loadRatio)
		}
	}

	// 2. Model Warming (WARMING_UP + ETA < threshold)
	if warmingBackend := p.findWarmingBackendForModelUnsafe(model); warmingBackend != "" {
		state := p.backends[warmingBackend]
		state.mu.Lock()
		ws, exists := state.WarmingUpModels[model]
		state.mu.Unlock()

		if exists {
			eta := time.Until(ws.EstimatedReadyAt)
			syncTimeout := 30 * time.Second
			if p.config.Balancing.SyncModelLoad.Timeout != "" {
				if d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil { syncTimeout = d }
			}
			if eta > 0 && eta < syncTimeout {
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(warmingBackend, model) { return warmingBackend }
					time.Sleep(500 * time.Millisecond)
				}
				logger.Get().Warnw("model warmup timeout", "backend", warmingBackend, "model", model)
			}
		}
	}

	// 3. Sync Model Load (запуск загрузки с таймаутом)
	if p.config.Balancing.SyncModelLoad.Enabled {
		freeBackend := p.findFreeBackendForModelUnsafe(model)
		if freeBackend != "" {
			state := p.backends[freeBackend]
			p.warmupModel(freeBackend, state.Backend.Host, state.Backend.OllamaPort, model)
			syncTimeout := 30 * time.Second
			if p.config.Balancing.SyncModelLoad.Timeout != "" {
				if d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil { syncTimeout = d }
			}
			start := time.Now()
			for time.Since(start) < syncTimeout {
				if p.checkModelReadyUnsafe(freeBackend, model) { return freeBackend }
				time.Sleep(500 * time.Millisecond)
			}
			logger.Get().Warnw("sync model load timeout", "backend", freeBackend, "model", model)
		}
	}

	// 4. selectByResources (scoring v2)
	return p.selectByResources()
}`

	if !strings.Contains(string(data), old) {
		fmt.Println("OLD_CODE_NOT_FOUND_in_file")
		// Show what's at line 609-613 to debug
		lines := strings.Split(string(data), "\n")
		for i := 608; i < 615 && i < len(lines); i++ {
			fmt.Printf("Line %d: %q\n", i+1, lines[i])
		}
		os.Exit(1)
	}

	result := strings.Replace(string(data), old, new, 1)
	err = os.WriteFile("proxy.go", []byte(result), 0644)
	if err != nil {
		fmt.Println("Error writing proxy.go:", err)
		os.Exit(1)
	}
	fmt.Println("OK: selectBackend replaced successfully")
}