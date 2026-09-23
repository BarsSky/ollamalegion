//go:build llama_stub

// backend_model_op_request_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС на класс багов «поле есть в ModelOpRequest, но HTTP-слой его не
// декодирует»: balancer.ModelOpRequest умеет BatchSize / FlashAttn / UseMmap /
// KVCacheType (executeLlamaCppLoad прокидывает их в cppworker
// /api/models/load), но inline-структура в executeBackendModelOp их не
// содержала, поэтому значения молча терялись.
//
// Симптом для пользователя: из WebUI нельзя применить настройки модели —
// batch_size / flash_attn / use_mmap / kv_cache_type не доходили до cppworker,
// работали только contextSize и gpuLayers.
//
// Тест проверяет именно маппинг JSON → ModelOpRequest, поэтому падает, если
// кто-то добавит поле в ModelOpRequest и забудет про HTTP-слой (или наоборот).
package api

import (
	"encoding/json"
	"testing"
)

// TestBackendModelOpRequest_AllLoadParamsMapped — все параметры загрузки,
// которые принимает cppworker, должны переживать декодирование тела запроса и
// попадать в balancer.ModelOpRequest.
func TestBackendModelOpRequest_AllLoadParamsMapped(t *testing.T) {
	// Полный набор полей, который отправляет WebUI (GgufApi.manageModel).
	raw := `{
		"operation": "load",
		"modelName": "gemma-4-E4B-it-Q4_K_M",
		"contextSize": 32768,
		"gpuLayers": -1,
		"batchSize": 1024,
		"flashAttn": 1,
		"useMmap": false,
		"kvCacheType": "q8_0",
		"overrideTensors": ["blk\\.\\d+\\.ffn_.*_exps\\.weight"],
		"overrideTensorBufts": ["CPU"],
		"insecure": true,
		"stream": true,
		"force": true
	}`

	var req backendModelOpRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := req.toModelOpRequest()

	if got.Operation != "load" {
		t.Errorf("Operation = %q, want load", got.Operation)
	}
	if got.ModelName != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("ModelName = %q", got.ModelName)
	}
	// contextSize / gpuLayers — были и раньше, проверяем чтобы рефакторинг
	// (вынос структуры) их не потерял.
	if got.ContextSize == nil || *got.ContextSize != 32768 {
		t.Errorf("ContextSize = %v, want 32768", got.ContextSize)
	}
	if got.GPULayers == nil || *got.GPULayers != -1 {
		t.Errorf("GPULayers = %v, want -1", got.GPULayers)
	}
	// Новые (R66d) поля — их раньше не было в HTTP-слое.
	if got.BatchSize == nil || *got.BatchSize != 1024 {
		t.Errorf("BatchSize = %v, want 1024 — HTTP-слой снова теряет batchSize", got.BatchSize)
	}
	if got.FlashAttn == nil || *got.FlashAttn != 1 {
		t.Errorf("FlashAttn = %v, want 1 — HTTP-слой снова теряет flashAttn", got.FlashAttn)
	}
	if got.UseMmap == nil || *got.UseMmap != false {
		t.Errorf("UseMmap = %v, want false — HTTP-слой снова теряет useMmap", got.UseMmap)
	}
	if got.KVCacheType == nil || *got.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType = %v, want q8_0 — HTTP-слой снова теряет kvCacheType", got.KVCacheType)
	}
	if len(got.OverrideTensors) != 1 || len(got.OverrideTensorBufts) != 1 {
		t.Errorf("override tensors lost: %v / %v", got.OverrideTensors, got.OverrideTensorBufts)
	}
	if !got.Insecure || !got.Stream {
		t.Errorf("Insecure=%v Stream=%v, want true/true", got.Insecure, got.Stream)
	}
	if got.Force == nil || !*got.Force {
		t.Errorf("Force = %v, want true", got.Force)
	}
}

// TestBackendModelOpRequest_OmittedParamsStayNil — если поле не пришло, оно
// должно остаться nil: cppworker тогда применит свой дефолт/профиль.
// Нулевые значения вместо nil («flashAttn: 0» = выключить) сломали бы
// приоритет «явное > профиль > дефолт» в executeLlamaCppLoad.
func TestBackendModelOpRequest_OmittedParamsStayNil(t *testing.T) {
	var req backendModelOpRequest
	if err := json.Unmarshal([]byte(`{"operation":"load","modelName":"m"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := req.toModelOpRequest()

	if got.ContextSize != nil {
		t.Errorf("ContextSize = %v, want nil", *got.ContextSize)
	}
	if got.GPULayers != nil {
		t.Errorf("GPULayers = %v, want nil", *got.GPULayers)
	}
	if got.BatchSize != nil {
		t.Errorf("BatchSize = %v, want nil", *got.BatchSize)
	}
	if got.FlashAttn != nil {
		t.Errorf("FlashAttn = %v, want nil", *got.FlashAttn)
	}
	if got.UseMmap != nil {
		t.Errorf("UseMmap = %v, want nil", *got.UseMmap)
	}
	if got.KVCacheType != nil {
		t.Errorf("KVCacheType = %v, want nil", *got.KVCacheType)
	}
	if got.Force != nil {
		t.Errorf("Force = %v, want nil", *got.Force)
	}
}

// TestClusterReloadModelRequest_AllLoadParamsMapped — тот же класс багов на
// пути reload (POST /api/v1/cluster/models/{name}/reload): «перезагрузить с
// новыми настройками» тоже теряло batch/flash/mmap/kv-cache.
func TestClusterReloadModelRequest_AllLoadParamsMapped(t *testing.T) {
	raw := `{
		"operation": "reload",
		"backendId": "b1",
		"contextSize": 65536,
		"gpuLayers": -2,
		"batchSize": 2048,
		"flashAttn": 0,
		"useMmap": true,
		"kvCacheType": "f16",
		"force": true,
		"reason": "ui test"
	}`
	var req clusterReloadModelRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := req.toModelOpRequest("gemma-4", "load")

	if got.Operation != "load" || got.ModelName != "gemma-4" {
		t.Errorf("Operation/ModelName = %q/%q, want load/gemma-4", got.Operation, got.ModelName)
	}
	if got.ContextSize == nil || *got.ContextSize != 65536 {
		t.Errorf("ContextSize = %v, want 65536", got.ContextSize)
	}
	if got.GPULayers == nil || *got.GPULayers != -2 {
		t.Errorf("GPULayers = %v, want -2", got.GPULayers)
	}
	if got.BatchSize == nil || *got.BatchSize != 2048 {
		t.Errorf("BatchSize = %v, want 2048", got.BatchSize)
	}
	if got.FlashAttn == nil || *got.FlashAttn != 0 {
		t.Errorf("FlashAttn = %v, want 0 (явное выключение)", got.FlashAttn)
	}
	if got.UseMmap == nil || !*got.UseMmap {
		t.Errorf("UseMmap = %v, want true", got.UseMmap)
	}
	if got.KVCacheType == nil || *got.KVCacheType != "f16" {
		t.Errorf("KVCacheType = %v, want f16", got.KVCacheType)
	}
	if got.Force == nil || !*got.Force {
		t.Errorf("Force = %v, want true", got.Force)
	}
}
