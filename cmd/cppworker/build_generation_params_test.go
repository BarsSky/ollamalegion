// build_generation_params_test.go — Unit tests for buildGenerationParams / normalizeGenerateRequest.
//
// Round 16 follow-up fix (2026-07-30): *float64/*int в generateRequest чтобы
// отличить "клиент не задал поле" (nil) от "клиент явно задал 0" (*0.0).
// Раньше `if > 0` ИГНОРИРОВАЛО temperature=0 от Cline/Aider/Continue →
// подставлялся дефолт cppworker (0.7) → не-greedy sampling для tool calls.
//
// Эти тесты ЗАЩИЩАЮТ от регрессии: если кто-то опять поменяет pointer обратно
// на float64 + `if > 0`, все тесты ниже упадут.
package main

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestBuildGenerationParams_TemperatureZero — самый важный тест:
//   client шлёт temperature=0 → params.Temperature ДОЛЖЕН быть 0 (greedy),
//   НЕ default 0.7.
func TestBuildGenerationParams_TemperatureZero(t *testing.T) {
	tempZero := 0.0
	req := generateRequest{Temperature: &tempZero}
	params := buildGenerationParams(req)
	if params.Temperature != 0.0 {
		t.Errorf("temperature=0 from client should be honored, got %v (default fallback bug?)", params.Temperature)
	}
}

// TestBuildGenerationParams_TemperatureUnset — клиент НЕ шлёт temperature:
//   params.Temperature должен быть default (0.7), НЕ 0.
func TestBuildGenerationParams_TemperatureUnset(t *testing.T) {
	req := generateRequest{} // no Temperature field set
	params := buildGenerationParams(req)
	def := bridge.DefaultGenerationParams()
	if params.Temperature != def.Temperature {
		t.Errorf("unset temperature should fall back to default %v, got %v", def.Temperature, params.Temperature)
	}
}

// TestBuildGenerationParams_TemperatureExplicit — клиент шлёт temperature=0.7:
//   params.Temperature должен быть 0.7, НЕ default.
func TestBuildGenerationParams_TemperatureExplicit(t *testing.T) {
	temp := 0.7
	req := generateRequest{Temperature: &temp}
	params := buildGenerationParams(req)
	if params.Temperature != 0.7 {
		t.Errorf("temperature=0.7 from client should be honored, got %v", params.Temperature)
	}
}

// TestBuildGenerationParams_TopPZero — top_p=0 означает "no top_p filter" в
// OpenAI-совместимом API. Раньше `if > 0` ИГНОРИРОВАЛО это → top_p=0.9 дефолт.
func TestBuildGenerationParams_TopPZero(t *testing.T) {
	topPZero := 0.0
	req := generateRequest{TopP: &topPZero}
	params := buildGenerationParams(req)
	if params.TopP != 0.0 {
		t.Errorf("top_p=0 from client should be honored (no top_p filter), got %v", params.TopP)
	}
}

// TestBuildGenerationParams_RepeatPenaltyOne — repeat_penalty=1.0 = no penalty
// (это стандартное значение, которое Cline не шлёт явно но иногда шлёт).
// Раньше `if > 0` (1.0 > 0 → true) → params.RepeatPenalty = 1.0. Это работало.
// Но `if > 0` (1.0 > 0 → true) корректно, а вот 0.5 (less than 1.0, allow repeats)
// НЕ работало бы раньше. Тест ниже проверяет что 0.5 пробрасывается.
func TestBuildGenerationParams_RepeatPenaltyHalf(t *testing.T) {
	rp := 0.5
	req := generateRequest{RepeatPenalty: &rp}
	params := buildGenerationParams(req)
	if params.RepeatPenalty != 0.5 {
		t.Errorf("repeat_penalty=0.5 (allow repeats) should be honored, got %v", params.RepeatPenalty)
	}
}

// TestNormalizeGenerateRequest_OptionsFallback — Ollama-формат
// (options: {temperature: 0.7}) должен конвертироваться в req.Temperature = &0.7
// даже если req.Temperature == nil.
func TestNormalizeGenerateRequest_OptionsFallback(t *testing.T) {
	req := &generateRequest{
		Options: generateOptions{Temperature: 0.7},
	}
	normalizeGenerateRequest(req)
	if req.Temperature == nil {
		t.Fatalf("Options.Temperature=0.7 should populate req.Temperature (nil after normalize)")
	}
	if *req.Temperature != 0.7 {
		t.Errorf("expected req.Temperature=0.7, got %v", *req.Temperature)
	}
}

// TestNormalizeGenerateRequest_OptionsZeroIgnored — Ollama-формат
// (options: {temperature: 0}) — если клиент не задал (0 = default в Go),
// мы НЕ должны заполнять req.Temperature (оставить nil → default 0.7).
//
// Раньше `if Options.Temperature > 0` означало "0 → не заполняем" — правильно
// для 0, но это случайно правильно. Сейчас мы явно проверяем != 0 — то же поведение.
func TestNormalizeGenerateRequest_OptionsZeroIgnored(t *testing.T) {
	req := &generateRequest{
		Options: generateOptions{Temperature: 0}, // "не задано" в Ollama-формате
	}
	normalizeGenerateRequest(req)
	if req.Temperature != nil {
		t.Errorf("Options.Temperature=0 should NOT populate req.Temperature (let default win), got %v", *req.Temperature)
	}
}

// TestNormalizeGenerateRequest_ExplicitZeroBeatsOptions — клиент явно шлёт
// temperature=0 (через req.Temperature = &0.0), И в options есть своё значение.
// Приоритет: explicit req.Temperature > Options.Temperature.
func TestNormalizeGenerateRequest_ExplicitZeroBeatsOptions(t *testing.T) {
	zero := 0.0
	optTemp := 0.7
	req := &generateRequest{
		Temperature: &zero,
		Options:     generateOptions{Temperature: optTemp},
	}
	normalizeGenerateRequest(req)
	if req.Temperature == nil {
		t.Fatalf("explicit req.Temperature=0 should be preserved")
	}
	if *req.Temperature != 0.0 {
		t.Errorf("explicit req.Temperature=0 should win over Options.Temperature=%v, got %v", optTemp, *req.Temperature)
	}
}
