// nctx_profile_window_r397_test.go — R397 (2026-11-04).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «приходит запрос от клиента на пустой бэкенд — на
// бэкенде поднимается окно 32768, а не 131072, что идёт в запросе».
//
// ЖИВОЙ ПРОГОН (стенд) показал две независимые причины:
//
//  1. ЗАГРУЗКА. Клиент num_ctx=131072 → автозагрузка просит contextPerSeq=65536
//     (окно на слот) → applyProfileLoadParams подставляет ContextSize=32768 из
//     профиля → resolveDeclaredWindow выбирает суммарное (32768) и выбрасывает
//     per-seq → воркер грузит 32768. Профиль работал как ПРЕДЕЛ запроса, хотя
//     по R68 он — подсказка первичной загрузки.
//
//  2. ПОТОЛОК АВТО-РОСТА. Даже после загрузки запрос на 131072 отвергался:
//     потолок = max(LB_NCTX_RELOAD_MAX_N_CTX, contextLength профиля) = 32768, а
//     contextLengthAuto/contextLengthMax (131072) не участвовали нигде — режим
//     «auto» был мёртв по построению.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR397_ProfileDoesNotOverrideExplicitRequestWindow — главный дефект первого
// пункта: явный per-seq из запроса не должен заменяться суммарным окном профиля.
func TestR397_ProfileDoesNotOverrideExplicitRequestWindow(t *testing.T) {
	prof := types.LlamaCppModelProfile{
		ContextLength:     32768, // подсказка профиля (как в живом профиле 27B)
		ContextLengthAuto: true,
		ContextLengthMax:  131072,
	}
	perSeq := 65536
	req := ModelOpRequest{ModelName: "Qwen3.8-27B-UD-Q4_K_M", ContextPerSeq: &perSeq}

	applyProfileLoadParams(&req, prof)

	if req.ContextPerSeq == nil || *req.ContextPerSeq != 65536 {
		t.Fatalf("per-seq из запроса потерян/подменён: %v — именно это давало загрузку 32768 вместо окна клиента",
			req.ContextPerSeq)
	}
	if req.ContextSize != nil {
		t.Fatalf("профиль подставил суммарное окно %d поверх явного per-seq — воркер посчитает окно по профилю, "+
			"а не по запросу", *req.ContextSize)
	}
}

// TestR397_ProfileStillFillsWindowWhenRequestSilent — если клиент окно не
// задавал, профиль остаётся подсказкой первичной загрузки (поведение R68).
func TestR397_ProfileStillFillsWindowWhenRequestSilent(t *testing.T) {
	prof := types.LlamaCppModelProfile{ContextLength: 32768}
	req := ModelOpRequest{ModelName: "m"}

	applyProfileLoadParams(&req, prof)

	if req.ContextSize == nil || *req.ContextSize != 32768 {
		t.Fatalf("без окна в запросе профиль обязан подставить своё: %v", req.ContextSize)
	}
}

// TestR397_BothRequestWindowsStillResolved — R255 сохранён: если запрос задал
// ОБА поля и они противоречат, побеждает суммарное (per-seq не отправляем).
func TestR397_BothRequestWindowsStillResolved(t *testing.T) {
	prof := types.LlamaCppModelProfile{ContextLength: 32768, ContextPerSeq: 65536}
	total := 131072
	perSeq := 65536
	req := ModelOpRequest{ModelName: "m", ContextSize: &total, ContextPerSeq: &perSeq}

	applyProfileLoadParams(&req, prof)

	if req.ContextSize == nil || *req.ContextSize != 131072 {
		t.Fatalf("суммарное окно запроса изменилось: %v", req.ContextSize)
	}
	if req.ContextPerSeq != nil {
		t.Fatalf("при противоречии двух полей per-seq должен быть снят (R255), получено %d", *req.ContextPerSeq)
	}
}

// TestR397_AutoGrowMaxRaisesCeiling — главный дефект второго пункта:
// contextLengthAuto=true + contextLengthMax=131072 обязаны разрешать автоподъём
// до 131072, иначе режим «auto» не работает.
func TestR397_AutoGrowMaxRaisesCeiling(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"Qwen3.8-27B": {
				ContextLength:     32768,
				ContextLengthAuto: true,
				ContextLengthMax:  131072,
			},
		},
	}}

	// env-потолок 32768 (живой .env стенда) больше не связывает.
	if got := p.effectiveAutoReloadCeiling("Qwen3.8-27B", 32768); got != 131072 {
		t.Fatalf("потолок = %d, want 131072: contextLengthAuto+contextLengthMax объявлены оператором, "+
			"но не участвуют — запрос на 131072 отвергался «операторским лимитом»", got)
	}
	// Без auto заявленный максимум не действует: подсказка первичной загрузки
	// не становится политикой авто-роста.
	p.config.LlamaCppModelProfiles["manual"] = types.LlamaCppModelProfile{
		ContextLength:    32768,
		ContextLengthMax: 131072, // auto=false
	}
	if got := p.effectiveAutoReloadCeiling("manual", 32768); got != 32768 {
		t.Fatalf("без contextLengthAuto потолок = %d, want 32768 (env)", got)
	}
	// Заявленный максимум ниже env не понижает потолок.
	p.config.LlamaCppModelProfiles["small"] = types.LlamaCppModelProfile{
		ContextLength: 16384, ContextLengthAuto: true, ContextLengthMax: 32768,
	}
	if got := p.effectiveAutoReloadCeiling("small", 65536); got != 65536 {
		t.Fatalf("потолок = %d, want 65536 (env выше заявленного максимума)", got)
	}
}

// TestR397_ProfileLookupFindsSubstringKey — третья причина живого отказа:
// профиль оператора называется `Qwen3.8-27B`, а модель — `Qwen3.8-27B-UD-Q4_K_M`.
//
// GetModelProfile искал только точный/legacy-ключ и возвращал «профиля нет»,
// поэтому declaredWindowTotal не видел contextLengthMax и потолок авто-роста
// оставался env-значением (32768). Загрузка при этом профиль находила
// (profileForModel — по substring), то есть два читателя одного профиля
// отвечали по-разному.
func TestR397_ProfileLookupFindsSubstringKey(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"Qwen3.8-27B": {
				ContextLength:     32768,
				ContextLengthAuto: true,
				ContextLengthMax:  131072,
			},
		},
	}}

	if _, ok := p.GetModelProfile("Qwen3.8-27B-UD-Q4_K_M"); !ok {
		t.Fatal("профиль Qwen3.8-27B не найден для модели Qwen3.8-27B-UD-Q4_K_M: " +
			"потолок авто-роста и primary/disabled читают именно GetModelProfile")
	}
	if got := p.effectiveAutoReloadCeiling("Qwen3.8-27B-UD-Q4_K_M", 32768); got != 131072 {
		t.Fatalf("потолок = %d, want 131072 (contextLengthMax профиля, найденного по substring)", got)
	}
	// Точный ключ по-прежнему в приоритете: два профиля, один точный.
	p.config.LlamaCppModelProfiles["Qwen3.8-27B-UD-Q4_K_M"] = types.LlamaCppModelProfile{ContextLength: 65536}
	if prof, ok := p.GetModelProfile("Qwen3.8-27B-UD-Q4_K_M"); !ok || prof.ContextLength != 65536 {
		t.Fatalf("точный ключ должен выигрывать у substring: %+v (ok=%v)", prof, ok)
	}
}
