//go:build llama_stub

// load_defaults_r83_test.go — R83 (2026-09-30).
//
// ЗАДАЧА ОПЕРАТОРА: «тот конфиг, что выступает в роли инициализации при старте
// балансера, сделать редактируемым со стороны WebUI: если модель была добавлена
// или выкачана новая — это позволит прописать в конфиг новые параметры под новую
// модель».
//
// Здесь фиксируется контракт:
//  1. PUT /api/v1/cppworker/load-defaults пишет значения в
//     config.defaultModelProfile (а не «в никуда»);
//  2. частичное обновление НЕ обнуляет остальные поля (PATCH-семантика, как у
//     model-profiles — иначе форма WebUI стирала бы настройки);
//  3. GET отдаёт то, что записали, и указывает ИСТОЧНИК каждого значения;
//  4. загрузка модели без своего профиля берёт эти значения (проверяется через
//     applyProfileLoadParams — ту же функцию, что вызывает executeLlamaCppLoad).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

func newLoadDefaultsServer(t *testing.T) (*Server, *types.LoadBalancerConfig) {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil), cfg
}

func doLoadDefaults(t *testing.T, s *Server, method, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, "/api/v1/cppworker/load-defaults", rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleLoadDefaults(w, req)

	var parsed map[string]interface{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("ответ не JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w, parsed
}

// TestR83_LoadDefaults_PutWritesToConfig — сохранение из WebUI попадает в
// конфиг балансера (defaultModelProfile), а не теряется.
func TestR83_LoadDefaults_PutWritesToConfig(t *testing.T) {
	s, cfg := newLoadDefaultsServer(t)

	w, _ := doLoadDefaults(t, s, http.MethodPut,
		`{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"kvCacheType":"q4_0","parallel":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", w.Code, w.Body.String())
	}
	if cfg.DefaultModelProfile == nil {
		t.Fatal("defaultModelProfile не записан в конфиг — настройки «по умолчанию» " +
			"не переживут рестарт и не применятся к новым моделям")
	}
	if cfg.DefaultModelProfile.ContextLength != 16384 ||
		cfg.DefaultModelProfile.Parallel != 2 ||
		cfg.DefaultModelProfile.KVCacheType != "q4_0" {
		t.Errorf("записано не то: %+v", *cfg.DefaultModelProfile)
	}
}

// TestR83_LoadDefaults_PartialUpdateKeepsOtherFields — форма может прислать
// только часть полей: остальные обязаны сохраниться.
func TestR83_LoadDefaults_PartialUpdateKeepsOtherFields(t *testing.T) {
	s, cfg := newLoadDefaultsServer(t)

	if w, _ := doLoadDefaults(t, s, http.MethodPut,
		`{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"kvCacheType":"q8_0","parallel":2}`); w.Code != http.StatusOK {
		t.Fatalf("первый PUT: %d %s", w.Code, w.Body.String())
	}
	// WebUI прислал только contextLength — остальное должно остаться.
	if w, _ := doLoadDefaults(t, s, http.MethodPut, `{"contextLength":8192}`); w.Code != http.StatusOK {
		t.Fatalf("второй PUT: %d %s", w.Code, w.Body.String())
	}

	got := cfg.DefaultModelProfile
	if got == nil {
		t.Fatal("профиль потерян после частичного обновления")
	}
	if got.ContextLength != 8192 {
		t.Errorf("contextLength = %d, want 8192 (новое значение)", got.ContextLength)
	}
	if got.Parallel != 2 || got.KVCacheType != "q8_0" || got.BatchSize != 512 {
		t.Errorf("частичное обновление обнулило поля: parallel=%d kv=%q batch=%d (want 2/q8_0/512)",
			got.Parallel, got.KVCacheType, got.BatchSize)
	}
}

// TestR83_LoadDefaults_GetReportsValueAndSource — оператор должен видеть не
// только значение, но и откуда оно взялось.
func TestR83_LoadDefaults_GetReportsValueAndSource(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	if w, _ := doLoadDefaults(t, s, http.MethodPut,
		`{"contextLength":16384,"parallel":2}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	w, resp := doLoadDefaults(t, s, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	effective, _ := resp["effective"].(map[string]interface{})
	source, _ := resp["source"].(map[string]interface{})
	if effective == nil || source == nil {
		t.Fatalf("в ответе нет effective/source: %s", w.Body.String())
	}
	if v, _ := effective["contextLength"].(float64); int(v) != 16384 {
		t.Errorf("effective.contextLength = %v, want 16384", effective["contextLength"])
	}
	if s, _ := source["contextLength"].(string); s != "defaultProfile" {
		t.Errorf("source.contextLength = %q, want defaultProfile", source["contextLength"])
	}
	// Без доступного cppworker остальные поля помечены как «не задано» — это
	// честно: значения не выдумываются.
	if s, _ := source["batchSize"].(string); s == "" {
		t.Error("source.batchSize пуст — источник должен быть указан всегда")
	}
}

// TestR83_LoadDefaults_AppliedToModelWithoutProfile — значения из конфига
// реально применяются при загрузке: цепочку «профиль модели → дефолт» проверяет
// internal/balancer (TestR83_DefaultProfile_FillsGapsAfterModelProfile), здесь —
// что PUT из WebUI кладёт значения именно в тот конфиг, который читает загрузка.
func TestR83_LoadDefaults_AppliedToModelWithoutProfile(t *testing.T) {
	s, cfg := newLoadDefaultsServer(t)
	if w, _ := doLoadDefaults(t, s, http.MethodPut,
		`{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"kvCacheType":"q4_0","parallel":2}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if cfg.DefaultModelProfile == nil || cfg.DefaultModelProfile.ContextLength != 16384 {
		t.Fatalf("конфиг загрузки не заполнен: %+v", cfg.DefaultModelProfile)
	}
	// Тот же объект читает executeLlamaCppLoad через proxy.GetDefaultModelProfile.
	got, ok := s.proxy.GetDefaultModelProfile()
	if !ok {
		t.Fatal("GetDefaultModelProfile не видит записанные настройки — загрузка " +
			"модели без профиля их не применит")
	}
	if got.Parallel != 2 || got.KVCacheType != "q4_0" {
		t.Errorf("прочитано не то: parallel=%d kv=%q", got.Parallel, got.KVCacheType)
	}
}
