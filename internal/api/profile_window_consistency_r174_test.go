//go:build llama_stub

// profile_window_consistency_r174_test.go — R174 (2026-10-18).
//
// НАЙДЕНО СЦЕНАРИЯМИ НАСТРОЕК (params-e2e --scenario reload): оператор выставил
// в карточке «8192 суммарно» и «parallel=1», нажал «Применить» — модель
// загрузилась как ctx=4096, perSeq=2048, parallel=2.
//
// Причина: профиль хранил СТАРЫЙ contextPerSeq=4096, apply отправлял его вместе с
// contextSize=8192, и cppworker считал слоты от perSeq (8192/4096 = 2), а не от
// явного parallel=1. Правило R174: когда заданы и суммарное окно, и слоты, окно
// НА СЛОТ вычисляется из них, а устаревшее значение профиля не переопределяет.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// captureReloadR174 — мок cppworker: принимает reload и запоминает тело.
func captureReloadR174(t *testing.T) (*httptest.Server, *map[string]interface{}) {
	t.Helper()
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/reload" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"loading"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func portOfR174(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u := srv.URL
	i := len("http://127.0.0.1:")
	p, err := strconv.Atoi(u[i:])
	if err != nil {
		t.Fatalf("не разобрал порт из %q: %v", u, err)
	}
	return p
}

// TestR174_Merge_ClearsStalePerSeq — главный контрфакт: смена суммарного окна
// и/или слотов СБРАСЫВАЕТ устаревшее окно на слот.
func TestR174_Merge_ClearsStalePerSeq(t *testing.T) {
	existing := types.LlamaCppModelProfile{ContextLength: 32768, ContextPerSeq: 4096, Parallel: 2}
	cli, par := 8192, 1
	values := types.LlamaCppModelProfile{ContextLength: cli, Parallel: par}
	presence := profilePresence{Parallel: &par}

	merged := mergeProfileUpdate(existing, values, presence)
	if merged.ContextPerSeq != 0 {
		t.Errorf("contextPerSeq = %d, want 0: устаревшее окно на слот не сброшено — cppworker "+
			"посчитает слоты от него и модель поднимется не с тем окном", merged.ContextPerSeq)
	}
	if merged.ContextLength != 8192 || merged.Parallel != 1 {
		t.Errorf("contextLength/parallel = %d/%d, want 8192/1", merged.ContextLength, merged.Parallel)
	}
}

// TestR174_Merge_ExplicitPerSeqWins — R119: намеренно заданное окно на слот
// (например 8192 при 3 слотах) не переписывается и не сбрасывается.
func TestR174_Merge_ExplicitPerSeqWins(t *testing.T) {
	existing := types.LlamaCppModelProfile{ContextLength: 16384, ContextPerSeq: 5376, Parallel: 3}
	per, par := 8192, 3
	values := types.LlamaCppModelProfile{ContextPerSeq: per, Parallel: par}
	presence := profilePresence{ContextPerSeq: &per, Parallel: &par}

	merged := mergeProfileUpdate(existing, values, presence)
	if merged.ContextPerSeq != 8192 {
		t.Errorf("contextPerSeq = %d, want 8192: явно заданное окно на слот обязано победить", merged.ContextPerSeq)
	}
}

// TestR174_Merge_NoWindowChangeKeepsPerSeq — правка другого поля не должна
// обнулять окно на слот.
func TestR174_Merge_NoWindowChangeKeepsPerSeq(t *testing.T) {
	existing := types.LlamaCppModelProfile{ContextLength: 16384, ContextPerSeq: 5376, Parallel: 3}
	batch := 256
	values := types.LlamaCppModelProfile{BatchSize: batch}
	presence := profilePresence{}

	merged := mergeProfileUpdate(existing, values, presence)
	if merged.ContextPerSeq != 5376 {
		t.Errorf("contextPerSeq = %d, want 5376: правка batchSize не касается окна", merged.ContextPerSeq)
	}
}
