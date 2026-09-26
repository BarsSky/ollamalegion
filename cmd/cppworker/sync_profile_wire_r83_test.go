//go:build llama_stub

// sync_profile_wire_r83_test.go — R83 §3.4 (2026-09-26): проводной формат профилей.
//
// Живой факт: ответ балансера `/api/v1/cppworker/model-profiles` содержит
// "Qwen3.8-27B": {..., "kvCacheType": "q8_0"}, а cppworker после применения
// профиля логировал profileKVCacheType="". Проверяем, доезжает ли поле через
// JSON-декодирование в types.LlamaCppModelProfile — то есть не теряется ли оно
// между балансером и воркером.
package main

import (
	"encoding/json"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_ProfileWire_KVCacheTypeDecodes — поле обязано декодироваться.
func TestR83_ProfileWire_KVCacheTypeDecodes(t *testing.T) {
	// Кусок реального ответа балансера (см. живой GET /api/v1/cppworker/model-profiles).
	raw := `{"models":{"Qwen3.8-27B":{"contextLength":32768,"batchSize":512,` +
		`"numGpuLayers":-2,"flashAttn":true,"useMmap":true,` +
		`"contextLengthAuto":true,"contextLengthMax":131072,"kvCacheType":"q8_0"}},` +
		`"total":1}`

	var body struct {
		Models map[string]types.LlamaCppModelProfile `json:"models"`
		Total  int                                   `json:"total"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	prof, ok := body.Models["Qwen3.8-27B"]
	if !ok {
		t.Fatal("профиль Qwen3.8-27B не декодирован")
	}
	if prof.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType = %q, want q8_0 — поле теряется на проводе "+
			"(проверьте json-тег в types.LlamaCppModelProfile)", prof.KVCacheType)
	}
	if prof.ContextLength != 32768 {
		t.Errorf("ContextLength = %d, want 32768", prof.ContextLength)
	}
}

// TestR83_ProfileWire_SyncerDecodePath — тот же JSON через тот же декодер, что в
// fetchOnce (структура тела ответа), а не «похожий» разбор.
func TestR83_ProfileWire_SyncerDecodePath(t *testing.T) {
	raw := `{"models":{"Qwen3.8-27B":{"contextLength":32768,"batchSize":512,` +
		`"numGpuLayers":-2,"kvCacheType":"q8_0"}},"total":1}`

	// Копия анонимной структуры из fetchOnce.
	var body struct {
		Models map[string]types.LlamaCppModelProfile `json:"models"`
		Total  int                                   `json:"total"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ps := &profileSyncerT{cache: body.Models}
	prof := ps.applyProfileOnLoad("qwen3.8:latest")
	if prof == nil {
		t.Fatal("профиль не найден после декодирования")
	}
	if prof.KVCacheType != "q8_0" {
		t.Errorf("после декодирования и резолва KVCacheType = %q, want q8_0", prof.KVCacheType)
	}
}
