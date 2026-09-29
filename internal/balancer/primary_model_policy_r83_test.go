// primary_model_policy_r83_test.go — R83 (2026-09-29).
//
// ПРИМАРИ-РЕЖИМ: администратор фиксирует параметры модели, клиентский запрос их НЕ
// меняет.
//
// ЖАЛОБА ОПЕРАТОРА: «настройки не должны сбиваться запросами от клиента; если
// настройки клиента не проходят по значениям для работы с моделью — пусть будет
// отбивка, что настройки в этом бэкенде зафиксированы, править через администратора».
//
// Что проверяется:
//  1. запрос, которому ХВАТАЕТ зафиксированного контекста, обслуживается (и num_ctx
//     в теле подменяется на загруженный, чтобы cppworker не увидел чужой num_ctx);
//  2. запросу, которому нужно БОЛЬШЕ, возвращается 413 с текстом про зафиксированные
//     настройки и «правку через администратора» — вместо тихой перезагрузки;
//  3. без флага primary поведение прежнее (запрос идёт обычным путём);
//  4. для primary-модели AutoTune отключён.
package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// primaryHarness — Proxy с одним загруженным контекстом и профилем модели.
func primaryHarness(t *testing.T, loadedNCtx int, primary bool) *Proxy {
	t.Helper()
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"primary-model": {ContextLength: loadedNCtx, Primary: primary},
			},
		},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: NewModelManager(nil),
	}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.nctxReload.SetLastKnownNCtx("bk", loadedNCtx)
	return p
}

// chatBody собирает минимальное тело /api/chat с нужным num_ctx и num_predict.
func chatBody(numCtx, numPredict int, prompt string) []byte {
	body := map[string]interface{}{
		"model":    "primary-model",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
		"options":  map[string]interface{}{"num_ctx": numCtx, "num_predict": numPredict},
	}
	b, _ := json.Marshal(body)
	return b
}

// TestR83_Primary_SmallRequestIsServed — хватает контекста → обслуживаем, настройки
// не трогаем, num_ctx в теле подменяется на зафиксированный.
func TestR83_Primary_SmallRequestIsServed(t *testing.T) {
	p := primaryHarness(t, 16384, true)
	body := chatBody(16384, 32, "short prompt")

	out, needsProxy, errMsg, status := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", body, "/api/chat")
	if !needsProxy || errMsg != "" || status != http.StatusOK {
		t.Fatalf("маленький запрос должен обслуживаться: needsProxy=%v err=%q status=%d",
			needsProxy, errMsg, status)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("тело после политики не JSON: %v", err)
	}
	opts, _ := got["options"].(map[string]interface{})
	if nc, _ := opts["num_ctx"].(float64); int(nc) != 16384 {
		t.Errorf("num_ctx в теле = %v, want 16384 (зафиксированный, а не клиентский)",
			opts["num_ctx"])
	}
}

// TestR83_Primary_BigRequestIsRejectedWithAdminMessage — не хватает контекста →
// 413 с объяснением, что настройки зафиксированы и правятся через администратора.
func TestR83_Primary_BigRequestIsRejectedWithAdminMessage(t *testing.T) {
	p := primaryHarness(t, 16384, true)
	// Клиент просит 32768 и большую генерацию — зафиксированных 16384 не хватит.
	body := chatBody(32768, 20000, "short prompt")

	_, needsProxy, errMsg, status := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", body, "/api/chat")
	if needsProxy {
		t.Fatal("запрос не должен уходить на cppworker: настройки зафиксированы, " +
			"а контекста не хватает — ожидался отказ")
	}
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", status)
	}
	if errMsg == "" {
		t.Fatal("пустое сообщение об отказе — клиент не поймёт причину")
	}
	// Текст должен объяснять ДВЕ вещи: настройки зафиксированы и что делать.
	for _, want := range []string{"ЗАФИКСИРОВАННЫЕ", "n_ctx=16384", "администратора"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("в отказе нет %q; текст: %s", want, errMsg)
		}
	}
}

// TestR83_Primary_DisabledKeepsPreviousBehavior — без флага primary запрос,
// требующий большего контекста, идёт обычным путём (балансер вправе перезагрузить).
func TestR83_Primary_DisabledKeepsPreviousBehavior(t *testing.T) {
	p := primaryHarness(t, 16384, false)
	body := chatBody(32768, 512, "short prompt")

	_, needsProxy, errMsg, status := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", body, "/api/chat")
	// Политика primary не должна вмешиваться: отказ с текстом про администратора
	// здесь неуместен (модель не помечена как зафиксированная).
	if status == http.StatusRequestEntityTooLarge && strings.Contains(errMsg, "ЗАФИКСИРОВАННЫЕ") {
		t.Fatalf("без primary-флага отказ про фиксацию не должен возвращаться: %s", errMsg)
	}
	_ = needsProxy // дальнейшее поведение — забота preflight, здесь важно отсутствие primary-отказа
}

// TestR83_Primary_DisablesAutoTune — primary-модель исключена из авто-оптимизации.
func TestR83_Primary_DisablesAutoTune(t *testing.T) {
	p := primaryHarness(t, 16384, true)
	if IsAutoTuneEnabled(p, "primary-model") {
		t.Error("для primary-модели AutoTune обязан быть выключен: иначе авто-оптимизация " +
			"перезагрузит модель и сдвинет зафиксированные администратором параметры")
	}

	// Контроль: та же модель без флага — AutoTune работает по глобальной настройке.
	p2 := primaryHarness(t, 16384, false)
	p2.config.Balancing.AutoTune = true
	if !IsAutoTuneEnabled(p2, "primary-model") {
		t.Error("без primary-флага AutoTune должен работать (глобальный свитч включён)")
	}
}

// TestR83_Primary_UnknownContextDoesNotBlock — если зафиксированный контекст
// неизвестен (0), политика не вмешивается: лучше пропустить запрос обычным путём,
// чем отказать наугад.
func TestR83_Primary_UnknownContextDoesNotBlock(t *testing.T) {
	p := primaryHarness(t, 0, true)
	body := chatBody(32768, 512, "prompt")

	_, needsProxy, errMsg, status := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", body, "/api/chat")
	if status == http.StatusRequestEntityTooLarge {
		t.Errorf("при неизвестном зафиксированном контексте отказ недопустим: %s", errMsg)
	}
	_ = needsProxy
}
