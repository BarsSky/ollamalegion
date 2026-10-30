// llama_cpp_metrics_zero_omitempty_r342_test.go — R342 (2026-10-30).
//
// ЖИВОЙ ДЕФЕКТ (аудит docs/plans/2026-10-30-abort-and-metrics-audit.md). Блок
// `llamaCpp` отдаётся в `/api/v1/cluster` для КАЖДОГО бэкенда. У показываемых
// полей (`activeRequests`, `requestsPerSecond`, `avgResponseTime`, `freeSlots`,
// `availableSlots`) не было `omitempty`, поэтому ноль всегда присутствовал в
// JSON. WebUI проверяет `!== undefined`, то есть ноль считался НАСТОЯЩИМ
// значением и перекрывал правильные данные из `ollama.*` — Monitor во время
// генерации показывал «0 активных запросов» и RPS=0.
//
// Тест фиксирует контракт сериализации: нули этих полей НЕ попадают в JSON
// (значит фолбэк WebUI работает), а ненулевые значения — попадают.
package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR342_DisplayFieldsOmittedWhenZero — нули не сериализуются.
func TestR342_DisplayFieldsOmittedWhenZero(t *testing.T) {
	m := LlamaCppMetrics{}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonText := string(raw)
	for _, field := range []string{
		"activeRequests", "requestsPerSecond", "avgResponseTime", "freeSlots", "availableSlots",
	} {
		if strings.Contains(jsonText, `"`+field+`"`) {
			t.Errorf("поле %q сериализуется нулём: WebUI примет его за настоящее значение и перекроет ollama.* — Monitor покажет 0 (живой дефект): %s",
				field, jsonText)
		}
	}
}

// TestR342_DisplayFieldsPresentWhenSet — ненулевые значения обязаны доезжать.
func TestR342_DisplayFieldsPresentWhenSet(t *testing.T) {
	m := LlamaCppMetrics{
		ActiveRequests:    2,
		RequestsPerSecond: 1.5,
		AvgResponseTime:   800,
		FreeSlots:         3,
		AvailableSlots:    4,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]float64{
		"activeRequests":    2,
		"requestsPerSecond": 1.5,
		"avgResponseTime":   800,
		"freeSlots":         3,
		"availableSlots":    4,
	}
	for field, want := range cases {
		got, ok := decoded[field].(float64)
		if !ok {
			t.Errorf("поле %q отсутствует в JSON, хотя значение %v задано: %s", field, want, string(raw))
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}

// TestR342_CapacityFieldsStillSerialized — поля ёмкости НЕ трогаем: их читает
// планировщик, и пропажа нуля здесь означала бы «неизвестно» вместо «ноль слотов».
func TestR342_CapacityFieldsStillSerialized(t *testing.T) {
	raw, err := json.Marshal(LlamaCppMetrics{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonText := string(raw)
	for _, field := range []string{"totalRequests", "maxModels", "maxConcurrentReqs"} {
		if !strings.Contains(jsonText, `"`+field+`"`) {
			t.Errorf("поле ёмкости %q пропало из JSON: планировщик должен видеть ноль явно: %s", field, jsonText)
		}
	}
}
