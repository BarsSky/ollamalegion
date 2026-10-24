//go:build llama_stub

// reload_payload_hints_r119_test.go — R119 (2026-10-15): n_ctx-перезагрузка не
// имеет права терять (и не имеет права молча перебивать) настройки оператора.
//
// ЖИВЫЕ ДЕФЕКТЫ (стенд), три независимых сборщика тела:
//
//  1. DoReload (nctx_reload.go) собирал тело С НУЛЯ — профиль в него не попадал:
//     kvCacheType не уходил (cppworker брал свой дефолт), parallel не уходил
//     (модель возвращалась к CPPWORKER_N_PARALLEL), enableReasoning не уходил
//     (перезагрузка молча выключала размышления).
//  2. load-fallback (reload → 404) собирал тело заново из четырёх ключей и терял
//     всё, что добавили хинты/стратегия.
//  3. executeAsyncReload жёстко зашивал kvCacheType=q4_0/gpuLayers=-2/useMmap=true:
//     явная настройка оператора (профиль q8_0) перебивалась константой.
//
// Тест падает до правки: все три места игнорируют хинты.
package balancer

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

// --- юнит-уровень -----------------------------------------------------------

// TestR119_ApplyReloadHintsToPayload — хинты оператора доезжают до тела reload.
func TestR119_ApplyReloadHintsToPayload(t *testing.T) {
	reasoning := true
	payload := map[string]interface{}{
		"name":        "m",
		"contextSize": 32768,
		"force":       true,
		"gpuLayers":   -2,
	}
	applyReloadHintsToPayload(payload, ReloadHints{
		KVCacheType:     "q8_0",
		Parallel:        3,
		EnableReasoning: &reasoning,
	})

	if got := payload["kvCacheType"]; got != "q8_0" {
		t.Errorf("kvCacheType = %v, want q8_0: без него cppworker берёт свой дефолт и модель "+
			"переезжает на другой тип KV-кэша", got)
	}
	if got := payload["parallel"]; got != 3 {
		t.Errorf("parallel = %v, want 3: без него cppworker берёт CPPWORKER_N_PARALLEL и число "+
			"слотов молча меняется", got)
	}
	if got := payload["enableReasoning"]; got != true {
		t.Errorf("enableReasoning = %v, want true: перезагрузка молча выключала размышления", got)
	}
	// contextPerSeq НАМЕРЕННО не отправляем на этом пути: cppworker при
	// contextPerSeq > 0 игнорирует contextSize (считает суммарное как
	// PAD256(per_seq) × слотов), и перезагрузка не достигла бы запрошенного окна.
	if _, present := payload["contextPerSeq"]; present {
		t.Errorf("contextPerSeq не должен уходить в тело n_ctx-reload (%v): он сделал бы "+
			"contextSize недействующим", payload["contextPerSeq"])
	}
	// Ничего лишнего не перетёрли.
	if payload["force"] != true || payload["contextSize"] != 32768 {
		t.Errorf("базовые поля тела изменены: %v", payload)
	}
}

// TestR119_ApplyReloadHintsToPayload_EmptyHintsNoop — без хинтов тело не
// меняется (старые стенды без профилей работают как раньше).
func TestR119_ApplyReloadHintsToPayload_EmptyHintsNoop(t *testing.T) {
	payload := map[string]interface{}{"name": "m", "contextSize": 4096}
	applyReloadHintsToPayload(payload, ReloadHints{})
	if len(payload) != 2 {
		t.Errorf("тело изменилось без хинтов: %v", payload)
	}
}

// TestR119_LoadBodyFromReloadPayload — fallback-тело сохраняет всё, кроме force.
func TestR119_LoadBodyFromReloadPayload(t *testing.T) {
	src := map[string]interface{}{
		"name":                "m",
		"contextSize":         65536,
		"force":               true,
		"gpuLayers":           -2,
		"kvCacheType":         "q4_0",
		"parallel":            3,
		"enableReasoning":     true,
		"overrideTensors":     []string{"a"},
		"overrideTensorBufts": []string{"b"},
	}
	out := loadBodyFromReloadPayload(src)

	if _, present := out["force"]; present {
		t.Error("force остался в теле /api/models/load: cppworker декодирует строго " +
			"(DisallowUnknownFields) и ответит 400 «unknown field force»")
	}
	for _, key := range []string{"name", "contextSize", "gpuLayers", "kvCacheType", "parallel", "enableReasoning", "overrideTensors", "overrideTensorBufts"} {
		if _, present := out[key]; !present {
			t.Errorf("ключ %q потерян на fallback-пути (раньше здесь перечислялись только "+
				"kvCacheType/gpuLayers/useMmap/flashAttn)", key)
		}
	}
	if _, present := src["force"]; !present {
		t.Error("исходное тело изменено (функция должна копировать, а не портить)")
	}
}

// TestR119_AsyncReloadLoadBody_ProfileBeatsHardcode — явная настройка оператора
// важнее исторических констант, но сами константы остаются fallback'ом.
func TestR119_AsyncReloadLoadBody_ProfileBeatsHardcode(t *testing.T) {
	reasoning := false
	body := asyncReloadLoadBody("m", 65536, ReloadHints{
		KVCacheType:     "q8_0",
		Parallel:        2,
		EnableReasoning: &reasoning,
	})
	if body["kvCacheType"] != "q8_0" {
		t.Errorf("kvCacheType = %v, want q8_0 (профиль): константа q4_0 молча перебивала "+
			"явную настройку оператора", body["kvCacheType"])
	}
	if body["parallel"] != 2 {
		t.Errorf("parallel = %v, want 2", body["parallel"])
	}
	if body["enableReasoning"] != false {
		t.Errorf("enableReasoning = %v, want false (явное значение профиля)", body["enableReasoning"])
	}
	// gpuLayers/useMmap остаются историческими (они не про намерение оператора,
	// а про влезание в VRAM) — проверяем, что их не потеряли.
	if body["gpuLayers"] != -2 || body["useMmap"] != true {
		t.Errorf("потеряны fallback-константы: gpuLayers=%v useMmap=%v", body["gpuLayers"], body["useMmap"])
	}

	// Без хинтов — ровно прежнее поведение (иначе на 8 ГБ можно потерять окно).
	bare := asyncReloadLoadBody("m", 65536, ReloadHints{})
	if bare["kvCacheType"] != "q4_0" || bare["gpuLayers"] != -2 || bare["useMmap"] != true {
		t.Errorf("без профиля тело изменилось: %v", bare)
	}
	if _, present := bare["parallel"]; present {
		t.Errorf("parallel не должен появляться без хинта: %v", bare["parallel"])
	}
}

// TestR119_ReloadHintsFor_ProfileCarriesParallelAndReasoning — хинты берут
// parallel/enableReasoning из профиля, а parallel — ещё и из загруженной модели.
func TestR119_ReloadHintsFor_ProfileCarriesParallelAndReasoning(t *testing.T) {
	p := admissionTestProxy(t, 4)
	reasoning := true
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma-4": {
			ContextLength:   32768,
			KVCacheType:     "q4_0",
			Parallel:        3,
			EnableReasoning: &reasoning,
		},
	}

	hints := p.reloadHintsFor("llama_adm", "gemma-4-E4B-it-Q4_K_M")
	if hints.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0", hints.KVCacheType)
	}
	if hints.Parallel != 3 {
		t.Errorf("Parallel = %d, want 3 (профиль): иначе перезагрузка меняет число слотов", hints.Parallel)
	}
	if hints.EnableReasoning == nil || !*hints.EnableReasoning {
		t.Errorf("EnableReasoning = %v, want true (профиль)", hints.EnableReasoning)
	}

	// Профиля нет — параллельность берём фактическую у загруженной модели.
	p2 := admissionTestProxy(t, 4)
	p2.config.LlamaCppModelProfiles = nil
	p2.metricsMgr.mu.Lock()
	p2.metricsMgr.llamaMetrics["llama_adm"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: "m", State: "loaded", ContextLength: 32768, Parallel: 2, KvCacheType: "q8_0",
		}},
	}
	p2.metricsMgr.mu.Unlock()
	h2 := p2.reloadHintsFor("llama_adm", "m")
	if h2.Parallel != 2 {
		t.Errorf("Parallel = %d, want 2 (фактическое у загруженной модели)", h2.Parallel)
	}
	if h2.EnableReasoning != nil {
		t.Errorf("EnableReasoning = %v, want nil (нет источника)", h2.EnableReasoning)
	}
}

// --- интеграция: preflight → async-перезагрузка ------------------------------

type asyncLoadLog struct {
	mu       sync.Mutex
	payloads []map[string]interface{}
}

func (l *asyncLoadLog) add(p map[string]interface{}) {
	l.mu.Lock()
	l.payloads = append(l.payloads, p)
	l.mu.Unlock()
}

func (l *asyncLoadLog) first() (map[string]interface{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.payloads) == 0 {
		return nil, false
	}
	return l.payloads[0], true
}

// TestR119_PreflightAsyncReload_CarriesProfileParams — сквозная проверка: запрос
// с большим num_ctx запускает async-перезагрузку, и в её теле видны настройки
// профиля (а не только жёсткие константы).
func TestR119_PreflightAsyncReload_CarriesProfileParams(t *testing.T) {
	const modelName = "gemma-4-E4B-it-Q4_K_M"
	captured := &asyncLoadLog{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models":        []interface{}{},
			"loaded_models": []interface{}{},
		})
	})
	mux.HandleFunc("/api/models/load", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		captured.add(payload)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":       "loading",
			"progress_url": "/api/models/load/progress?model=" + modelName,
		})
	})
	mux.HandleFunc("/api/models/load/progress", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"loading"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	hostOnly := host[:strings.LastIndex(host, ":")]
	port := 0
	_, _ = fmt.Sscanf(host[strings.LastIndex(host, ":")+1:], "%d", &port)

	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "test-backend", Type: types.BackendTypeLlamaCpp,
			Host: hostOnly, CppWorkerPort: port, OllamaPort: port,
		}},
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			modelName: {
				ContextLength: 32768,
				KVCacheType:   "q8_0",
				Parallel:      3,
			},
		},
	}
	p := newProxyWithCleanup(t, cfg)
	reasoning := true
	prof := p.config.LlamaCppModelProfiles[modelName]
	prof.EnableReasoning = &reasoning
	p.config.LlamaCppModelProfiles[modelName] = prof
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["test-backend"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: modelName, State: "loaded", ContextLength: 4096, Parallel: 1,
		}},
	}
	p.metricsMgr.mu.Unlock()

	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"x"}],"options":{"num_ctx":16384}}`)
	_, _, _, _ = p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// Асинхронная перезагрузка идёт в горутине — ждём появления тела.
	var payload map[string]interface{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := captured.first(); ok {
			payload = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if payload == nil {
		t.Fatal("async-перезагрузка не отправила тело на /api/models/load")
	}

	if payload["kvCacheType"] != "q8_0" {
		t.Errorf("kvCacheType = %v, want q8_0 (профиль): до правки здесь всегда стояла "+
			"константа q4_0", payload["kvCacheType"])
	}
	if payload["parallel"] != float64(3) {
		t.Errorf("parallel = %v, want 3 (профиль): до правки поля не было вовсе", payload["parallel"])
	}
	if payload["enableReasoning"] != true {
		t.Errorf("enableReasoning = %v, want true: до правки поля не было, и перезагрузка "+
			"выключала размышления", payload["enableReasoning"])
	}
	if payload["contextSize"] != float64(16384) {
		t.Errorf("contextSize = %v, want 16384 (цель перезагрузки)", payload["contextSize"])
	}
}

// --- прогресс: warmupLlamaCppModel -------------------------------------------

// TestR119_WarmupLoad_CarriesProfileParams — прогрев (POST /load) обязан
// поднимать модель с настройками профиля. До правки тело состояло из
// name/path/contextSize/gpuLayers, поэтому kvCacheType не уходил вовсе (cppworker
// брал свой дефолт — «модель загружена как f16») и parallel терялся.
func TestR119_WarmupLoad_CarriesProfileParams(t *testing.T) {
	const modelName = "gemma-4-E4B-it-Q4_K_M"
	var (
		mu       sync.Mutex
		captured map[string]interface{}
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/models/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[],"count":0}`))
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		captured = payload
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"loaded"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host := hostPort[:strings.LastIndex(hostPort, ":")]
	port := 0
	_, _ = fmt.Sscanf(hostPort[strings.LastIndex(hostPort, ":")+1:], "%d", &port)

	reasoning := true
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "test-backend", Type: types.BackendTypeLlamaCpp,
			Host: host, CppWorkerPort: port, OllamaPort: port,
		}},
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			modelName: {
				ContextLength:   32768,
				KVCacheType:     "q4_0",
				Parallel:        2,
				EnableReasoning: &reasoning,
			},
		},
	}
	p := newProxyWithCleanup(t, cfg)
	p.warmupLlamaCppModel("test-backend", host, port, modelName, warmupOptions{})

	var payload map[string]interface{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		payload = captured
		mu.Unlock()
		if payload != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if payload == nil {
		t.Fatal("прогрев не отправил тело на /load")
	}
	if payload["kvCacheType"] != "q4_0" {
		t.Errorf("kvCacheType = %v, want q4_0 (профиль): без него cppworker берёт свой дефолт "+
			"(f16/q8_0) и модель поднимается «не как настроено»", payload["kvCacheType"])
	}
	if payload["parallel"] != float64(2) {
		t.Errorf("parallel = %v, want 2 (профиль)", payload["parallel"])
	}
	if payload["enableReasoning"] != true {
		t.Errorf("enableReasoning = %v, want true (профиль)", payload["enableReasoning"])
	}
	if payload["name"] != modelName {
		t.Errorf("name = %v, want %s", payload["name"], modelName)
	}
}
