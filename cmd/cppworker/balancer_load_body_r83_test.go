// balancer_load_body_r83_test.go — R83/v67 (2026-10-02).
//
// Зеркальная половина контракта «балансер → cppworker» (см.
// internal/balancer/load_body_contract_r83_test.go и llamaCppLoadBodyKeys).
//
// cppworker декодирует тело загрузки СТРОГИМ декодером
// (pkg/types.DecodeJSONRequest → DisallowUnknownFields). Значит, ключ, который
// балансер положил в тело, а эти структуры не знают, — это НЕ «поле
// проигнорируется», а HTTP 400 `invalid JSON: json: unknown field "..."` и
// полный отказ загрузки модели.
//
// Живой инцидент (2026-10-02): профиль gemma-4 в WebUI с галочкой «включить
// размышления» → балансер отправил enableReasoning на legacy /api/models/load
// → loadModelRequest его не знал → 400 → модель не поднималась вообще.
//
// Этот тест фиксирует СПИСОК ключей тела загрузки. Он продублирован здесь
// намеренно (пакет main не может импортировать internal/balancer без
// вытаскивания всего балансера в бинарь cppworker), поэтому при изменении
// балансерного тела нужно править оба списка — оба теста упадут.
package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// balancerLoadBodyKeys — ключи, которые балансер кладёт в тело POST
// /api/models/load. Держать синхронно с internal/balancer
// llamaCppLoadBodyKeys (buildLlamaCppLoadBody).
var balancerLoadBodyKeys = []string{
	"name",
	"contextSize",
	"gpuLayers",
	"kvCacheType",
	"useMmap",
	"flashAttn",
	"batchSize",
	"parallel",
	"contextPerSeq",
	"enableReasoning",
	"overrideTensors",
	"overrideTensorBufts",
}

// balancerLoadBodyJSON собирает тело ровно так, как это делает балансер:
// все числовые/строковые поля заданы (граничный случай — «профиль заполнил
// всё»), override-tensors присутствуют (граничный случай load-with-params).
func balancerLoadBodyJSON(t *testing.T) []byte {
	t.Helper()
	body := map[string]interface{}{
		"name":                "gemma-4-E4B-it-Q4_K_M",
		"contextSize":         65536,
		"gpuLayers":           -1,
		"kvCacheType":         "q4_0",
		"useMmap":             true,
		"flashAttn":           1,
		"batchSize":           512,
		"parallel":            1,
		"contextPerSeq":       32768,
		"enableReasoning":     true,
		"overrideTensors":     []string{"blk.*=CPU"},
		"overrideTensorBufts": []string{"CPU"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Список ключей в тесте должен совпадать с тем, что реально отправлено.
	if len(body) != len(balancerLoadBodyKeys) {
		t.Fatalf("тестовое тело содержит %d ключей, а balancerLoadBodyKeys — %d; "+
			"обнови оба списка (этот и internal/balancer llamaCppLoadBodyKeys)",
			len(body), len(balancerLoadBodyKeys))
	}
	for _, k := range balancerLoadBodyKeys {
		if _, ok := body[k]; !ok {
			t.Fatalf("ключ %q из balancerLoadBodyKeys отсутствует в тестовом теле", k)
		}
	}
	return raw
}

// decodeStrict повторяет ровно то, что делает types.DecodeJSONRequest:
// json.Decoder с DisallowUnknownFields.
func decodeStrict(t *testing.T, raw []byte, dst interface{}) error {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// TestBalancerLoadBody_AcceptedByLegacyAndParamsEndpoints — тело балансера
// обязано приниматься ОБОИМ эндпоинтам загрузки: legacy /api/models/load
// (loadModelRequest) и /api/models/load-with-params (loadWithParamsRequest).
func TestBalancerLoadBody_AcceptedByLegacyAndParamsEndpoints(t *testing.T) {
	raw := balancerLoadBodyJSON(t)

	var legacy loadModelRequest
	if err := decodeStrict(t, raw, &legacy); err != nil {
		t.Fatalf("legacy /api/models/load отверг тело балансера: %v\n"+
			"Ровно это превращается в 400 и полностью ломает авто-загрузку модели "+
			"(см. инцидент enableReasoning 2026-10-02).", err)
	}
	if legacy.EnableReasoning == nil || !*legacy.EnableReasoning {
		t.Fatalf("legacy: enableReasoning = %v, want true", legacy.EnableReasoning)
	}
	if legacy.Parallel == nil || *legacy.Parallel != 1 {
		t.Fatalf("legacy: parallel = %v, want 1", legacy.Parallel)
	}
	if legacy.KVCacheType == nil || *legacy.KVCacheType != "q4_0" {
		t.Fatalf("legacy: kvCacheType = %v, want q4_0", legacy.KVCacheType)
	}

	var params loadWithParamsRequest
	if err := decodeStrict(t, raw, &params); err != nil {
		t.Fatalf("/api/models/load-with-params отверг тело балансера: %v", err)
	}
	if params.EnableReasoning == nil || !*params.EnableReasoning {
		t.Fatalf("load-with-params: enableReasoning = %v, want true", params.EnableReasoning)
	}
}

// TestBalancerLoadBody_ReasoningReachesOpts — значение из тела доезжает до
// LoadModelOpts так же, как это делает handleLoadModel: иначе галочка
// «включить размышления» в WebUI сохраняется, но модель грузится как раньше
// (живая жалоба оператора).
func TestBalancerLoadBody_ReasoningReachesOpts(t *testing.T) {
	raw := balancerLoadBodyJSON(t)

	var legacy loadModelRequest
	if err := decodeStrict(t, raw, &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}

	// handleLoadModel: opts.EnableReasoning = req.EnableReasoning.
	opts := cppbackend.LoadModelOpts{}
	if legacy.EnableReasoning != nil {
		opts.EnableReasoning = legacy.EnableReasoning
	}
	if opts.EnableReasoning == nil || !*opts.EnableReasoning {
		t.Fatalf("legacy: opts.EnableReasoning = %v, want true", opts.EnableReasoning)
	}
}

// TestCurrentReasoningPtr_ReloadKeepsFlag — reload обязан сохранять уже
// действующий per-model reasoning: раньше opts.EnableReasoning оставался nil,
// cppbackend падал на cfg.DefaultEnableReasoning (false), и ЛЮБАЯ перезагрузка
// (preflight n_ctx, смена окна) молча выключала размышления.
func TestCurrentReasoningPtr_ReloadKeepsFlag(t *testing.T) {
	if got := currentReasoningPtr(nil); got != nil {
		t.Fatalf("currentReasoningPtr(nil) = %v, want nil", got)
	}
	on := currentReasoningPtr(&cppbackend.ModelInfo{ReasoningEnabled: true})
	if on == nil || !*on {
		t.Fatalf("currentReasoningPtr(on) = %v, want &true", on)
	}
	off := currentReasoningPtr(&cppbackend.ModelInfo{ReasoningEnabled: false})
	if off == nil || *off {
		t.Fatalf("currentReasoningPtr(off) = %v, want &false (не nil: nil = наследовать дефолт)", off)
	}
}

// TestBalancerLoadBody_StrictDecoderStillRejectsUnknown — сам страж обязан
// оставаться строгим: поле вне контракта обязано давать ошибку. Иначе тест
// выше перестанет что-либо доказывать.
func TestBalancerLoadBody_StrictDecoderStillRejectsUnknown(t *testing.T) {
	raw := []byte(`{"name":"m","fieldThatDoesNotExist":1}`)
	var legacy loadModelRequest
	if err := decodeStrict(t, raw, &legacy); err == nil {
		t.Fatal("строгий декодер принял неизвестное поле — контракт больше не проверяется")
	}
}
