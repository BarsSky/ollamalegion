//go:build llama_stub

// load_defaults_reasoning_test.go — R106b (2026-10-12).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «не дало во вкладке настройки бекенда на странице gguf
// models поменять параметры размышления с выключенного на включенный … модель
// gemma4 при загрузке применяет не те параметры что указаны в настройках …
// сейчас просто список общих настроек который ещё и не применяется, хотя
// «Сохранить» отрабатывает».
//
// Что происходило (воспроизведено на живом стенде):
//  1. Оператор переключал reasoning и сохранял — PUT /api/v1/cppworker/load-defaults
//     отвечал 200, и значение честно ложилось в config.defaultModelProfile
//     (поле EnableReasoning в профиле есть, presence-мерж его переносит);
//  2. НО GET load-defaults строил effective только из env контейнера
//     (`defaultEnableReasoning`) и поле профиля не читал ВООБЩЕ. В списке
//     «настройки по умолчанию для новых моделей» оставалось «off (env контейнера)»
//     → оператор видел, что «галочка не применяется», хотя загрузка её получала;
//  3. то же в карточке модели (GET model-catalog): flash/mmap/reasoning не
//     показывались вовсе, поэтому расхождение «в настройках одно, грузится другое»
//     было невидимым.
//
// Тесты ниже держат контракт: значение reasoning из профиля ОБЯЗАНО читаться
// обратно с источником defaultProfile, явное false отличимо от «не задавать»,
// а карточка модели показывает reasoning/flash/mmap вместе с источником.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR106b_LoadDefaults_ReasoningFromProfileReportedAsDefaultProfile — главный
// регресс: сохранённое в профиле значение видно в effective с источником.
func TestR106b_LoadDefaults_ReasoningFromProfileReportedAsDefaultProfile(t *testing.T) {
	s, cfg := newLoadDefaultsServer(t)

	if w, _ := doLoadDefaults(t, s, http.MethodPut, `{"contextLength":16384,"enableReasoning":true}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if cfg.DefaultModelProfile == nil || cfg.DefaultModelProfile.EnableReasoning == nil ||
		!*cfg.DefaultModelProfile.EnableReasoning {
		t.Fatalf("enableReasoning не записан в defaultModelProfile: %+v", cfg.DefaultModelProfile)
	}

	w, resp := doLoadDefaults(t, s, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	effective, _ := resp["effective"].(map[string]interface{})
	source, _ := resp["source"].(map[string]interface{})
	if v, _ := effective["enableReasoning"].(bool); !v {
		t.Errorf("effective.enableReasoning = %v, want true. "+
			"Это и есть живой дефект: оператор сохраняет «включено», а список "+
			"показывает выключенное (значение читалось только из env контейнера).",
			effective["enableReasoning"])
	}
	if s, _ := source["enableReasoning"].(string); s != "defaultProfile" {
		t.Errorf("source.enableReasoning = %q, want defaultProfile", source["enableReasoning"])
	}
}

// TestR106b_LoadDefaults_ReasoningExplicitFalseIsSticky — обратная сторона:
// выключение тоже должно работать. В профиле поле *bool, поэтому явное false
// обязано отличаться от «поле не прислали» и не подменяться env-значением.
func TestR106b_LoadDefaults_ReasoningExplicitFalseIsSticky(t *testing.T) {
	s, cfg := newLoadDefaultsServer(t)

	if w, _ := doLoadDefaults(t, s, http.MethodPut, `{"contextLength":16384,"enableReasoning":true}`); w.Code != http.StatusOK {
		t.Fatalf("PUT true: %d %s", w.Code, w.Body.String())
	}
	if w, _ := doLoadDefaults(t, s, http.MethodPut, `{"contextLength":16384,"enableReasoning":false}`); w.Code != http.StatusOK {
		t.Fatalf("PUT false: %d %s", w.Code, w.Body.String())
	}
	if cfg.DefaultModelProfile == nil || cfg.DefaultModelProfile.EnableReasoning == nil ||
		*cfg.DefaultModelProfile.EnableReasoning {
		t.Fatalf("явное false потеряно: %+v", cfg.DefaultModelProfile)
	}

	_, resp := doLoadDefaults(t, s, http.MethodGet, "")
	effective, _ := resp["effective"].(map[string]interface{})
	source, _ := resp["source"].(map[string]interface{})
	if v, ok := effective["enableReasoning"].(bool); !ok || v {
		t.Errorf("effective.enableReasoning = %v, want false", effective["enableReasoning"])
	}
	if s, _ := source["enableReasoning"].(string); s != "defaultProfile" {
		t.Errorf("source.enableReasoning = %q, want defaultProfile (явное «выкл» из профиля)", source["enableReasoning"])
	}
}

// TestR106b_LoadDefaults_ReasoningNoneWhenNothingSet — ничего не выдумываем:
// без профиля и без доступного cppworker источник честно «none».
func TestR106b_LoadDefaults_ReasoningNoneWhenNothingSet(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)

	_, resp := doLoadDefaults(t, s, http.MethodGet, "")
	source, _ := resp["source"].(map[string]interface{})
	if got, _ := source["enableReasoning"].(string); got != "none" {
		t.Errorf("source.enableReasoning = %q, want none (ни профиля, ни cppworker)", got)
	}
}

// TestR106b_ModelCatalog_ShowsReasoningPerModel — карточка модели («что
// применится к ЭТОЙ модели») обязана показывать reasoning/flash/mmap с
// источником: профиль модели → настройки по умолчанию → env контейнера.
func TestR106b_ModelCatalog_ShowsReasoningPerModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[
				{"name":"with-profile.gguf","sizeBytes":100},
				{"name":"without-profile.gguf","sizeBytes":200}]}`))
		case "/api/v1/cppworker/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"config":{
				"defaultCtxSize":32768,
				"defaultBatchSize":512,
				"defaultGpuLayers":-1,
				"defaultNParallel":1,
				"defaultKvCacheType":"f16",
				"defaultFlashAttnType":-1,
				"defaultUseMmap":true,
				"defaultEnableReasoning":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	var port int
	{
		u, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatalf("не разобрал URL upstream %q: %v", upstream.URL, err)
		}
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			t.Fatalf("не разобрал порт upstream %q: %v", upstream.URL, err)
		}
	}

	s, _ := newLoadDefaultsServer(t)
	if err := s.proxy.AddBackend(types.Backend{
		ID:            "cppworker-test",
		Host:          "127.0.0.1",
		CppWorkerPort: port,
		Type:          types.BackendTypeLlamaCpp,
		Status:        types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	reasoningOn := true
	s.proxy.SetModelProfile("with-profile", types.LlamaCppModelProfile{
		ContextLength:   16384,
		EnableReasoning: &reasoningOn,
	})
	s.proxy.SetModelProfile("without-profile.gguf", types.LlamaCppModelProfile{
		ContextLength: 8192,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-catalog", nil)
	w := httptest.NewRecorder()
	s.handleModelCatalog(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("model-catalog: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Files []struct {
			Name       string                 `json:"name"`
			HasProfile bool                   `json:"hasProfile"`
			Effective  map[string]interface{} `json:"effective"`
			Source     map[string]string      `json:"source"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("файлов в каталоге %d, want 2 (%s)", len(resp.Files), w.Body.String())
	}

	byName := map[string]int{}
	for i, f := range resp.Files {
		byName[f.Name] = i
	}
	// 1) У модели со своим профилем reasoning=true и источник — профиль.
	idx, ok := byName["with-profile.gguf"]
	if !ok {
		t.Fatalf("в каталоге нет with-profile.gguf: %s", w.Body.String())
	}
	gotWith := resp.Files[idx]
	if v, _ := gotWith.Effective["enableReasoning"].(bool); !v {
		t.Errorf("with-profile: effective.enableReasoning = %v, want true", gotWith.Effective["enableReasoning"])
	}
	if gotWith.Source["enableReasoning"] != "profile" {
		t.Errorf("with-profile: source.enableReasoning = %q, want profile", gotWith.Source["enableReasoning"])
	}
	// flash/mmap тоже обязаны быть видны (они уходят в тело загрузки).
	if gotWith.Source["useMmap"] == "" {
		t.Errorf("with-profile: useMmap не показан (source пуст) — карточка скрывает mmap")
	}

	// 2) У модели без профиля reasoning — из env контейнера (false).
	idx2, ok := byName["without-profile.gguf"]
	if !ok {
		t.Fatalf("в каталоге нет without-profile.gguf: %s", w.Body.String())
	}
	gotWithout := resp.Files[idx2]
	if v, ok := gotWithout.Effective["enableReasoning"].(bool); !ok || v {
		t.Errorf("without-profile: effective.enableReasoning = %v, want false (env контейнера)",
			gotWithout.Effective["enableReasoning"])
	}
	if gotWithout.Source["enableReasoning"] != "cppworker-env" {
		t.Errorf("without-profile: source.enableReasoning = %q, want cppworker-env",
			gotWithout.Source["enableReasoning"])
	}
}

// TestR106b_ModelCatalog_IgnoreDefaultsStillAppliesProfile — R106b: профиль
// модели применяется ВСЕГДА, а ignoreDefaults гасит только слой «настройки по
// умолчанию» (так же, как в executeLlamaCppLoad с R83/v62).
//
// ЖИВОЙ ДЕФЕКТ: на стенде все 18 профилей помечены ignoreDefaults, и карточка
// показывала для них значения слоя по умолчанию/env вместо значений профиля —
// оператор видел «модель грузится не с теми параметрами, что в настройках».
func TestR106b_ModelCatalog_IgnoreDefaultsStillAppliesProfile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/files":
			_, _ = w.Write([]byte(`{"files":[{"name":"ignored.gguf","sizeBytes":100}]}`))
		case "/api/v1/cppworker/config":
			_, _ = w.Write([]byte(`{"config":{"defaultCtxSize":32768,"defaultNParallel":1,"defaultEnableReasoning":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	s, _ := newLoadDefaultsServer(t)
	if err := s.proxy.AddBackend(types.Backend{
		ID: "cppworker-test", Host: "127.0.0.1", CppWorkerPort: port,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	// Настройки по умолчанию: другое окно и параллельность — они НЕ должны
	// попасть в карточку, потому что профиль модели просил ignoreDefaults.
	on := true
	s.proxy.SetDefaultModelProfile(types.LlamaCppModelProfile{
		ContextLength: 65536, Parallel: 4, EnableReasoning: &on,
	})
	reasoningOn := true
	s.proxy.SetModelProfile("ignored", types.LlamaCppModelProfile{
		ContextLength:   4096,
		EnableReasoning: &reasoningOn,
		IgnoreDefaults:  true,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-catalog", nil)
	w := httptest.NewRecorder()
	s.handleModelCatalog(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("model-catalog: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Files []struct {
			Name           string                 `json:"name"`
			HasProfile     bool                   `json:"hasProfile"`
			ProfileIgnored bool                   `json:"profileIgnored"`
			Effective      map[string]interface{} `json:"effective"`
			Source         map[string]string      `json:"source"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("файлов %d, want 1 (%s)", len(resp.Files), w.Body.String())
	}
	f := resp.Files[0]
	if !f.HasProfile || !f.ProfileIgnored {
		t.Errorf("флаги профиля потеряны: hasProfile=%v profileIgnored=%v", f.HasProfile, f.ProfileIgnored)
	}
	if v, ok := intFromEffective(f.Effective, "contextLength"); !ok || v != 4096 {
		t.Errorf("contextLength = %v, want 4096 (профиль модели применяется и при ignoreDefaults)",
			f.Effective["contextLength"])
	}
	if f.Source["contextLength"] != "profile" {
		t.Errorf("source.contextLength = %q, want profile", f.Source["contextLength"])
	}
	if v, _ := f.Effective["enableReasoning"].(bool); !v {
		t.Errorf("enableReasoning = %v, want true (из профиля модели)", f.Effective["enableReasoning"])
	}
	if f.Source["enableReasoning"] != "profile" {
		t.Errorf("source.enableReasoning = %q, want profile", f.Source["enableReasoning"])
	}
	// Слой «настройки по умолчанию» для этой модели отключён: его parallel=4
	// не должен появиться как значение, а источник — не defaultProfile.
	if f.Source["parallel"] == "defaultProfile" {
		t.Errorf("настройки по умолчанию применились, хотя профиль просил ignoreDefaults: %v", f.Effective)
	}
}

// TestR106b_FillFromCppWorker_BoolsAndNoOverwrite — env контейнера добивает
// только ПУСТЫЕ поля: значения профиля не перетираются.
func TestR106b_FillFromCppWorker_BoolsAndNoOverwrite(t *testing.T) {
	effective := map[string]interface{}{
		"enableReasoning": true, // уже задано профилем
		"useMmap":         false,
	}
	source := map[string]string{
		"enableReasoning": "profile",
		"useMmap":         "profile",
	}
	cw := cppworkerSnapshot{Available: true, Config: map[string]interface{}{
		"defaultFlashAttnType":   float64(-1),
		"defaultUseMmap":         true,
		"defaultEnableReasoning": false,
	}}
	fillFromCppWorker(effective, source, cw)

	if effective["enableReasoning"] != true || source["enableReasoning"] != "profile" {
		t.Errorf("env перетёрло значение профиля: %v/%q", effective["enableReasoning"], source["enableReasoning"])
	}
	if effective["useMmap"] != false || source["useMmap"] != "profile" {
		t.Errorf("env перетёрло mmap профиля: %v/%q", effective["useMmap"], source["useMmap"])
	}
	if v, _ := effective["flashAttn"].(int); v != -1 {
		t.Errorf("flashAttn = %v, want -1 из env", effective["flashAttn"])
	}
	if source["flashAttn"] != "cppworker-env" {
		t.Errorf("source.flashAttn = %q, want cppworker-env", source["flashAttn"])
	}
}
