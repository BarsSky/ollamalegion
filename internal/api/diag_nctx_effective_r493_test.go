package api

// diag_nctx_effective_r493_test.go — дефект 1 из 12c: дамп обязан показывать
// ДЕЙСТВУЮЩИЕ значения секции окна, а не только сырой config.json.
//
// Живой факт: в одном summary.json лежали auto_reload_max_n_ctx=131072 и
// preflight_enabled=false, а preflight в том же прогоне работал асинхронно и
// поднял окно до 188416. Оператор читал сырое поле и искал причину не там.

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR493_DiagNCtxEffectiveAppliesEnv(t *testing.T) {
	// Сырой конфиг: как будто в config.json осталось старое (выключено, 131072).
	raw := types.NCtxReloadSettings{
		AutoReloadNCtx:       false,
		PreflightEnabled:     false,
		PreflightAsyncReload: false,
		AutoReloadMaxNCtx:    131072,
	}

	// Без env действующие значения совпадают с сырыми.
	for _, k := range []string{"LB_NCTX_RELOAD_ENABLED", "LB_NCTX_PREFLIGHT_ENABLED",
		"LB_NCTX_PREFLIGHT_ASYNC_RELOAD", "LB_NCTX_RELOAD_MAX_N_CTX"} {
		t.Setenv(k, "") // пустая строка = ParseBool/Atoi ошибка = сырое значение
		os.Unsetenv(k)
	}
	eff := diagNCtxReloadEffectiveR493(raw)
	if eff["preflight_enabled"] != false || eff["auto_reload_max_n_ctx"] != 131072 {
		t.Fatalf("без env действующие значения должны совпадать с сырыми: %+v", eff)
	}

	// С env (как на втором стенде) — действующие другие, и это видно отдельно.
	t.Setenv("LB_NCTX_RELOAD_ENABLED", "true")
	t.Setenv("LB_NCTX_PREFLIGHT_ENABLED", "true")
	t.Setenv("LB_NCTX_PREFLIGHT_ASYNC_RELOAD", "true")
	t.Setenv("LB_NCTX_RELOAD_MAX_N_CTX", "262144")
	eff = diagNCtxReloadEffectiveR493(raw)
	if eff["auto_reload_n_ctx"] != true {
		t.Errorf("auto_reload_n_ctx = %v, ожидалось true (LB_NCTX_RELOAD_ENABLED)", eff["auto_reload_n_ctx"])
	}
	if eff["preflight_enabled"] != true || eff["preflight_async_reload"] != true {
		t.Errorf("preflight не переопределён env: %+v", eff)
	}
	if eff["auto_reload_max_n_ctx"] != 262144 {
		t.Errorf("auto_reload_max_n_ctx = %v, ожидалось 262144", eff["auto_reload_max_n_ctx"])
	}
	if eff["source"] == "" {
		t.Error("нет пометки источника — оператор снова не отличит сырое от действующего")
	}
}

// TestR493_DiagNCtxEffectiveNamesMatchBalancer — страж синхронности имён env.
//
// Разбор env продублирован здесь осознанно (balancer-тип не экспортирован), и
// цена этой копии — расхождение имён при правке одной стороны. Поэтому имена
// сверяются с единственным парсером балансера.
func TestR493_DiagNCtxEffectiveNamesMatchBalancer(t *testing.T) {
	src, err := os.ReadFile("../balancer/nctx_reload_config_bridge.go")
	if err != nil {
		t.Fatalf("не прочитать парсер балансера: %v", err)
	}
	ours, err := os.ReadFile("diag_nctx_effective_r493.go")
	if err != nil {
		t.Fatalf("не прочитать наш разбор: %v", err)
	}
	for _, name := range []string{
		"LB_NCTX_RELOAD_ENABLED",
		"LB_NCTX_PREFLIGHT_ENABLED",
		"LB_NCTX_PREFLIGHT_ASYNC_RELOAD",
		"LB_NCTX_RELOAD_MAX_N_CTX",
	} {
		if !strings.Contains(string(src), name) {
			t.Errorf("в парсере балансера нет %q — наш разбор разошёлся с источником правды", name)
		}
		if !strings.Contains(string(ours), name) {
			t.Errorf("в дампе нет %q — действующие значения перестанут учитывать это переопределение", name)
		}
	}
}

// TestR493_DiagPayloadHasEffectiveBlock — сам дамп обязан содержать ОБА блока и
// пояснение. Без этого «эффективные значения» существуют только в коде.
func TestR493_DiagPayloadHasEffectiveBlock(t *testing.T) {
	s := &Server{config: &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			// Сырой конфиг заведомо старый, env — действующий (как на стенде).
			NCtxReload: types.NCtxReloadSettings{
				AutoReloadNCtx:    false,
				PreflightEnabled:  false,
				AutoReloadMaxNCtx: 131072,
			},
		},
	}}
	t.Setenv("LB_NCTX_PREFLIGHT_ENABLED", "true")
	t.Setenv("LB_NCTX_RELOAD_MAX_N_CTX", "262144")

	payload := s.diagSummaryPayloadR476()
	if _, ok := payload["nctx_reload"]; !ok {
		t.Fatal("в дампе нет сырой секции nctx_reload")
	}
	eff, ok := payload["nctx_reload_effective"].(map[string]interface{})
	if !ok {
		t.Fatal("в дампе нет nctx_reload_effective — оператор снова прочитает сырое поле как действующее")
	}
	if eff["preflight_enabled"] != true || eff["auto_reload_max_n_ctx"] != 262144 {
		t.Errorf("действующие значения в дампе неверны: %+v", eff)
	}
	if note, _ := payload["nctx_reload_note"].(string); note == "" {
		t.Error("нет пояснения, что nctx_reload — сырая секция, а effective учитывает LB_NCTX_*")
	}
}
