//go:build llama_stub

// window_reserve_r502_test.go — R502 (2026-11-19), аудит S4, пункт N-9.
//
// ДЕФЕКТ. «Влезает ли промпт» решали два разных числа: балансер считал запас
// ответа 256 токенов, воркер требует жёсткие 512 (иначе HTTP 413). Для остатка
// 300…511 балансер решал «обслужить» — и клиент получал отказ при живом запасе
// окна.
//
// Тест держит два свойства:
//  1. оба конца берут ОДНО значение (общая константа, а не два литерала);
//  2. граница из разрыва (256 < room < 512) больше не считается «влезает».
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR502_MinAnswerTokensIsSharedConstant — N-9: балансер и воркер считают по
// одной константе, а не по двум литералам.
func TestR502_MinAnswerTokensIsSharedConstant(t *testing.T) {
	if agentOutputReserveMinTokens != types.ContextBudgetMinAnswerTokensR502 {
		t.Fatalf("балансерный минимум бюджета ответа = %d, общая константа = %d: "+
			"два числа снова разошлись (аудит N-9)",
			agentOutputReserveMinTokens, types.ContextBudgetMinAnswerTokensR502)
	}
	// Значение обязано быть не меньше жёсткого пола воркера: воркер ниже 512 не
	// опускает n_predict, поэтому балансер не имеет права обещать меньше.
	if types.ContextBudgetMinAnswerTokensR502 < 512 {
		t.Fatalf("общий минимум = %d, ожидалось ≥512 (жёсткий пол воркера)",
			types.ContextBudgetMinAnswerTokensR502)
	}
}

// TestR502_GapNoLongerLooksLikeItFits — N-9: остаток из старого разрыва
// (256…511) обязан требовать роста, а не «обслуживать как есть».
func TestR502_GapNoLongerLooksLikeItFits(t *testing.T) {
	const loaded = 4096
	for _, room := range []int{400, 511} {
		estimated := loaded - room
		need, why := growthNeedR447(estimated, loaded, 0)
		if !need {
			t.Fatalf("остаток под ответ %d токенов признан достаточным (%q): "+
				"воркер на таком остатке отвечает 413 (аудит N-9)", room, why)
		}
		if !strings.Contains(why, "512") {
			t.Fatalf("причина роста не называет общий минимум: %q", why)
		}
	}

	// Ровно граница: остаток 512 — достаточно, 511 — уже нет.
	if need, why := growthNeedR447(loaded-512, loaded, 0); need {
		t.Fatalf("остаток 512 токенов признан недостаточным (%q), хотя это ровно пол", why)
	}
	if need, _ := growthNeedR447(loaded-511, loaded, 0); !need {
		t.Fatal("остаток 511 токенов признан достаточным, хотя пол — 512")
	}
}
