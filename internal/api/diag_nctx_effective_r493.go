package api

// diag_nctx_effective_r493.go — дефект 1 из разбора 12c: диагностический дамп
// показывал сырую секцию `nctx_reload` рядом с действующим потолком и не
// объяснял, что это РАЗНЫЕ вещи.
//
// ЖИВОЙ ФАКТ (архив второго стенда, 2026-11-16). В одном summary.json:
//   nctx_reload.auto_reload_max_n_ctx = 131072, preflight_enabled = false,
//   preflight_async_reload = false        ← сырая секция config.json,
//   nctx_ceiling.envCeiling           = 262144,
// а preflight в том же прогоне работал АСИНХРОННО и поднял окно до 188416
// (target_n_ctx=188416 в логе). Три поля противоречили поведению рядом.
//
// ПРИЧИНА. `LB_NCTX_*` переопределяют секцию на старте
// (balancer.applyNCtxReloadEnvOverrides — единственный парсер, менять его не
// надо). Дамп же печатал конфиг ДО этих переопределений.
//
// РЕШЕНИЕ. Рядом с сырой секцией печатаем действующие значения, собранные по тем
// же env-именам и тому же правилу (strconv.ParseBool/Atoi, ошибка = исходное
// значение). Повтор разбора здесь осознан: balancer-тип не экспортирован в
// types, а тащить его в api ради трёх полей — худшая связность. Синхронность
// имён стережёт тест diag_nctx_effective_r493_test.go.

import (
	"os"
	"strconv"

	"ollama-loadbalancer/pkg/types"
)

// diagNCtxReloadEffectiveR493 — действующие значения секции nctxReload.
func diagNCtxReloadEffectiveR493(cfg types.NCtxReloadSettings) map[string]interface{} {
	enabled := cfg.AutoReloadNCtx
	preflight := cfg.PreflightEnabled
	async := cfg.PreflightAsyncReload
	maxN := cfg.AutoReloadMaxNCtx

	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			preflight = b
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_ASYNC_RELOAD"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			async = b
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_MAX_N_CTX"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			maxN = n
		}
	}

	return map[string]interface{}{
		"auto_reload_n_ctx":      enabled,
		"preflight_enabled":      preflight,
		"preflight_async_reload": async,
		"auto_reload_max_n_ctx":  maxN,
		"source":                 "config.json + LB_NCTX_* (те же переопределения, что применяются к работе)",
	}
}
