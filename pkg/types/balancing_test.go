// balancing_test.go — R60.18 F2: tests for removed StreamingMaxDuration field.
//
// Background: до R60.18 `streamingMaxDuration` был в BalancingSettings type
// как documented config field, но НИКОГДА не читался кодом. Операторы, которые
// писали `streamingMaxDuration: 1800` в config.json, получали silent no-op.
//
// R60.18 F2 fix: REMOVED field из типа. Этот test проверяет:
//  1. JSON с полем парсится (поле silently ignored, как у любых других unknown fields)
//  2. JSON без поля парсится (default values)
//  3. Структура config valid как раньше
package types

import (
	"encoding/json"
	"testing"
)

// TestBalancingSettings_StreamingMaxDurationRemoved —
// config может содержать streamingMaxDuration, оно просто silently ignored.
// Это backward-compat поведение, чтобы существующие конфиги не падали.
func TestBalancingSettings_StreamingMaxDurationRemoved(t *testing.T) {
	// JSON со старым полем (для backward compat — операторы не должны
	// получать ошибку при обновлении cppworker).
	input := []byte(`{
		"useEnhancedScoring": true,
		"streamingMaxDuration": 1800
	}`)

	var b BalancingSettings
	if err := json.Unmarshal(input, &b); err != nil {
		t.Errorf("Unmarshal с streamingMaxDuration должен пройти без ошибки: %v", err)
	}
	// Поле отсутствует в типе → default value (false для bool, 0 для int).
	// Главное что парсинг НЕ падает.
}

// TestBalancingSettings_NoStreamingMaxDuration_ValidConfig —
// нормальный config без deprecated поля парсится корректно.
func TestBalancingSettings_NoStreamingMaxDuration_ValidConfig(t *testing.T) {
	input := []byte(`{
		"useEnhancedScoring": true,
		"modelLoadTimeout": 900,
		"requestTimeout": 600
	}`)

	var b BalancingSettings
	if err := json.Unmarshal(input, &b); err != nil {
		t.Errorf("Unmarshal без streamingMaxDuration: %v", err)
	}
	if !b.UseEnhancedScoring {
		t.Errorf("UseEnhancedScoring не парсится")
	}
	if b.ModelLoadTimeout != 900 {
		t.Errorf("ModelLoadTimeout = %d, want 900", b.ModelLoadTimeout)
	}
	if b.RequestTimeout != 600 {
		t.Errorf("RequestTimeout = %d, want 600", b.RequestTimeout)
	}
}
