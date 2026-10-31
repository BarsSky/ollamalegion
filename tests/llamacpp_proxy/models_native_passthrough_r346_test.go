// models_native_passthrough_r346_test.go — R346 (2026-10-30).
//
// ЗАЧЕМ. `handleModels` балансера объявлен как «список моделей в НАТИВНОМ формате
// cppworker'а» (internal/balancer/llamacpp_handlers_readonly.go:588), но структура
// декодирования (cppWorkerModelsNative) держала camelCase-теги — `sizeBytes`,
// `contextSize`, `nLayers`, `gpuLayers`, `loadedAt` — тогда как воркер печатает
// snake_case (`size_bytes`, `context_size`, `n_layers`, `gpu_layers_actual`,
// `loaded_at`). Поле не совпало → ноль → omitempty → в ответе балансера оставались
// только name/path/state. Ошибку поймали в поллере (комментарий R45) и описали как
// F-15 в docs/archive/audits/AUDIT-balancer-cppworker-wire.md, но этот путь
// (публичный /api/models) остался непоправленным.
//
// Дополнительно (R248 → R346): воркер отдаёт ФАКТИЧЕСКИЙ offload в
// `gpu_layers_actual`, а `gpu_layers` содержит -1 (авто). Балансер обязан брать
// фактическое число: иначе клиент/UI видит -1 или 0 при реальных 42 слоях на GPU.
//
// Проверяется:
//  1. размер, слои, n_embd, n_vocab, окно, время загрузки доходят до ответа;
//  2. active_queries доходит (R346);
//  3. gpu_layers: приоритет gpu_layers_actual, а не -1 из gpu_layers;
//  4. nHeads/totalQueries ОТСУТСТВУЮТ — воркер их в /api/models не печатает,
//     значит вечный ноль в контракте не нужен.
package llamacpp_proxy

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestR346_ModelsNativePassthrough — /api/models отдаёт реальные поля воркера.
func TestR346_ModelsNativePassthrough(t *testing.T) {
	mp := newMatrixProxy(t)

	resp, raw := matrixDo(t, mp, http.MethodGet, "/api/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
	}

	var body struct {
		Count  int `json:"count"`
		Models []struct {
			Name          string `json:"name"`
			State         string `json:"state"`
			SizeBytes     int64  `json:"sizeBytes"`
			NLayers       int    `json:"nLayers"`
			NEmbd         int    `json:"nEmbd"`
			NVocab        int    `json:"nVocab"`
			ContextSize   int    `json:"contextSize"`
			GPULayers     int    `json:"gpuLayers"`
			ActiveQueries int    `json:"activeQueries"`
			LoadedAt      string `json:"loadedAt"`
			NHeads        *int   `json:"nHeads"`
			TotalQueries  *int   `json:"totalQueries"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("ответ не JSON: %v; тело: %s", err, raw)
	}
	if body.Count != 1 || len(body.Models) != 1 {
		t.Fatalf("count=%d, models=%d, want 1/1; тело: %s", body.Count, len(body.Models), raw)
	}

	m := body.Models[0]
	if m.Name == "" {
		t.Errorf("потерялось имя модели: %s", raw)
	}
	if m.State != "loaded" {
		t.Errorf("state = %q, want loaded", m.State)
	}
	if m.SizeBytes != 4215695776 {
		t.Errorf("sizeBytes = %d, want 4215695776 — snake_case size_bytes не декодировался (%s)", m.SizeBytes, raw)
	}
	if m.NLayers != 42 {
		t.Errorf("nLayers = %d, want 42 (n_layers)", m.NLayers)
	}
	if m.NEmbd != 2560 {
		t.Errorf("nEmbd = %d, want 2560 (n_embd)", m.NEmbd)
	}
	if m.NVocab != 262144 {
		t.Errorf("nVocab = %d, want 262144 (n_vocab)", m.NVocab)
	}
	if m.ContextSize != 32768 {
		t.Errorf("contextSize = %d, want 32768 (context_size)", m.ContextSize)
	}
	if m.ActiveQueries != 2 {
		t.Errorf("activeQueries = %d, want 2 (active_queries)", m.ActiveQueries)
	}
	if m.LoadedAt == "" {
		t.Errorf("loadedAt пуст — snake_case loaded_at не декодировался (%s)", raw)
	}
	// gpu_layers в заглушке = -1 (авто), фактический offload = 42 (R248).
	if m.GPULayers != 42 {
		t.Errorf("gpuLayers = %d, want 42 (gpu_layers_actual имеет приоритет над gpu_layers=-1)", m.GPULayers)
	}
	if m.NHeads != nil {
		t.Errorf("nHeads присутствует (=%d), хотя воркер его в /api/models не печатает", *m.NHeads)
	}
	if m.TotalQueries != nil {
		t.Errorf("totalQueries присутствует (=%d), хотя воркер его в /api/models не печатает", *m.TotalQueries)
	}
}
