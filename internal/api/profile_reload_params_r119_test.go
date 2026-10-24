// profile_reload_params_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд). Оператор задавал в WebUI профиль модели
// («16384 суммарно, 8192 на клиента, parallel=3, галочка размышлений») и нажимал
// «Применить». Параметры 5/5 доезжали, а contextPerSeq и enableReasoning — нет:
//   - cppworker поднимал ctx=16896 / per_seq=5632 (окно ДЕЛИЛОСЬ между слотами
//     вместо того, чтобы быть на слот);
//   - reasoning оставался выключенным.
//
// Причина: mergeModelProfile не сохранял contextPerSeq (PUT отвечал 200, поле
// исчезало), а reloadModelOnCppWorker не отправлял ни contextPerSeq, ни
// enableReasoning — хотя cppworker оба поля принимает (reloadModelRequest).
//
// Тест падает до правки: body reload не содержит contextPerSeq/enableReasoning,
// а профиль после мержа теряет contextPerSeq.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// mockReloadCapture — мок cppworker, который запоминает тело reload-запроса.
func mockReloadCapture(t *testing.T) (*httptest.Server, *map[string]interface{}) {
	t.Helper()
	var captured map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/reload" {
			http.NotFound(w, r)
			return
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured = payload
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "reloaded"})
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

// TestR119_ReloadBody_CarriesContextPerSeqAndReasoning — «Применить профиль»
// обязано довозить до cppworker окно на клиента и переключатель размышлений.
func TestR119_ReloadBody_CarriesContextPerSeqAndReasoning(t *testing.T) {
	srv, captured := mockReloadCapture(t)
	host, port := splitHostPortTest(t, srv.URL)

	reasoning := true
	s := &Server{}
	backend := types.Backend{ID: "b1", Host: host, CppWorkerPort: port}

	if _, err := s.reloadModelOnCppWorker(backend, "gemma-4-E4B-it-Q4_K_M", types.LlamaCppModelProfile{
		ContextLength:   16384,
		ContextPerSeq:   8192,
		Parallel:        3,
		EnableReasoning: &reasoning,
		KVCacheType:     "q4_0",
	}); err != nil {
		t.Fatalf("reloadModelOnCppWorker: %v", err)
	}

	if *captured == nil {
		t.Fatal("reload не был вызван")
	}
	if got, ok := (*captured)["contextPerSeq"].(float64); !ok || int(got) != 8192 {
		t.Errorf("contextPerSeq = %v, want 8192: без него cppworker делит окно между слотами "+
			"(16384/3 → per_seq 5632) вместо окна НА клиента", (*captured)["contextPerSeq"])
	}
	if got, ok := (*captured)["enableReasoning"].(bool); !ok || !got {
		t.Errorf("enableReasoning = %v, want true: галочка «размышления» из профиля не доехала "+
			"до cppworker", (*captured)["enableReasoning"])
	}
	if got, ok := (*captured)["parallel"].(float64); !ok || int(got) != 3 {
		t.Errorf("parallel = %v, want 3 (контроль: остальные поля профиля не сломались)", (*captured)["parallel"])
	}
}

// TestR119_ReloadBody_OmitsUnsetFields — нулевые значения профиля НЕ должны
// уходить в тело: 0 = «не задано», и cppworker обязан взять свой дефолт.
func TestR119_ReloadBody_OmitsUnsetFields(t *testing.T) {
	srv, captured := mockReloadCapture(t)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{ID: "b1", Host: host, CppWorkerPort: port}
	if _, err := s.reloadModelOnCppWorker(backend, "m", types.LlamaCppModelProfile{
		ContextLength: 4096,
	}); err != nil {
		t.Fatalf("reloadModelOnCppWorker: %v", err)
	}

	for _, key := range []string{"contextPerSeq", "enableReasoning"} {
		if _, present := (*captured)[key]; present {
			t.Errorf("%s присутствует в теле при незаданном профиле (значение %v): "+
				"cppworker обязан сам решить дефолт", key, (*captured)[key])
		}
	}
}

// TestR119_MergeModelProfile_KeepsContextPerSeq — сохранение профиля не должно
// терять окно на клиента.
func TestR119_MergeModelProfile_KeepsContextPerSeq(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength: 16384,
		Parallel:      3,
	}
	update := types.LlamaCppModelProfile{
		ContextLength: 16384,
		ContextPerSeq: 8192,
		Parallel:      3,
	}

	out := mergeModelProfile(existing, update)
	if out.ContextPerSeq != 8192 {
		t.Errorf("ContextPerSeq = %d после мержа, want 8192: поле терялось при сохранении, "+
			"и на загрузку уходил только contextLength", out.ContextPerSeq)
	}

	// 0 = «не менять»: частичное обновление не сбрасывает уже сохранённое окно.
	kept := mergeModelProfile(types.LlamaCppModelProfile{ContextPerSeq: 8192}, types.LlamaCppModelProfile{ContextLength: 8192})
	if kept.ContextPerSeq != 8192 {
		t.Errorf("ContextPerSeq = %d после частичного обновления, want 8192 (0 означает «не менять»)", kept.ContextPerSeq)
	}
}

// mergeFromJSON — повторить то, что делает PUT-хендлер: декодировать тело в
// профиль + в profilePresence и смержить с существующим.
func mergeFromJSON(t *testing.T, existing types.LlamaCppModelProfile, raw string) types.LlamaCppModelProfile {
	t.Helper()
	var upd types.LlamaCppModelProfile
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&upd); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var presence profilePresence
	if err := json.Unmarshal([]byte(raw), &presence); err != nil {
		t.Fatalf("presence %s: %v", raw, err)
	}
	return mergeProfileUpdate(existing, upd, presence)
}

// TestR119_ClearParallelAndContextPerSeq — явный 0 снимает настройку.
//
// Живое следствие отсутствия этого правила: восстановить профиль после теста
// удалось только через DELETE + PUT — `"parallel": 0` молча сохранял прежнее
// значение, и «вернуть дефолт окружения» через API было нельзя.
func TestR119_ClearParallelAndContextPerSeq(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength: 8192,
		Parallel:      3,
		ContextPerSeq: 8192,
		KVCacheType:   "q4_0",
	}

	// Явные нули = снять.
	cleared := mergeFromJSON(t, existing, `{"contextLength":8192,"parallel":0,"contextPerSeq":0}`)
	if cleared.Parallel != 0 {
		t.Errorf("Parallel = %d, want 0: явный \"parallel\": 0 обязан снимать настройку "+
			"(иначе вернуться к CPPWORKER_N_PARALLEL через API нельзя)", cleared.Parallel)
	}
	if cleared.ContextPerSeq != 0 {
		t.Errorf("ContextPerSeq = %d, want 0: явный \"contextPerSeq\": 0 обязан снимать окно на клиента",
			cleared.ContextPerSeq)
	}
	if cleared.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q: поля, которых нет в теле, обязаны сохраняться", cleared.KVCacheType)
	}

	// Поля не переданы = сохранить (прежняя PATCH-like семантика).
	kept := mergeFromJSON(t, existing, `{"contextLength":8192}`)
	if kept.Parallel != 3 || kept.ContextPerSeq != 8192 {
		t.Errorf("без полей в теле получили Parallel=%d ContextPerSeq=%d, want 3 и 8192 "+
			"(частичное обновление не должно сбрасывать настройки)", kept.Parallel, kept.ContextPerSeq)
	}

	// Передано новое значение = применить.
	changed := mergeFromJSON(t, existing, `{"contextLength":16384,"parallel":2,"contextPerSeq":4096}`)
	if changed.Parallel != 2 || changed.ContextPerSeq != 4096 || changed.ContextLength != 16384 {
		t.Errorf("получили Parallel=%d ContextPerSeq=%d ContextLength=%d, want 2/4096/16384",
			changed.Parallel, changed.ContextPerSeq, changed.ContextLength)
	}
}

// TestR119_ValidateContextPerSeq — границы окна на клиента.
func TestR119_ValidateContextPerSeq(t *testing.T) {
	base := types.LlamaCppModelProfile{ContextLength: 8192}
	bad := base
	bad.ContextPerSeq = -1
	if err := validateModelProfile(bad); err == nil {
		t.Error("contextPerSeq=-1 принят, ожидалась ошибка")
	}
	tooBig := base
	tooBig.ContextPerSeq = 262145
	if err := validateModelProfile(tooBig); err == nil {
		t.Error("contextPerSeq=262145 принят, ожидалась ошибка")
	}
	ok := base
	ok.ContextPerSeq = 8192
	if err := validateModelProfile(ok); err != nil {
		t.Errorf("contextPerSeq=8192 отвергнут: %v", err)
	}
	zero := base
	if err := validateModelProfile(zero); err != nil {
		t.Errorf("contextPerSeq=0 (не задано) отвергнут: %v", err)
	}
}
