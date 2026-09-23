//go:build llama_stub

// profile_update_r67a_test.go — R67a (2026-09-23).
//
// РЕГРЕСС на «n_ctx жёстко 32768 при свободной VRAM» на мощном GPU.
//
// Воспроизведено на живом стеке:
//  1. профиль с contextLengthAuto=true, contextLengthMax=131072, kvCacheType=q4_0,
//     flashAttn=true, streamingTimeoutSec=1800;
//  2. сохранение профиля как это делает WebUI-форма (шлёт только
//     contextLength/batchSize/numGpuLayers/notes) → HTTP 200 OK;
//  3. GET: contextLengthAuto СБРОШЕН, contextLengthMax СБРОШЕН, kvCacheType "",
//     flashAttn nil, streamingTimeoutSec 0.
//
// Последствие: без contextLengthAuto профиль снова ЖЁСТКИЙ потолок n_ctx
// (resolveModelMaxContext tier 2 → profile.ContextLength), поэтому клиент с
// num_ctx=65536/131072 получал 32768 независимо от свободной VRAM; сброшенный
// kvCacheType завышал KV-cache (f16 вместо q4_0) и занижал feasible n_ctx.
//
// Плюс: частичное обновление (PATCH-like) отклонялось с 400
// «contextLength is required and must be > 0».
package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func boolPtrR67a(b bool) *bool { return &b }
func intPtrR67a(i int) *int    { return &i }

// TestProfilePresenceJSON_DecodesAutoMaxPointers — profilePresence различает
// «не передано» (nil) и «передано false/0», а обычные поля идут в
// types.LlamaCppModelProfile (тело декодируется дважды).
func TestProfilePresenceJSON_DecodesAutoMaxPointers(t *testing.T) {
	body := `{"contextLength":32768,"batchSize":512,"numGpuLayers":-1,
	          "contextLengthAuto":true,"contextLengthMax":131072,
	          "kvCacheType":"q4_0","flashAttn":true,
	          "streamingTimeoutSec":1800,"notes":"n"}`

	var upd types.LlamaCppModelProfile
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&upd); err != nil {
		t.Fatalf("decode values: %v", err)
	}
	var presence profilePresence
	if err := json.Unmarshal([]byte(body), &presence); err != nil {
		t.Fatalf("decode presence: %v", err)
	}

	if presence.ContextLengthAuto == nil || !*presence.ContextLengthAuto {
		t.Errorf("presence ContextLengthAuto = %v, want true", presence.ContextLengthAuto)
	}
	if presence.ContextLengthMax == nil || *presence.ContextLengthMax != 131072 {
		t.Errorf("presence ContextLengthMax = %v, want 131072", presence.ContextLengthMax)
	}
	if upd.ContextLength != 32768 {
		t.Errorf("ContextLength = %d, want 32768", upd.ContextLength)
	}
	if upd.BatchSize != 512 {
		t.Errorf("BatchSize = %d, want 512", upd.BatchSize)
	}
	if upd.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0", upd.KVCacheType)
	}
	if upd.FlashAttn == nil || !*upd.FlashAttn {
		t.Errorf("FlashAttn = %v, want true", upd.FlashAttn)
	}
	if upd.StreamingTimeoutSec != 1800 {
		t.Errorf("StreamingTimeoutSec = %d, want 1800", upd.StreamingTimeoutSec)
	}
}

// TestProfilePresenceJSON_AbsentFieldsAreNil — если поля не переданы, указатели
// остаются nil, то есть мерж их не трогает (прежнее значение сохраняется).
func TestProfilePresenceJSON_AbsentFieldsAreNil(t *testing.T) {
	var presence profilePresence
	if err := json.Unmarshal([]byte(`{"contextLength":8192}`), &presence); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if presence.ContextLengthAuto != nil {
		t.Errorf("ContextLengthAuto = %v, want nil (не передано)", *presence.ContextLengthAuto)
	}
	if presence.ContextLengthMax != nil {
		t.Errorf("ContextLengthMax = %v, want nil (не передано)", *presence.ContextLengthMax)
	}
	if presence.Disabled != nil {
		t.Errorf("Disabled = %v, want nil (не передано)", *presence.Disabled)
	}
}

// TestMergeProfileUpdate_NewProfileKeepsTimeouts — СОЗДАНИЕ профиля (existing
// нулевой) не должно терять поля: раньше create шёл через mergeModelProfile,
// который не знал про таймауты/maxTokens, и они пропадали (живая проверка:
// streamingTimeoutSec=1800 после PUT отдавал 0).
func TestMergeProfileUpdate_NewProfileKeepsTimeouts(t *testing.T) {
	created := mergeProfileUpdate(types.LlamaCppModelProfile{}, types.LlamaCppModelProfile{
		ContextLength:       32768,
		StreamingTimeoutSec: 1800,
		MaxTokens:           4096,
		FirstByteTimeoutSec: 300,
	}, profilePresence{ContextLengthAuto: boolPtrR67a(true)})

	if created.StreamingTimeoutSec != 1800 || created.MaxTokens != 4096 || created.FirstByteTimeoutSec != 300 {
		t.Fatalf("создание профиля теряет таймауты/maxTokens: %d/%d/%d",
			created.StreamingTimeoutSec, created.MaxTokens, created.FirstByteTimeoutSec)
	}
	if !created.ContextLengthAuto {
		t.Error("contextLengthAuto не применился при создании профиля")
	}
}

// TestMergeProfileUpdate_WebUISaveKeepsEverythingElse — главный регресс:
// сохранение из UI (только ctx/batch/gpu/notes) не должно обнулять
// contextLengthAuto/Max, kvCacheType, flashAttn, numa, useMmap и таймауты.
func TestMergeProfileUpdate_WebUISaveKeepsEverythingElse(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength:           32768,
		BatchSize:               512,
		NumGPULayers:            -2,
		ContextLengthAuto:       true,
		ContextLengthMax:        131072,
		KVCacheType:             "q4_0",
		FlashAttn:               boolPtrR67a(true),
		NUMA:                    boolPtrR67a(true),
		UseMmap:                 boolPtrR67a(true),
		Parallel:                4,
		StreamingTimeoutSec:     1800,
		StreamingIdleTimeoutSec: 600,
		RequestTimeoutSec:       900,
		Notes:                   "старый",
	}
	// Ровно то, что отправляет webui/js/modules/cppworker-params.js.
	values := types.LlamaCppModelProfile{
		ContextLength: 32768,
		BatchSize:     256,
		NumGPULayers:  -1,
		Notes:         "edited in UI",
	}

	got := mergeProfileUpdate(existing, values, profilePresence{})

	if !got.ContextLengthAuto {
		t.Error("contextLengthAuto потерян: профиль снова станет ЖЁСТКИМ потолком n_ctx")
	}
	if got.ContextLengthMax != 131072 {
		t.Errorf("contextLengthMax = %d, want 131072", got.ContextLengthMax)
	}
	if got.KVCacheType != "q4_0" {
		t.Errorf("kvCacheType = %q, want q4_0 (сброс → f16 KV → завышенный KV-cache)", got.KVCacheType)
	}
	if got.FlashAttn == nil || !*got.FlashAttn {
		t.Errorf("flashAttn = %v, want true", got.FlashAttn)
	}
	if got.NUMA == nil || !*got.NUMA {
		t.Errorf("numa = %v, want true", got.NUMA)
	}
	if got.UseMmap == nil || !*got.UseMmap {
		t.Errorf("useMmap = %v, want true", got.UseMmap)
	}
	if got.Parallel != 4 {
		t.Errorf("parallel = %d, want 4", got.Parallel)
	}
	if got.StreamingTimeoutSec != 1800 || got.StreamingIdleTimeoutSec != 600 || got.RequestTimeoutSec != 900 {
		t.Errorf("per-model таймауты потеряны: %d/%d/%d",
			got.StreamingTimeoutSec, got.StreamingIdleTimeoutSec, got.RequestTimeoutSec)
	}
	if got.BatchSize != 256 || got.NumGPULayers != -1 || got.Notes != "edited in UI" {
		t.Errorf("изменённые поля не применились: batch=%d gpu=%d notes=%q",
			got.BatchSize, got.NumGPULayers, got.Notes)
	}
}

// TestMergeProfileUpdate_CanEnableAutoOnExisting — раньше было НЕВОЗМОЖНО
// включить авто-режим у существующего профиля: PUT заменял профиль телом, а
// частичное обновление отклонялось 400.
func TestMergeProfileUpdate_CanEnableAutoOnExisting(t *testing.T) {
	existing := types.LlamaCppModelProfile{ContextLength: 32768, BatchSize: 512}
	got := mergeProfileUpdate(existing, types.LlamaCppModelProfile{}, profilePresence{
		ContextLengthAuto: boolPtrR67a(true),
		ContextLengthMax:  intPtrR67a(131072),
	})

	if !got.ContextLengthAuto || got.ContextLengthMax != 131072 {
		t.Fatalf("не удалось включить auto-режим: auto=%v max=%d", got.ContextLengthAuto, got.ContextLengthMax)
	}
	if got.ContextLength != 32768 || got.BatchSize != 512 {
		t.Errorf("существующие поля изменились: ctx=%d batch=%d", got.ContextLength, got.BatchSize)
	}
}

// TestMergeProfileUpdate_ExplicitFalseAndZeroClear — «передано false/0» должно
// ОЧИЩАТЬ значения (оператор может снять потолок, выключить auto и снять disabled).
func TestMergeProfileUpdate_ExplicitFalseAndZeroClear(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength:     32768,
		ContextLengthAuto: true,
		ContextLengthMax:  131072,
		Disabled:          true,
	}
	got := mergeProfileUpdate(existing, types.LlamaCppModelProfile{}, profilePresence{
		ContextLengthAuto: boolPtrR67a(false),
		ContextLengthMax:  intPtrR67a(0),
		Disabled:          boolPtrR67a(false),
	})

	if got.ContextLengthAuto {
		t.Error("contextLengthAuto=false не применился")
	}
	if got.ContextLengthMax != 0 {
		t.Errorf("contextLengthMax=0 не применился: %d", got.ContextLengthMax)
	}
	if got.Disabled {
		t.Error("disabled=false не применился (модель осталась заблокированной)")
	}
}

// TestMergeProfileUpdate_NewProfileRequiresContextLength — для нового профиля
// contextLength по-прежнему обязателен (400 с понятным сообщением), но частичное
// обновление существующего больше не падает.
func TestMergeProfileUpdate_NewProfileRequiresContextLength(t *testing.T) {
	merged := mergeProfileUpdate(types.LlamaCppModelProfile{}, types.LlamaCppModelProfile{},
		profilePresence{ContextLengthAuto: boolPtrR67a(true)})
	if err := validateModelProfile(merged); err == nil {
		t.Fatal("новый профиль без contextLength должен быть отклонён")
	}
	if merged.ContextLength != 0 {
		t.Errorf("ContextLength = %d, want 0", merged.ContextLength)
	}
}

// TestMergeProfileUpdate_BoolPointerFieldsPreserved — указательные поля профиля
// (AutoTune/FlashAttn) не должны теряться при частичном обновлении, иначе
// UI-сохранение снова начнёт «обнулять» настройки.
func TestMergeProfileUpdate_BoolPointerFieldsPreserved(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength: 8192,
		AutoTune:      boolPtrR67a(false),
		FlashAttn:     boolPtrR67a(true),
		MaxTokens:     4096,
	}
	got := mergeProfileUpdate(existing, types.LlamaCppModelProfile{ContextLength: 8192, Notes: "x"}, profilePresence{})

	if got.AutoTune == nil || *got.AutoTune {
		t.Errorf("AutoTune потерян/изменён: %v", got.AutoTune)
	}
	if got.FlashAttn == nil || !*got.FlashAttn {
		t.Errorf("FlashAttn потерян: %v", got.FlashAttn)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", got.MaxTokens)
	}
}
