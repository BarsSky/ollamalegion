// exact_tokens_without_calibration_r493_test.go — R493 (2026-11-17): точный
// подсчёт токенов не должен зависеть от наличия хранилища калибровки.
//
// ЖИВОЙ ДЕФЕКТ. В `applyPromptCalibration` стоял ранний выход
//
//	if raw <= 0 || p == nil || p.promptCalib == nil { return }
//
// и вызов `applyExactPromptTokensR473` жил ПОСЛЕ него. Следствие: если хранилище
// калибровки не создано (юнит-тесты, лёгкая сборка, прокси без state-файла),
// решения об окне считаются по эвристике, которая завышает в 1.92–2.01 раза —
// ровно та причина, из-за которой на втором стенде клиент получал 503
// «needs 184296 tokens» при фактических 93962.
//
// Проверка: мета с длинным образцом промпта и живым «воркером» (httptest),
// у прокси НЕТ promptCalib — после extractRequestMeta оценка обязана стать
// точной, источник — "tokenizer".
package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newExactTokensWorkerR493 — мини-воркер: отдаёт список моделей и считает токены.
func newExactTokensWorkerR493(t *testing.T, model string, tokens int) (*Proxy, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loaded",`+
			`"context_size":65536,"context_per_seq":32768,"max_slots":2,"parallel":2}]}`, model)
	})
	mux.HandleFunc("/api/tokenize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tokens":%d,"source":"tokenizer","model_loaded":true}`, tokens)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("разбор адреса тестового воркера: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		client:     &http.Client{},
	}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Host: u.Hostname(), CppWorkerPort: port, Type: types.BackendTypeLlamaCpp,
	}}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: model, ContextLength: 65536, ContextPerSeq: 32768, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()
	return p, model
}

// TestR493_ExactTokensAppliedWithoutCalibrationStore — главный регресс: прокси
// БЕЗ promptCalib всё равно получает точное число токенов.
func TestR493_ExactTokensAppliedWithoutCalibrationStore(t *testing.T) {
	const model = "r493-exact-no-calib"
	p, _ := newExactTokensWorkerR493(t, model, 777)
	if p.promptCalib != nil {
		t.Fatal("у тестового прокси не должно быть хранилища калибровки — иначе тест не про этот дефект")
	}

	meta := &RequestMeta{
		ModelName:             model,
		ExactSampleText:       strings.Repeat("промпт ", exactTokensMinSampleR473),
		EstimatedPromptTokens: 5000,
		PromptNonASCIIShare:   0.9,
	}
	p.applyPromptCalibration(meta)

	if meta.PromptTokensSource != "tokenizer" {
		t.Fatalf("источник токенов = %q, ожидался tokenizer: без калибровки точный подсчёт пропущен (живой дефект)", meta.PromptTokensSource)
	}
	want := 777 + exactTokensOverheadTokensR473
	if meta.EstimatedPromptTokens != want {
		t.Fatalf("точных токенов = %d, ожидалось %d (777 + overhead %d)", meta.EstimatedPromptTokens, want, exactTokensOverheadTokensR473)
	}
}

// TestR493_ExactTokensAppliedWithCalibrationStore — прежнее поведение не сломано:
// когда калибровка есть, точный подсчёт тоже применяется (и он последний).
func TestR493_ExactTokensAppliedWithCalibrationStore(t *testing.T) {
	const model = "r493-exact-with-calib"
	p, _ := newExactTokensWorkerR493(t, model, 4242)
	p.promptCalib = newPromptCalibration("")

	meta := &RequestMeta{
		ModelName:             model,
		ExactSampleText:       strings.Repeat("промпт ", exactTokensMinSampleR473),
		EstimatedPromptTokens: 5000,
		PromptNonASCIIShare:   0.9,
	}
	p.applyPromptCalibration(meta)

	if meta.PromptTokensSource != "tokenizer" {
		t.Fatalf("источник токенов = %q, ожидался tokenizer", meta.PromptTokensSource)
	}
	if want := 4242 + exactTokensOverheadTokensR473; meta.EstimatedPromptTokens != want {
		t.Fatalf("точных токенов = %d, ожидалось %d", meta.EstimatedPromptTokens, want)
	}
}
