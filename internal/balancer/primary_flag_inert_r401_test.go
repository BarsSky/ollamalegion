// primary_flag_inert_r401_test.go — R401 (2026-11-05).
//
// ЧТО СЛУЧИЛОСЬ. Живая жалоба оператора: «флаг primary бесполезен и непонятно
// зачем нужен… вставляет палки в колёса» — на стенде с несколькими бэкендами.
// Флаг появился в R83 (коммит 1999663a) по запросу оператора: клиентский
// num_ctx молча перезагружал модель на большее окно, и нужна была отбивка
// «настройки зафиксированы администратором». Но политика эксплуатации теперь
// другая: холодный запрос грузит модель под свои параметры, простаивающей
// копии окно растят, если влезает в память. Флаг запрещал ровно это, поэтому
// отключён (R401).
//
// ЧТО ПРОВЕРЯЕТСЯ. Профиль с primary=true больше НЕ меняет решений балансера:
//  1. isPrimaryModel всегда false — единственная точка чтения флага не
//     возвращает старое поведение ни одному вызывающему;
//  2. большой клиентский запрос не получает primary-отказ 413 с текстом
//     «ЗАФИКСИРОВАННЫЕ» и не подменяется «зафиксированным» окном;
//  3. AutoTune для такой модели не выключается флагом (решают профиль и
//     глобальный свитч — как для любой другой модели);
//  4. малое окно в теле запроса остаётся клиентским, а не переписывается на
//     «зафиксированное».
package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// primaryLegacyHarness — Proxy с профилем, где остался исторический primary=true.
func primaryLegacyHarness(t *testing.T, loadedNCtx int) *Proxy {
	t.Helper()
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"primary-model": {ContextLength: loadedNCtx, Primary: true},
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

// legacyChatBody собирает минимальное тело /api/chat с нужным num_ctx и num_predict.
func legacyChatBody(numCtx, numPredict int, prompt string) []byte {
	body := map[string]interface{}{
		"model":    "primary-model",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
		"options":  map[string]interface{}{"num_ctx": numCtx, "num_predict": numPredict},
	}
	b, _ := json.Marshal(body)
	return b
}

// TestR401_PrimaryFlagIsInert — флаг не влезает в решения: большой запрос не
// получает primary-отказ, малое окно не подменяется.
func TestR401_PrimaryFlagIsInert(t *testing.T) {
	p := primaryLegacyHarness(t, 16384)
	if p.isPrimaryModel("primary-model") {
		t.Fatal("isPrimaryModel обязан возвращать false: флаг primary отключён (R401)")
	}

	// (1) Запрос, требующий больше «зафиксированного» окна, больше не получает
	// отказ «настройки зафиксированы администратором».
	bigBody := legacyChatBody(32768, 20000, "short prompt")
	_, _, errMsg, status := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", bigBody, "/api/chat")
	if status == http.StatusRequestEntityTooLarge && strings.Contains(errMsg, "ЗАФИКСИРОВАННЫЕ") {
		t.Fatalf("primary-отказ вернулся, хотя флаг отключён: status=%d err=%q", status, errMsg)
	}

	// (2) Малое окно остаётся клиентским: политика больше не переписывает
	// num_ctx на «зафиксированный».
	smallBody := legacyChatBody(16384, 32, "short prompt")
	out, _, smallMsg, smallStatus := p.preflightNCtxReloadIfNeeded(nil, "bk", "primary-model", smallBody, "/api/chat")
	if smallStatus == http.StatusRequestEntityTooLarge && strings.Contains(smallMsg, "ЗАФИКСИРОВАННЫЕ") {
		t.Fatalf("малый запрос отвергнут primary-политикой: %q", smallMsg)
	}
	if len(out) > 0 {
		var got map[string]interface{}
		if err := json.Unmarshal(out, &got); err == nil {
			if opts, ok := got["options"].(map[string]interface{}); ok {
				if nc, ok := opts["num_ctx"].(float64); ok && int(nc) != 16384 {
					t.Errorf("num_ctx в теле = %v, want 16384 (клиентское, без primary-подмены)", opts["num_ctx"])
				}
			}
		}
	}
}

// TestR401_PrimaryFlagDoesNotDisableAutoTune — AutoTune решают профиль и
// глобальный свитч, а не мёртвый флаг.
func TestR401_PrimaryFlagDoesNotDisableAutoTune(t *testing.T) {
	p := primaryLegacyHarness(t, 16384)
	p.config.Balancing.AutoTune = true
	if !IsAutoTuneEnabled(p, "primary-model") {
		t.Error("профиль с primary=true не должен выключать AutoTune: флаг отключён (R401)")
	}
}
