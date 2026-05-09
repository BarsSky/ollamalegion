package virtualmodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ---------- Тесты Registry ----------

func TestRegistry_NewAndDefault(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry() returned nil")
	}
	if r.IsEnabled() {
		t.Error("registry should be disabled by default")
	}
	if len(r.List()) != 0 {
		t.Error("registry should have 0 models by default")
	}
}

func TestRegistry_EnableDisable(t *testing.T) {
	r := NewRegistry()
	r.SetEnabled(true)
	if !r.IsEnabled() {
		t.Error("registry should be enabled after SetEnabled(true)")
	}
	r.SetEnabled(false)
	if r.IsEnabled() {
		t.Error("registry should be disabled after SetEnabled(false)")
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	cfg := types.VirtualModelConfig{
		Name:        "test-model",
		Description: "test description",
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	err := r.Register(cfg)
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	vm := r.Get("test-model")
	if vm == nil {
		t.Fatal("Get() returned nil for registered model")
	}
	if vm.GetName() != "test-model" {
		t.Errorf("GetName() = %q, want %q", vm.GetName(), "test-model")
	}
}

func TestRegistry_Get_NotExists(t *testing.T) {
	r := NewRegistry()
	vm := r.Get("nonexistent")
	if vm != nil {
		t.Error("Get() should return nil for nonexistent model")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	cfg := types.VirtualModelConfig{
		Name: "to-delete",
		Coordination: types.CoordinationConfig{
			Mode: "sequential",
		},
	}
	_ = r.Register(cfg)

	if !r.IsVirtualModel("to-delete") {
		t.Error("model should exist before unregister")
	}

	r.Unregister("to-delete")
	if r.IsVirtualModel("to-delete") {
		t.Error("model should not exist after unregister")
	}
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	r.Register(types.VirtualModelConfig{Name: "m1", Coordination: types.CoordinationConfig{Mode: "sequential"}})
	r.Register(types.VirtualModelConfig{Name: "m2", Coordination: types.CoordinationConfig{Mode: "sequential"}})
	r.Register(types.VirtualModelConfig{Name: "m3", Coordination: types.CoordinationConfig{Mode: "sequential"}})

	models := r.List()
	if len(models) != 3 {
		t.Errorf("List() returned %d models, want 3", len(models))
	}
}

func TestRegistry_IsVirtualModel(t *testing.T) {
	r := NewRegistry()
	r.Register(types.VirtualModelConfig{Name: "exists", Coordination: types.CoordinationConfig{Mode: "sequential"}})

	if !r.IsVirtualModel("exists") {
		t.Error("IsVirtualModel('exists') should be true")
	}
	if r.IsVirtualModel("missing") {
		t.Error("IsVirtualModel('missing') should be false")
	}
}

// ---------- Тесты VirtualModel базовые ----------

func TestVirtualModel_NewAndGetters(t *testing.T) {
	cfg := types.VirtualModelConfig{
		Name:        "vm-test",
		Description: "a virtual model for testing",
		Slices: []types.ModelSliceConfig{
			{ID: "s1", ModelName: "model-a", Ordinal: 1, TargetBackends: []string{"127.0.0.1:11434"}},
			{ID: "s2", ModelName: "model-b", Ordinal: 2, TargetBackends: []string{"127.0.0.1:11435"}},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	vm := NewVirtualModel(cfg)
	if vm == nil {
		t.Fatal("NewVirtualModel() returned nil")
	}
	if vm.GetName() != "vm-test" {
		t.Errorf("GetName() = %q, want %q", vm.GetName(), "vm-test")
	}
	if vm.GetActiveJobs() != 0 {
		t.Errorf("GetActiveJobs() = %d, want 0", vm.GetActiveJobs())
	}

	modelName := vm.GetSliceModelName("s1")
	if modelName != "model-a" {
		t.Errorf("GetSliceModelName('s1') = %q, want %q", modelName, "model-a")
	}

	modelName = vm.GetSliceModelName("nonexistent")
	if modelName != "" {
		t.Errorf("GetSliceModelName('nonexistent') = %q, want ''", modelName)
	}

	slices := vm.GetSlices()
	if len(slices) != 2 {
		t.Errorf("GetSlices() returned %d slices, want 2", len(slices))
	}
}

// ---------- Тесты вспомогательных функций ----------

func TestSortSlices(t *testing.T) {
	slices := []types.ModelSliceConfig{
		{ID: "c", Ordinal: 3},
		{ID: "a", Ordinal: 1},
		{ID: "b", Ordinal: 2},
	}

	sorted := sortSlices(slices)

	if len(sorted) != 3 {
		t.Fatalf("sortSlices returned %d items, want 3", len(sorted))
	}
	if sorted[0].ID != "a" || sorted[1].ID != "b" || sorted[2].ID != "c" {
		t.Errorf("sortSlices order = %v, want [a b c]", idsOf(sorted))
	}
}

func idsOf(slices []types.ModelSliceConfig) []string {
	ids := make([]string, len(slices))
	for i, s := range slices {
		ids[i] = s.ID
	}
	return ids
}

func TestParseBackendID(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"127.0.0.1:11434", "127.0.0.1", 11434, false},
		{"192.168.1.1:8080", "192.168.1.1", 8080, false},
		{"localhost:11434", "localhost", 11434, false},
		{"10.0.0.1", "10.0.0.1", 11434, false},
		{"host:port", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			host, port, err := parseBackendID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseBackendID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if host != tt.wantHost {
				t.Errorf("parseBackendID() host = %q, want %q", host, tt.wantHost)
			}
			if port != tt.wantPort {
				t.Errorf("parseBackendID() port = %d, want %d", port, tt.wantPort)
			}
		})
	}
}

func TestSelectBackendForSlice_Empty(t *testing.T) {
	_, _, err := selectBackendForSlice([]string{}, nil)
	if err == nil {
		t.Error("selectBackendForSlice with empty backends should return error")
	}
}

func TestSelectBackendForSlice_NoLoadFn(t *testing.T) {
	host, port, err := selectBackendForSlice([]string{"10.0.0.1:12345"}, nil)
	if err != nil {
		t.Fatalf("selectBackendForSlice() error = %v", err)
	}
	if host != "10.0.0.1" || port != 12345 {
		t.Errorf("selectBackendForSlice() = (%s, %d), want (10.0.0.1, 12345)", host, port)
	}
}

func TestSelectBackendForSlice_WithLoadFn(t *testing.T) {
	backends := []string{"backend-a:11434", "backend-b:11434", "backend-c:11434"}
	loads := map[string]float64{
		"backend-a:11434": 0.9,
		"backend-b:11434": 0.3,
		"backend-c:11434": 0.6,
	}
	loadFn := func(id string) float64 { return loads[id] }

	host, port, err := selectBackendForSlice(backends, loadFn)
	if err != nil {
		t.Fatalf("selectBackendForSlice() error = %v", err)
	}
	if host != "backend-b" || port != 11434 {
		t.Errorf("selectBackendForSlice() = (%s, %d), want (backend-b, 11434)", host, port)
	}
}

func TestSelectBackendForSlice_AllHighLoad(t *testing.T) {
	backends := []string{"a:11434", "b:11434"}
	loadFn := func(id string) float64 { return 1.0 }

	host, _, err := selectBackendForSlice(backends, loadFn)
	if err != nil {
		t.Fatalf("selectBackendForSlice() error = %v", err)
	}
	// Should return the first one if all equally loaded
	if host != "a" {
		t.Errorf("selectBackendForSlice() = %q, want 'a'", host)
	}
}

// ---------- Тесты ExecutePipeline ----------

// TestVirtualModel_ExecutePipeline_NoSlices проверяет ошибку при пустых срезах
func TestVirtualModel_ExecutePipeline_NoSlices(t *testing.T) {
	cfg := types.VirtualModelConfig{
		Name: "empty",
		Coordination: types.CoordinationConfig{
			Mode: "sequential",
		},
	}
	vm := NewVirtualModel(cfg)
	_, err := vm.ExecutePipeline([]byte(`{"prompt":"test"}`), nil)
	if err == nil {
		t.Fatal("expected error for empty slices")
	}
	if !strings.Contains(err.Error(), "no slices") {
		t.Errorf("error message = %q, should contain 'no slices'", err.Error())
	}
}

// TestVirtualModel_ExecutePipeline_SingleSlice проверяет pipeline с одним срезом
func TestVirtualModel_ExecutePipeline_SingleSlice(t *testing.T) {
	// Создаём мок-бэкенд
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверяем, что пришёл правильный запрос
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req["model"] != "test-slice-model" {
			http.Error(w, fmt.Sprintf("unexpected model: %v", req["model"]), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"response":    "hello from slice",
			"done":        true,
			"model":       req["model"],
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer backend.Close()

	// Извлекаем host:port из мок-сервера
	addr := backend.Listener.Addr().String()

	cfg := types.VirtualModelConfig{
		Name: "single-slice-vm",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "test-slice-model",
				Ordinal:        1,
				TargetBackends: []string{addr},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	result, err := vm.ExecutePipeline(input, nil)
	if err != nil {
		t.Fatalf("ExecutePipeline() error = %v", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["response"] != "hello from slice" {
		t.Errorf("response = %q, want 'hello from slice'", resp["response"])
	}
}

// TestVirtualModel_ExecutePipeline_TwoSlices проверяет pipeline с двумя срезами
func TestVirtualModel_ExecutePipeline_TwoSlices(t *testing.T) {
	var mu sync.Mutex
	callOrder := make([]string, 0)

	// Мок для первого среза (embeddings)
	slice1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callOrder = append(callOrder, "slice1")
		mu.Unlock()

		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response":    "embeddings from slice1",
			"done":        true,
			"model":       req["model"],
			"slice1_data": "processed",
		})
	}))
	defer slice1.Close()

	// Мок для второго среза (llm)
	slice2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callOrder = append(callOrder, "slice2")
		mu.Unlock()

		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)

		// Проверяем, что получили output от slice1 как input
		if req["slice1_data"] != "processed" {
			http.Error(w, "missing slice1_data", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response":    "final response from slice2",
			"done":        true,
			"model":       req["model"],
		})
	}))
	defer slice2.Close()

	cfg := types.VirtualModelConfig{
		Name: "two-slice-vm",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "embed",
				ModelName:      "nomic-embed-text",
				Ordinal:        1,
				TargetBackends: []string{slice1.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
			{
				ID:             "llm",
				ModelName:      "llama3.2",
				Ordinal:        2,
				TargetBackends: []string{slice2.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "test prompt"})
	result, err := vm.ExecutePipeline(input, nil)
	if err != nil {
		t.Fatalf("ExecutePipeline() error = %v", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["response"] != "final response from slice2" {
		t.Errorf("response = %q, want 'final response from slice2'", resp["response"])
	}

	// Проверяем порядок вызовов
	mu.Lock()
	defer mu.Unlock()
	if len(callOrder) != 2 || callOrder[0] != "slice1" || callOrder[1] != "slice2" {
		t.Errorf("call order = %v, want [slice1 slice2]", callOrder)
	}
}

// TestVirtualModel_ExecutePipeline_FallbackSkip проверяет skip при недоступности бэкенда
func TestVirtualModel_ExecutePipeline_FallbackSkip(t *testing.T) {
	availableBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "from available backend",
			"done":     true,
		})
	}))
	defer availableBackend.Close()

	cfg := types.VirtualModelConfig{
		Name: "skip-fallback",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "unavailable",
				ModelName:      "unreachable-model",
				Ordinal:        1,
				TargetBackends: []string{"127.0.0.1:1"}, // несуществующий порт
				FallbackMode:   "skip",
			},
			{
				ID:             "available",
				ModelName:      "working-model",
				Ordinal:        2,
				TargetBackends: []string{availableBackend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 2000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "test"})
	result, err := vm.ExecutePipeline(input, nil)
	if err != nil {
		t.Fatalf("ExecutePipeline() should succeed with skip, got error: %v", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["response"] != "from available backend" {
		t.Errorf("response = %q, want 'from available backend'", resp["response"])
	}
}

// TestVirtualModel_ExecutePipeline_FallbackAbort проверяет abort при недоступности бэкенда
func TestVirtualModel_ExecutePipeline_FallbackAbort(t *testing.T) {
	cfg := types.VirtualModelConfig{
		Name: "abort-fallback",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "unreachable-model",
				Ordinal:        1,
				TargetBackends: []string{"127.0.0.1:1"},
				FallbackMode:   "abort",
			},
			{
				ID:             "s2",
				ModelName:      "working-model",
				Ordinal:        2,
				TargetBackends: []string{"127.0.0.1:2"},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 1000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "test"})
	_, err := vm.ExecutePipeline(input, nil)
	if err == nil {
		t.Fatal("expected error for abort fallback, got nil")
	}
	if !strings.Contains(err.Error(), "no available backend") &&
		!strings.Contains(err.Error(), "failed") {
		t.Errorf("error should contain 'no available backend' or 'failed', got: %v", err)
	}
}

// TestVirtualModel_ExecutePipeline_BackendError проверяет ошибку при неудачном HTTP ответе
func TestVirtualModel_ExecutePipeline_BackendError(t *testing.T) {
	errorBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer errorBackend.Close()

	cfg := types.VirtualModelConfig{
		Name: "error-backend",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "failing-model",
				Ordinal:        1,
				TargetBackends: []string{errorBackend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 3000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	_, err := vm.ExecutePipeline(input, nil)
	if err == nil {
		t.Fatal("expected error for backend HTTP error")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error should contain 'failed', got: %v", err)
	}
}

// TestVirtualModel_ExecutePipeline_BackendErrorSkip проверяет skip при HTTP ошибке
func TestVirtualModel_ExecutePipeline_BackendErrorSkip(t *testing.T) {
	errorBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer errorBackend.Close()

	goodBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "recovery response",
			"done":     true,
		})
	}))
	defer goodBackend.Close()

	cfg := types.VirtualModelConfig{
		Name: "error-skip",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "failing",
				ModelName:      "failing-model",
				Ordinal:        1,
				TargetBackends: []string{errorBackend.Listener.Addr().String()},
				FallbackMode:   "skip",
			},
			{
				ID:             "working",
				ModelName:      "working-model",
				Ordinal:        2,
				TargetBackends: []string{goodBackend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 3000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	result, err := vm.ExecutePipeline(input, nil)
	if err != nil {
		t.Fatalf("ExecutePipeline() should succeed with skip, got error: %v", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["response"] != "recovery response" {
		t.Errorf("response = %q, want 'recovery response'", resp["response"])
	}
}

// TestVirtualModel_ExecutePipeline_WithOptions проверяет PipelineOption
func TestVirtualModel_ExecutePipeline_WithOptions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok",
			"done":     true,
		})
	}))
	defer backend.Close()

	cfg := types.VirtualModelConfig{
		Name: "opts-test",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "test-model",
				Ordinal:        1,
				TargetBackends: []string{backend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "test"})

	loadFnInvoked := false
	loadFn := func(id string) float64 {
		loadFnInvoked = true
		return 0.5
	}

	requestID := "custom-request-id"
	result, err := vm.ExecutePipeline(input, nil,
		WithRequestID(requestID),
		WithBackendLoadFn(loadFn),
	)
	if err != nil {
		t.Fatalf("ExecutePipeline() error = %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}

	if !loadFnInvoked {
		t.Error("loadFn should have been invoked")
	}

	// Проверяем, что активная задача была зарегистрирована и очищена
	time.Sleep(50 * time.Millisecond)
	if vm.GetActiveJobs() != 0 {
		t.Errorf("GetActiveJobs() = %d, want 0 after completion", vm.GetActiveJobs())
	}

	// Проверяем, что активная задача была в инфо
	infos := vm.GetActiveJobInfos()
	if len(infos) != 0 {
		t.Errorf("GetActiveJobInfos() = %d items, want 0 after completion", len(infos))
	}
}

// ---------- Тесты Router ----------

// TestRouter_Route проверяет маршрутизацию через виртуальную модель
func TestRouter_Route(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "routed correctly",
			"done":     true,
		})
	}))
	defer backend.Close()

	registry := NewRegistry()
	registry.SetEnabled(true)
	registry.Register(types.VirtualModelConfig{
		Name: "routed-model",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "actual-model",
				Ordinal:        1,
				TargetBackends: []string{backend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	})

	router := NewRouter(registry)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	result, handled, err := router.Route("routed-model", input, nil)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if !handled {
		t.Error("Route() should return handled=true")
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
}

// TestRouter_Route_NotVirtual проверяет, что Router пропускает не-виртуальные модели
func TestRouter_Route_NotVirtual(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(true)
	router := NewRouter(registry)

	_, handled, err := router.Route("normal-model", []byte(`{"prompt":"test"}`), nil)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if handled {
		t.Error("Route() should return handled=false for non-virtual model")
	}
}

// TestRouter_Route_Disabled проверяет, что Router не обрабатывает запросы при выключенном registry
func TestRouter_Route_Disabled(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(false)
	registry.Register(types.VirtualModelConfig{
		Name: "vm",
		Coordination: types.CoordinationConfig{
			Mode: "sequential",
		},
	})

	router := NewRouter(registry)
	_, handled, err := router.Route("vm", []byte(`{"prompt":"test"}`), nil)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if handled {
		t.Error("Route() should return handled=false when registry disabled")
	}
}

// TestRouter_ListVirtualModels проверяет список виртуальных моделей
func TestRouter_ListVirtualModels(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(true)
	registry.Register(types.VirtualModelConfig{
		Name:        "vm1",
		Description: "first VM",
		Slices: []types.ModelSliceConfig{
			{ID: "s1", ModelName: "model-a", Ordinal: 1, TargetBackends: []string{"127.0.0.1:11434"}},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	})

	router := NewRouter(registry)
	list := router.ListVirtualModels()
	if len(list) != 1 {
		t.Fatalf("ListVirtualModels() returned %d items, want 1", len(list))
	}
	if list[0]["name"] != "vm1" {
		t.Errorf("name = %q, want 'vm1'", list[0]["name"])
	}
}

// TestRouter_ListVirtualModels_Disabled проверяет, что при выключенном реестре возвращается nil
func TestRouter_ListVirtualModels_Disabled(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(false)
	router := NewRouter(registry)
	list := router.ListVirtualModels()
	if list != nil {
		t.Error("ListVirtualModels() should return nil when disabled")
	}
}

// TestRouter_GetVirtualModelStatus проверяет получение статуса
func TestRouter_GetVirtualModelStatus(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(true)
	registry.Register(types.VirtualModelConfig{
		Name:        "status-vm",
		Description: "status test",
		Slices: []types.ModelSliceConfig{
			{ID: "s1", ModelName: "test-model", Ordinal: 1, TargetBackends: []string{"10.0.0.1:11434"}},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 10000,
		},
	})

	router := NewRouter(registry)
	status := router.GetVirtualModelStatus("status-vm")
	if status == nil {
		t.Fatal("GetVirtualModelStatus() returned nil")
	}
	if status["name"] != "status-vm" {
		t.Errorf("name = %q, want 'status-vm'", status["name"])
	}
	if status["enabled"] != true {
		t.Errorf("enabled = %v, want true", status["enabled"])
	}
	if status["activeJobs"] != 0 {
		t.Errorf("activeJobs = %v, want 0", status["activeJobs"])
	}
}

// TestRouter_GetVirtualModelStatus_NotExists проверяет статус для несуществующей модели
func TestRouter_GetVirtualModelStatus_NotExists(t *testing.T) {
	registry := NewRegistry()
	registry.SetEnabled(true)
	router := NewRouter(registry)
	status := router.GetVirtualModelStatus("nonexistent")
	if status != nil {
		t.Error("GetVirtualModelStatus() should return nil for nonexistent model")
	}
}

// TestRouter_GetSliceTarget проверяет получение целевого бэкенда для среза
func TestRouter_GetSliceTarget(t *testing.T) {
	registry := NewRegistry()
	router := NewRouter(registry)

	target := router.GetSliceTarget(types.ModelSliceConfig{
		ID:             "s1",
		ModelName:      "test",
		Ordinal:        1,
		TargetBackends: []string{"backend-a:11434"},
	})
	if target != "backend-a:11434" {
		t.Errorf("GetSliceTarget() = %q, want 'backend-a:11434'", target)
	}
}

// TestRouter_GetSliceTarget_Empty проверяет пустые бэкенды
func TestRouter_GetSliceTarget_Empty(t *testing.T) {
	registry := NewRegistry()
	router := NewRouter(registry)

	target := router.GetSliceTarget(types.ModelSliceConfig{
		ID: "s1", ModelName: "test", Ordinal: 1,
	})
	if target != "" {
		t.Errorf("GetSliceTarget() = %q, want ''", target)
	}
}

// TestRouter_GetSliceTarget_InvalidType проверяет неверный тип
func TestRouter_GetSliceTarget_InvalidType(t *testing.T) {
	registry := NewRegistry()
	router := NewRouter(registry)

	target := router.GetSliceTarget("not-a-slice-config")
	if target != "" {
		t.Errorf("GetSliceTarget() = %q, want ''", target)
	}
}

// TestRouter_GetRegistry проверяет геттер реестра
func TestRouter_GetRegistry(t *testing.T) {
	registry := NewRegistry()
	router := NewRouter(registry)

	if router.GetRegistry() != registry {
		t.Error("GetRegistry() should return the same registry instance")
	}
}

// ---------- Тест параллельного доступа (race condition) ----------

func TestVirtualModel_ConcurrentAccess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok",
			"done":     true,
		})
	}))
	defer backend.Close()

	cfg := types.VirtualModelConfig{
		Name: "concurrent-vm",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "test-model",
				Ordinal:        1,
				TargetBackends: []string{backend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 5000,
		},
	}

	vm := NewVirtualModel(cfg)
	var wg sync.WaitGroup
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})

	// Запускаем 10 конкурентных запросов
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := vm.ExecutePipeline(input, nil, WithRequestID(fmt.Sprintf("req-%d", id)))
			if err != nil {
				t.Errorf("request %d failed: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	// После всех запросов активных задач быть не должно
	if vm.GetActiveJobs() != 0 {
		t.Errorf("GetActiveJobs() = %d, want 0 after all requests", vm.GetActiveJobs())
	}
}

// ---------- Тест pipeline с параметрами ----------

func TestVirtualModel_ExecutePipeline_WithParams(t *testing.T) {
	var capturedReq map[string]interface{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedReq)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok",
			"done":     true,
		})
	}))
	defer backend.Close()

	cfg := types.VirtualModelConfig{
		Name: "params-test",
		Slices: []types.ModelSliceConfig{
			{
				ID:             "s1",
				ModelName:      "test-model",
				Ordinal:        1,
				TargetBackends: []string{backend.Listener.Addr().String()},
				FallbackMode:   "abort",
			},
		},
		Coordination: types.CoordinationConfig{
			Mode:      "sequential",
			TimeoutMs: 3000,
		},
	}

	vm := NewVirtualModel(cfg)
	input, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	params := map[string]string{
		"temperature": "0.7",
		"top_p":       "0.9",
	}

	_, err := vm.ExecutePipeline(input, params)
	if err != nil {
		t.Fatalf("ExecutePipeline() error = %v", err)
	}

	if capturedReq["temperature"] != "0.7" {
		t.Errorf("temperature param not propagated: %v", capturedReq["temperature"])
	}
	if capturedReq["model"] != "test-model" {
		t.Errorf("model = %v, want 'test-model'", capturedReq["model"])
	}
	if capturedReq["prompt"] != "hello" {
		t.Errorf("prompt = %v, want 'hello'", capturedReq["prompt"])
	}
}
