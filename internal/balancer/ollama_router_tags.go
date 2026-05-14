package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// OllamaTag — модель из ответа /api/tags
type OllamaTag struct {
	Name       string                 `json:"name"`
	Model      string                 `json:"model,omitempty"`
	ModifiedAt time.Time              `json:"modified_at"`
	Size       int64                  `json:"size"`
	Digest     string                 `json:"digest"`
	Details    map[string]interface{} `json:"details"`
}

// OllamaTagsResponse — ответ /api/tags
type OllamaTagsResponse struct {
	Models []OllamaTag `json:"models"`
}

// OllamaProcess — запущенная модель из /api/ps
type OllamaProcess struct {
	Name      string                 `json:"name"`
	Model     string                 `json:"model,omitempty"`
	Size      int64                  `json:"size"`
	Digest    string                 `json:"digest"`
	Details   map[string]interface{} `json:"details"`
	ExpiresAt time.Time              `json:"expires_at"`
	SizeVRAM  int64                  `json:"size_vram"`
}

// OllamaPSResponse — ответ /api/ps
type OllamaPSResponse struct {
	Models []OllamaProcess `json:"models"`
}

// OllamaVersionResponse — ответ /api/version
type OllamaVersionResponse struct {
	Version string `json:"version"`
}

// ---------- Read-only aggregation endpoints ----------

func (or *OllamaRouter) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := or.getHealthyBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
		return
	}

	var wg sync.WaitGroup
	results := make(chan []OllamaTag, len(backends))
	errors := make(chan error, len(backends))

	for _, backend := range backends {
		wg.Add(1)
		go func(b backendInfo) {
			defer wg.Done()
			tags, err := or.fetchTags(b.host, b.port)
			if err != nil {
				errors <- fmt.Errorf("backend %s: %v", b.id, err)
				return
			}
			results <- tags
		}(backend)
	}

	go func() {
		wg.Wait()
		close(results)
		close(errors)
	}()

	uniqueModels := make(map[string]OllamaTag)
	for tags := range results {
		for _, t := range tags {
			uniqueModels[t.Name] = t
		}
	}

	models := make([]OllamaTag, 0, len(uniqueModels))
	for _, m := range uniqueModels {
		models = append(models, m)
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})

	writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: models})
}

func (or *OllamaRouter) handlePS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := or.getHealthyBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaPSResponse{Models: []OllamaProcess{}})
		return
	}

	var wg sync.WaitGroup
	results := make(chan []OllamaProcess, len(backends))

	for _, backend := range backends {
		wg.Add(1)
		go func(b backendInfo) {
			defer wg.Done()
			procs, err := or.fetchPS(b.host, b.port)
			if err != nil {
				return
			}
			results <- procs
		}(backend)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allProcesses []OllamaProcess
	for procs := range results {
		allProcesses = append(allProcesses, procs...)
	}

	writeJSON(w, http.StatusOK, OllamaPSResponse{Models: allProcesses})
}

func (or *OllamaRouter) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"version":        "ollamalegion-1.0.0",
		"ollamaVersions": make(map[string]string),
	}

	backends := or.getHealthyBackends()
	if len(backends) > 0 {
		var wg sync.WaitGroup
		versions := make(map[string]string)
		var mu sync.Mutex

		for _, backend := range backends {
			wg.Add(1)
			go func(b backendInfo) {
				defer wg.Done()
				ver, err := or.fetchVersion(b.host, b.port)
				if err != nil {
					return
				}
				mu.Lock()
				versions[b.id] = ver
				mu.Unlock()
			}(backend)
		}
		wg.Wait()

		response["ollamaVersions"] = versions
	}

	writeJSON(w, http.StatusOK, response)
}

func (or *OllamaRouter) fetchTags(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Models, nil
}

func (or *OllamaRouter) fetchPS(host string, port int) ([]OllamaProcess, error) {
	url := fmt.Sprintf("http://%s:%d/api/ps", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaPSResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Models, nil
}

func (or *OllamaRouter) fetchVersion(host string, port int) (string, error) {
	url := fmt.Sprintf("http://%s:%d/api/version", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	return data.Version, nil
}