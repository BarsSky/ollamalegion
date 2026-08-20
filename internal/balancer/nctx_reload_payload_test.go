//go:build llama_stub

// Round 51.4 (2026-08-20) — regression test for the "unknown field" reload
// failure that caused Cline to hang with keepalive while balancer retried
// preflight reloads. The bug: nctx_reload.go and nctx_reload_adaptive.go
// were sending fields (`reason`, `adaptiveStage`) that don't exist in
// cppworker's reloadModelRequest struct (cmd/cppworker/types.go:134).
// Go's strict JSON decoder rejected them with HTTP 400, every reload
// failed, Cline never got a response.
//
// This test mirrors cppworker's reloadModelRequest struct (locally, since
// the cmd/cppworker package is a separate main package and can't be
// imported). It builds the same payload the balancer sends, marshals it,
// and asserts that JSON unmarshal into a struct with DisallowUnknownFields
// (matching cppworker's behavior) does NOT fail.

package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// cppworkerReloadRequestMirror — локальное зеркало cmd/cppworker/types.go:134
// (reloadModelRequest). Если в апстриме добавят новое поле — обновить и здесь,
// иначе тест не сможет отловить drift.
type cppworkerReloadRequestMirror struct {
	Name               string   `json:"name"`
	ContextSize        *int     `json:"contextSize,omitempty"`
	BatchSize          *int     `json:"batchSize,omitempty"`
	GPULayers          *int     `json:"gpuLayers,omitempty"`
	FlashAttn          *int     `json:"flashAttn,omitempty"`
	NUMA               *bool    `json:"numa,omitempty"`
	UseMmap            *bool    `json:"useMmap,omitempty"`
	Force              *bool    `json:"force,omitempty"`
	Parallel           *int     `json:"parallel,omitempty"`
	KVCacheType        *string  `json:"kvCacheType,omitempty"`
	OverrideTensors    []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// TestNctxReload_PayloadIsCppworkerCompatible — главный regression-тест.
// Симулирует то, что balancer шлёт в cppworker /api/models/reload,
// и проверяет что cppworker НЕ вернёт 400 "unknown field".
func TestNctxReload_PayloadIsCppworkerCompatible(t *testing.T) {
	// Собираем payload тем же кодом, что nctx_reload.go:758
	payloadMap := map[string]interface{}{
		"name":        "gemma-4-E4B-it-Q4_K_M",
		"contextSize": 65536,
		"force":       true,
		"gpuLayers":   -2,
		"flashAttn":   -1,
		"useMmap":     true,
	}

	// Имитируем AdaptiveStrategy (см. enrichReloadPayload)
	strategy := &AdaptiveStrategy{
		GPULayers:   -2,
		UseMmap:     true,
		FlashAttnType: 0,
		KVCacheType: "q4_0",
		Stage:       "fallback_no_meta",
		NCtx:        65536,
	}
	enrichReloadPayload(payloadMap, strategy)

	// Также имитируем override-tensors (Round 7)
	strategy.OverrideTensors = []string{"blk\\.ffn_.*_exps\\.weight"}
	strategy.OverrideTensorBufts = []string{"CPU"}
	enrichReloadPayload(payloadMap, strategy)

	// Marshal
	raw, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode в зеркало cppworker struct. Decoder с DisallowUnknownFields
	// (так настроен cppworker's json.NewDecoder — см. handleReloadModel).
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var mirror cppworkerReloadRequestMirror
	if err := dec.Decode(&mirror); err != nil {
		t.Errorf("cppworker would REJECT this payload with HTTP 400: %v\npayload: %s",
			err, string(raw))
	}
}

// TestEnrichReloadPayload_NoAdaptiveStageField — guard-тест что мы НЕ
// возвращаем `adaptiveStage` (балансер-внутренняя метадата, не для cppworker).
func TestEnrichReloadPayload_NoAdaptiveStageField(t *testing.T) {
	strategy := &AdaptiveStrategy{
		Stage: "exact_fit",
	}
	payload := map[string]interface{}{"name": "x"}
	enrichReloadPayload(payload, strategy)
	if _, ok := payload["adaptiveStage"]; ok {
		t.Errorf("enrichReloadPayload added 'adaptiveStage' — cppworker will reject with HTTP 400. payload: %v", payload)
	}
}

// TestEnrichReloadPayload_NoReasonField — guard-тест что мы НЕ возвращаем
// `reason` (cppworker's reloadModelRequest struct не имеет этого поля;
// был добавлен только в loadModelRequest в R44.1).
func TestEnrichReloadPayload_NoReasonField(t *testing.T) {
	strategy := &AdaptiveStrategy{Stage: "exact_fit"}
	payload := map[string]interface{}{"name": "x"}
	enrichReloadPayload(payload, strategy)
	if _, ok := payload["reason"]; ok {
		t.Errorf("enrichReloadPayload added 'reason' — cppworker's reloadModelRequest struct does NOT have it (loadModelRequest does, but not reload). payload: %v", payload)
	}
}
