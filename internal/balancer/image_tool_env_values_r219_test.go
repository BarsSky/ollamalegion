//go:build llama_stub

// image_tool_env_values_r219_test.go — R219 (2026-10-21).
//
// Живой дефект R216: LB_IMAGE_TOOL_INJECT=off молча означал always — разбор знал
// только auto. Балансер объявлял generate_image каждому запросу, воркер видел
// tools[] и уходил в буферизированную ветку /v1, где размышления приезжали
// клиенту как ответ. Тест держит полную матрицу значений, включая неизвестное
// (должно остаться «always», чтобы поведение не менялось молча).
package balancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestR219_ImageToolInjectValueMatrix(t *testing.T) {
	cases := []struct {
		value    string
		wantOff  bool
		wantAuto bool
	}{
		{"off", true, false},
		{"OFF", true, false},
		{" off ", true, false},
		{"false", true, false},
		{"no", true, false},
		{"0", true, false},
		{"none", true, false},
		{"auto", false, true},
		{"AUTO", false, true},
		{"always", false, false},
		{"", false, false},
		{"on", false, false},     // неизвестное значение = прежнее поведение (always)
		{"banana", false, false}, // мусор не должен молча выключать инструмент
	}
	for _, tc := range cases {
		t.Setenv(imageToolEnvInject, tc.value)
		cfg := imageToolSettingsFromEnv()
		if cfg.InjectOff != tc.wantOff {
			t.Errorf("value %q: InjectOff = %v, want %v", tc.value, cfg.InjectOff, tc.wantOff)
		}
		if cfg.InjectAuto != tc.wantAuto {
			t.Errorf("value %q: InjectAuto = %v, want %v", tc.value, cfg.InjectAuto, tc.wantAuto)
		}
		assert.True(t, cfg.Enabled, "режим инъекции не выключает сам инструмент (для %q)", tc.value)
	}
}

// off не должен объявлять инструмент НИКОМУ, даже если клиент прислал tools.
func TestR219_InjectOffNeverInjects(t *testing.T) {
	t.Setenv(imageToolEnvInject, "off")
	cfg := imageToolSettingsFromEnv()
	for _, clientTools := range []int{0, 1, 5} {
		if imageToolShouldInject(cfg, clientTools) {
			t.Errorf("off: инструмент объявлен при clientTools=%d", clientTools)
		}
	}
	// Причина в логе должна отличаться от auto-случая.
	assert.Equal(t, imageToolSkipInjectOff, imageToolNoInjectReason(cfg))
}
