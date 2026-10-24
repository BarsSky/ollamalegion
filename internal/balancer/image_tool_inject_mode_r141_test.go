package balancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// R141 (2026-10-16): режим объявления image-инструмента.
//
// Живой дефект: балансер объявлял инструмент ВСЕМ клиентам, включая не
// приславших ни одного tools[]. Воркер, увидев непустой tools, вклеивал в
// промпт ~200 токенов преамбулы — замер на стенде: prompt_tokens 19 (напрямую)
// против 220 (через балансер), has_tools false против true, и ответ менялся с
// «Ок.» на reasoning-поток про вызов инструмента. Для клиента это выглядит как
// «нереальный tokens/s».
//
// always — прежнее поведение (дефолт, фича картинок доступна всем);
// auto — только клиентам, которые сами прислали tools[] (такие умеют
// обрабатывать tool_calls).

func TestImageToolShouldInjectModesR141(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cfg         imageToolConfig
		clientTools int
		want        bool
	}{
		{"always, клиент без инструментов — объявляем", imageToolConfig{InjectAuto: false}, 0, true},
		{"always, клиент с инструментами — объявляем", imageToolConfig{InjectAuto: false}, 3, true},
		{"auto, клиент без инструментов — НЕ объявляем", imageToolConfig{InjectAuto: true}, 0, false},
		{"auto, клиент с инструментами — объявляем", imageToolConfig{InjectAuto: true}, 1, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, imageToolShouldInject(tc.cfg, tc.clientTools))
		})
	}
}

// TestImageToolInjectModeFromEnvR141 — разбор LB_IMAGE_TOOL_INJECT.
func TestImageToolInjectModeFromEnvR141(t *testing.T) {
	t.Setenv(imageToolEnvInject, "")
	assert.False(t, imageToolSettingsFromEnv().InjectAuto,
		"дефолт — always: поведение стендов не меняется молча")

	t.Setenv(imageToolEnvInject, "always")
	assert.False(t, imageToolSettingsFromEnv().InjectAuto)

	t.Setenv(imageToolEnvInject, "AUTO")
	assert.True(t, imageToolSettingsFromEnv().InjectAuto, "значение читается регистронезависимо")

	t.Setenv(imageToolEnvInject, "auto")
	cfg := imageToolSettingsFromEnv()
	assert.True(t, cfg.InjectAuto)
	assert.True(t, cfg.Enabled, "режим инъекции не выключает сам инструмент")
}

// TestImageToolSkipReasonMentionsAutoModeR141 — причина отказа объясняет
// оператору, почему инструмента нет, и называет цену объявления.
func TestImageToolSkipReasonMentionsAutoModeR141(t *testing.T) {
	t.Parallel()

	msg := imageToolSkipReason(imageToolSkipNoClientTools)
	assert.Contains(t, msg, "LB_IMAGE_TOOL_INJECT=auto")
	assert.Contains(t, msg, "200", "цена объявления названа числом (преамбула инструментов)")
}
