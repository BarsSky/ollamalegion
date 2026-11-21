// window_growth_policy_r664_test.go — R664 (шаг A, 2026-11-20): рост окна по
// заявке клиента не блокирует его запрос.
//
// ЖИВОЙ ФАКТ (двухузловой стенд): заявка 16384 при загруженных 8192 и промпте в
// 15 токенов давала перезагрузку модели и 59.7 с ожидания (для 27B — минуты),
// хотя данные влезали в текущее окно с запасом.
//
// ШАГ A: триггер роста прежний (ЗАЯВКА КЛИЕНТА), меняется только цена — если
// данные влезают, обслуживаем сразу, окно поднимаем в фоне до заявленного
// значения. Ветки «данные не влезают» и «под ответ меньше минимума» остаются
// ростом, как в старом контракте.
package balancer

import (
	"strings"
	"testing"
)

// TestR664_GrowthActionRule — таблица действий шага A.
func TestR664_GrowthActionRule(t *testing.T) {
	cases := []struct {
		name              string
		estimated, loaded int
		requested         int
		want              growthActionR664
	}{
		{"сжатие: данные меньше окна, окна не просили", 500, 8192, 0, growthNoneR664},
		{"сжатие: клиент заявил МЕНЬШЕ загруженного", 500, 8192, 2048, growthNoneR664},
		{"данные влезают, клиент заявил больше — рост в фоне", 15, 8192, 16384, growthBackgroundR664},
		{"данные не влезают — рост (как было)", 13023, 8192, 0, growthRequiredR664},
		{"данные не влезают, окно заявлено — рост", 13023, 8192, 16384, growthRequiredR664},
		{"под ответ меньше минимума — рост (как было)", 2000, 2048, 0, growthRequiredR664},
		{"окно неизвестно — не спекулируем", 13023, 0, 16384, growthNoneR664},
		{"оценки нет, окна не просили", 0, 8192, 0, growthNoneR664},
		{"оценки нет, клиент заявил больше — в фоне", 0, 8192, 16384, growthBackgroundR664},
	}
	for _, tc := range cases {
		got, why := growthActionForR664(tc.estimated, tc.loaded, tc.requested)
		if got != tc.want {
			t.Fatalf("%s: action = %s, want %s (причина: %s)", tc.name, got.String(), tc.want.String(), why)
		}
		if strings.TrimSpace(why) == "" {
			t.Fatalf("%s: причина не названа — оператор не поймёт решения", tc.name)
		}
	}
}

// TestR664_GrowthNeedOnlyForImmediate — growthNeedR447 отвечает «нужен ли
// НЕМЕДЛЕННЫЙ рост»: заявка при влезающих данных — фон, а не немедленный рост.
func TestR664_GrowthNeedOnlyForImmediate(t *testing.T) {
	if need, _ := growthNeedR447(15, 8192, 16384); need {
		t.Fatal("заявка клиента при влезающих данных — фоновый рост, не немедленный")
	}
	if need, why := growthNeedR447(13023, 8192, 0); !need {
		t.Fatalf("данные не влезают — рост нужен, как и было (причина: %s)", why)
	}
	if need, why := growthNeedR447(13023, 8192, 16384); !need {
		t.Fatalf("данные не влезают и окно заявлено — рост обязателен (причина: %s)", why)
	}
}

// TestR664_BackgroundGrowthTargetIsClientWindow — цель роста — ровно заявленное
// клиентом окно (не оценка промпта, не профиль).
func TestR664_BackgroundGrowthTargetIsClientWindow(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 40000, RequestedNCtxOverride: 65536}
	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("ожидался PreflightReload, получено %v", res.Decision)
	}
	if res.TargetNCtx != 65536 {
		t.Fatalf("TargetNCtx = %d, ожидалось ровно заявленное клиентом 65536", res.TargetNCtx)
	}
}
