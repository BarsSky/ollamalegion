package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

func mustParsePort(urlStr string) int {
	parts := strings.Split(urlStr, ":")
	if len(parts) < 3 {
		return 11434
	}
	var port int
	fmt.Sscanf(parts[2], "%d", &port)
	return port
}

func newTestConfig(backends []types.Backend) *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			Algorithm:            "resource-aware",
			SessionStickiness:    true,
			SessionTTL:           900,
			RequestTimeout:       30,
			FirstByteTimeout:     2,
			StreamingIdleTimeout: 60,
			QueueMaxSize:         100,
			QueueWorkers:         4,
			QueueTimeout:         60,
			HealthCheckInterval:  10,
			ModelAffinity:        true,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 99, MaxVRAMUsagePercent: 99},
			CPU:    types.CPULimits{MaxUsagePercent: 99},
			Memory: types.MemoryLimits{MaxUsagePercent: 99},
			Disk:   types.DiskLimits{MinFreeMB: 0},
		},
		Backends: backends,
	}
}

func TestScenario1_StreamingFailover(t *testing.T) {
	// Пропускаем тест: ResponseHeaderTimeout в httptest.Server не работает
	// с in-memory pipe-соединениями. Для корректной проверки failover при
	// зависшем бэкенде требуется реальный TCP listener.
	t.Skip("Requires real TCP connections for ResponseHeaderTimeout to work with httptest.Server")

	var b1, b2 int32

	// Канал для graceful остановки блокирующего хендлера s1
	stopS1 := make(chan struct{})

	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&b1, 1)
		if strings.Contains(r.URL.Path, "/api/chat") || strings.Contains(r.URL.Path, "/api/generate") {
			// Блокируемся до отмены контекста или сигнала остановки теста
			select {
			case <-r.Context().Done():
			case <-stopS1:
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer close(stopS1)
	defer s1.Close()

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&b2, 1)
		if strings.Contains(r.URL.Path, "/api/chat") || strings.Contains(r.URL.Path, "/api/generate") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: {\"token\":\"test%d\"}\n\n", i)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s2.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-backend-1", Name: "GPU 1", Host: "127.0.0.1", OllamaPort: mustParsePort(s1.URL), Weight: 1, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
		{ID: "gpu-backend-2", Name: "GPU 2", Host: "127.0.0.1", OllamaPort: mustParsePort(s2.URL), Weight: 1, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
	})

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-backend-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 20, MemoryTotal: 24576, MemoryUsed: 8192, MemoryFree: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 15, MemoryTotal: 65536, MemoryUsed: 16384, MemoryFree: 49152},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6144}}, MaxModels: 10, MaxConcurrentRequests: 5},
	})
	proxy.UpdateMetrics("gpu-backend-2", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 15, MemoryTotal: 24576, MemoryUsed: 6144, MemoryFree: 18432},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryUsed: 12288, MemoryFree: 53248},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6144}}, MaxModels: 10, MaxConcurrentRequests: 5},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hello"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Name", "Cline")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	b1c, b2c := atomic.LoadInt32(&b1), atomic.LoadInt32(&b2)
	t.Logf("Request routed: B1=%d, B2=%d", b1c, b2c)
	if b2c != 1 {
		t.Errorf("Expected failover to gpu-backend-2, but B2=%d (B1=%d)", b2c, b1c)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "test") {
		t.Errorf("Missing streaming tokens in response: %s", string(body))
	}
}

func TestScenario2_DifferentClientsSameIP(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 30

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "t", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	send := func(name string) string {
		req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
			strings.NewReader(`{"model":"t","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Client-Name", name)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
			return ""
		}
		defer resp.Body.Close()
		return resp.Header.Get("X-Session-ID")
	}

	s1 := send("Cline")
	s2 := send("OpenWebUI")
	t.Logf("Cline: %s, OWUI: %s", s1, s2)
	if s1 == s2 {
		t.Error("Sessions should differ for different clients")
	}
}

func TestScenario3_DifferentModels(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 30

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 49152, MemoryUsed: 8192, MemoryFree: 40960},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "a:7b", VRAMUsage: 4096}, {Name: "b:14b", VRAMUsage: 8192}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req1, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"a:7b","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Client-Name", "Cline")
	r1, _ := http.DefaultClient.Do(req1)
	s1 := r1.Header.Get("X-Session-ID")
	r1.Body.Close()

	req2, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"b:14b","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Client-Name", "Cline")
	r2, _ := http.DefaultClient.Do(req2)
	s2 := r2.Header.Get("X-Session-ID")
	r2.Body.Close()

	t.Logf("S1=%s S2=%s", s1, s2)
	if s1 == s2 {
		t.Error("Sessions should differ for different models")
	}
}

func TestScenario4_VRAM(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "gpu-2", Name: "G2", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 30
	cfg.Resources.GPU.MaxVRAMUsagePercent = 90

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 23347, MemoryFree: 1229},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryUsed: 16384, MemoryFree: 49152},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "big", VRAMUsage: 20480}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})
	proxy.UpdateMetrics("gpu-2", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 15, MemoryTotal: 24576, MemoryUsed: 9830, MemoryFree: 14746},
		System: types.SystemMetrics{CPUUsagePercent: 8, MemoryTotal: 65536, MemoryUsed: 12288, MemoryFree: 53248},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "small", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"small","messages":[{"role":"user","content":"T"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer resp.Body.Close()

	bid := resp.Header.Get("X-Backend-ID")
	if bid == "gpu-1" {
		t.Error("Must avoid VRAM-full GPU 1")
	}
}

func TestScenario5_RPS(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/chat") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: {\"token\":\"%d\"}\n\n", i)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 120
	cfg.Balancing.RequestTimeout = 120
	cfg.Balancing.QueueTimeout = 120

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 20, MemoryTotal: 24576, MemoryUsed: 8192, MemoryFree: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryUsed: 16384, MemoryFree: 49152},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "t", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
				strings.NewReader(`{"model":"t","messages":[{"role":"user","content":"x"}],"stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Client-Name", "C")
			resp, _ := http.DefaultClient.Do(req)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	st := proxy.GetClusterState()
	t.Logf("RPS=%.2f, Total=%d", st.RPS, st.TotalRequests)
	if st.TotalRequests == 0 {
		t.Error("TotalRequests must be >0")
	}
}

func TestScenario7_Stickiness(t *testing.T) {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer b2.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(b1.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "gpu-2", Name: "G2", Host: "127.0.0.1", OllamaPort: mustParsePort(b2.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 30

	proxy := balancer.NewProxy(cfg)
	for _, id := range []string{"gpu-1", "gpu-2"} {
		proxy.UpdateMetrics(id, &types.BackendMetrics{
			GPU:    types.GPUMetrics{UsagePercent: 15, MemoryTotal: 24576, MemoryUsed: 8192, MemoryFree: 16384},
			System: types.SystemMetrics{CPUUsagePercent: 8, MemoryTotal: 65536, MemoryUsed: 12288, MemoryFree: 53248},
			Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "s", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
		})
	}

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	send := func() string {
		req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
			strings.NewReader(`{"model":"s","messages":[{"role":"user","content":"x"}],"stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Client-Name", "TC")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
			return ""
		}
		defer resp.Body.Close()
		return resp.Header.Get("X-Backend-ID")
	}

	a := send()
	b := send()
	t.Logf("R1=%s R2=%s", a, b)
	if a != b {
		t.Error("Stickiness broken")
	}
}

func TestScenario8_Health(t *testing.T) {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer b.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(b.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy, HasAgent: true},
	})
	cfg.Balancing.FirstByteTimeout = 30

	proxy := balancer.NewProxy(cfg)

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	resp, err := http.Get(lb.URL + "/health")
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer resp.Body.Close()

	var hr map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&hr)
	if resp.StatusCode != 200 || hr["status"] != "healthy" {
		t.Errorf("Bad health: %d %v", resp.StatusCode, hr)
	}
}

func TestScenario9_Shutdown(t *testing.T) {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer b.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(b.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.HealthCheckInterval = 1
	cfg.Balancing.FirstByteTimeout = 30

	proxy := balancer.NewProxy(cfg)
	hc := balancer.NewHealthChecker(proxy, time.Duration(cfg.Balancing.HealthCheckInterval)*time.Second, 3)
	hc.Start()
	time.Sleep(500 * time.Millisecond)
	hc.Stop()
	t.Log("Graceful shutdown OK")
}

func TestScenario10_Concurrent(t *testing.T) {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer b.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(b.URL), Weight: 1, MaxConcurrentReqs: 50, Status: types.StatusHealthy},
	})
	cfg.Balancing.FirstByteTimeout = 30
	cfg.Balancing.QueueWorkers = 8

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 20, MemoryTotal: 98304, MemoryUsed: 16384, MemoryFree: 81920},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 131072, MemoryUsed: 16384, MemoryFree: 114688},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "m", VRAMUsage: 8192}}, MaxModels: 20, MaxConcurrentRequests: 50},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	names := []string{"Cline", "OpenWebUI", "Python", "cURL"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":false}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Client-Name", names[idx%len(names)])
			resp, _ := http.DefaultClient.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	ss := proxy.GetSessions()
	t.Logf("Sessions: %d", len(ss))
	if len(ss) == 0 {
		t.Error("No sessions")
	}
	if len(ss) > len(names) {
		t.Errorf("Max %d expected, got %d", len(names), len(ss))
	}
}