// load_body_contract_r83_test.go — R83/v67 (2026-10-02).
//
// Контракт тела загрузки: балансер → cppworker POST /api/models/load.
//
// ПОЧЕМУ ЭТОТ ТЕСТ СУЩЕСТВУЕТ. cppworker декодирует тело строгим декодером
// (pkg/types.DecodeJSONRequest → DisallowUnknownFields). Поэтому «лишний» ключ
// в теле — это не «поле проигнорируется», а HTTP 400 invalid JSON и ПОЛНЫЙ
// отказ загрузки. Ровно так упала загрузка gemma-4, когда балансер начал
// отправлять enableReasoning (R83/v62), а cppworker его ещё не принимал:
//
//	cppworker error (HTTP 400): invalid JSON: json: unknown field "enableReasoning"
//	ensureModelLoadedOnBackend: async load failed
//
// Тест держит список ключей синхронным: если кто-то добавит поле в тело и
// забудет принять его в cmd/cppworker/types.go, здесь упадёт
// TestLlamaCppLoadBody_KeyContract, а на стороне cppworker —
// TestBalancerLoadBody_AcceptedByLegacyAndParamsEndpoints.
package balancer

import (
	"reflect"
	"sort"
	"testing"
)

// fullLoadRequest возвращает ModelOpRequest со ВСЕМИ полями, которые попадают
// в тело загрузки (nil-поля в тело не кладутся — их проверяет отдельный тест).
func fullLoadRequest() ModelOpRequest {
	ctxSize := 65536
	gpuLayers := -1
	kv := "q4_0"
	useMmap := true
	flashAttn := 1
	batchSize := 512
	parallel := 1
	perSeq := 32768
	reasoning := true
	return ModelOpRequest{
		ModelName:       "gemma-4-E4B-it-Q4_K_M",
		ContextSize:     &ctxSize,
		GPULayers:       &gpuLayers,
		KVCacheType:     &kv,
		UseMmap:         &useMmap,
		FlashAttn:       &flashAttn,
		BatchSize:       &batchSize,
		Parallel:        &parallel,
		ContextPerSeq:   &perSeq,
		EnableReasoning: &reasoning,
	}
}

// TestLlamaCppLoadBody_KeyContract — набор ключей тела совпадает с
// объявленным контрактом (llamaCppLoadBodyKeys) и содержит enableReasoning.
func TestLlamaCppLoadBody_KeyContract(t *testing.T) {
	body := buildLlamaCppLoadBody(fullLoadRequest(), []string{"blk.*=CPU"}, []string{"CPU"}, true)

	got := make([]string, 0, len(body))
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)

	want := append([]string(nil), llamaCppLoadBodyKeys...)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("набор ключей тела загрузки разошёлся с контрактом:\n got: %v\nwant: %v\n"+
			"Если ключ добавлен осознанно — прими поле в cmd/cppworker/types.go "+
			"(loadModelRequest И loadWithParamsRequest) и обнови llamaCppLoadBodyKeys.", got, want)
	}
}

// TestLlamaCppLoadBody_ReasoningPresent — регресс-тест на живую жалобу
// «флаг включить размышление был активен, однако размышление не применилось»:
// enableReasoning обязан доезжать до cppworker и на legacy /api/models/load
// (useLoadWithParams=false), а не только на /load-with-params.
func TestLlamaCppLoadBody_ReasoningPresent(t *testing.T) {
	req := fullLoadRequest()
	for _, useParams := range []bool{false, true} {
		body := buildLlamaCppLoadBody(req, nil, nil, useParams)
		v, ok := body["enableReasoning"]
		if !ok {
			t.Fatalf("useLoadWithParams=%v: enableReasoning отсутствует в теле загрузки", useParams)
		}
		if b, isBool := v.(bool); !isBool || !b {
			t.Fatalf("useLoadWithParams=%v: enableReasoning = %#v, want true", useParams, v)
		}
	}
}

// TestLlamaCppLoadBody_Minimal — без параметров тело содержит только name:
// cppworker тогда применяет свои env/config-дефолты, а не нули.
func TestLlamaCppLoadBody_Minimal(t *testing.T) {
	body := buildLlamaCppLoadBody(ModelOpRequest{ModelName: "m"}, nil, nil, false)
	if len(body) != 1 {
		t.Fatalf("минимальное тело должно содержать только name, got %v", body)
	}
	if body["name"] != "m" {
		t.Fatalf("name = %#v, want \"m\"", body["name"])
	}
}

// TestLlamaCppLoadBody_ContextPerSeqOnlyPositive — нулевой contextPerSeq не
// уходит в тело (0 значило бы «не задано», но cppworker валидирует значение
// как окно на клиента; отрицательное/нулевое не должно туда попадать).
func TestLlamaCppLoadBody_ContextPerSeqOnlyPositive(t *testing.T) {
	zero := 0
	body := buildLlamaCppLoadBody(ModelOpRequest{ModelName: "m", ContextPerSeq: &zero}, nil, nil, false)
	if _, ok := body["contextPerSeq"]; ok {
		t.Fatalf("contextPerSeq=0 не должен попадать в тело: %v", body)
	}
}

// TestLlamaCppLoadBody_OverrideTensorsOnlyWithParams — override-tensors уходят
// только на /load-with-params: на legacy эндпоинте их просто нет.
func TestLlamaCppLoadBody_OverrideTensorsOnlyWithParams(t *testing.T) {
	legacy := buildLlamaCppLoadBody(ModelOpRequest{ModelName: "m"}, []string{"blk.*=CPU"}, []string{"CPU"}, false)
	if _, ok := legacy["overrideTensors"]; ok {
		t.Fatalf("overrideTensors не должны уходить на legacy /api/models/load: %v", legacy)
	}
	params := buildLlamaCppLoadBody(ModelOpRequest{ModelName: "m"}, []string{"blk.*=CPU"}, []string{"CPU"}, true)
	if _, ok := params["overrideTensors"]; !ok {
		t.Fatalf("overrideTensors обязаны уходить на /load-with-params: %v", params)
	}
}
