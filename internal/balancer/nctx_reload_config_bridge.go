// Package balancer — мост между internal/config (yaml/json конфиг балансировщика)
// и balancer.NCtxReloadConfig (структура, которую использует nctx_reload.go).
//
// Создан отдельный файл чтобы:
//  1. pkg/types не зависел от internal/balancer (иначе циклический import);
//  2. при отсутствии BalancingSettings.NCtxReload в конфиге использовался
//     безопасный default (AutoReloadNCtx=false), не падать на старых конфигах.
//
// Stage 6: прокидывание nctx_reload секции через internal/config + ENV fallback.
package balancer

import (
	"os"
	"strconv"

	"ollama-loadbalancer/pkg/types"
)

// loadNCtxReloadConfig — извлекает NCtxReloadConfig из полного конфига балансировщика.
// Если в config.json есть секция balancing.nctxReload — использует её,
// иначе — безопасные defaults (фича выключена).
//
// Также применяет ENV fallback (LB_NCTX_RELOAD_*), см. applyNCtxReloadEnvOverrides.
// Приоритет: ENV > config.json > defaults (что соответствует общему паттерну
// OLLAMALEGION_* / LB_* ENV overrides в internal/config/config.go).
func loadNCtxReloadConfig(cfg *types.LoadBalancerConfig) NCtxReloadConfig {
	def := DefaultNCtxReloadConfig()
	if cfg == nil {
		return applyNCtxReloadEnvOverrides(def)
	}
	src := NCtxReloadConfig{
		AutoReloadNCtx:             cfg.Balancing.NCtxReload.AutoReloadNCtx,
		AutoReloadMaxNCtx:          cfg.Balancing.NCtxReload.AutoReloadMaxNCtx,
		AutoReloadVRAMSafetyFactor: cfg.Balancing.NCtxReload.AutoReloadVRAMSafetyFactor,
		AutoReloadTimeoutSec:       cfg.Balancing.NCtxReload.AutoReloadTimeoutSec,
		// Round 31 #2 (2026-08-09): копируем preflight поля (раньше они терялись
		// при partial config — bool zero value = false, что выключало preflight).
		PreflightEnabled:            cfg.Balancing.NCtxReload.PreflightEnabled,
		PreflightAsyncReload:        cfg.Balancing.NCtxReload.PreflightAsyncReload,
		PreflightAsyncRetryAfterSec: cfg.Balancing.NCtxReload.PreflightAsyncRetryAfterSec,
	}
	// Заполняем дефолты для незаданных полей
	hasAnyField := src.AutoReloadNCtx || cfg.Balancing.NCtxReload.AutoReloadMaxNCtx > 0 ||
		cfg.Balancing.NCtxReload.AutoReloadVRAMSafetyFactor > 0 ||
		cfg.Balancing.NCtxReload.AutoReloadTimeoutSec > 0 ||
		cfg.Balancing.NCtxReload.PreflightEnabled ||
		cfg.Balancing.NCtxReload.PreflightAsyncReload
	if !hasAnyField {
		// Совсем пустая секция — используем defaults
		src = def
	} else {
		// Частично заполненная — мерджим с defaults для нулевых полей
		if src.AutoReloadVRAMSafetyFactor == 0 {
			src.AutoReloadVRAMSafetyFactor = def.AutoReloadVRAMSafetyFactor
		}
		if src.AutoReloadTimeoutSec == 0 {
			src.AutoReloadTimeoutSec = def.AutoReloadTimeoutSec
		}
		// Round 31 #2: preflight должен быть включён по умолчанию если есть
		// хоть какие-то поля в nctxReload секции (но в config.json не задан явно).
		// Без этого: preflight_enabled = false zero value → preflight отключён.
		if !src.PreflightEnabled {
			src.PreflightEnabled = def.PreflightEnabled
		}
		if src.PreflightAsyncRetryAfterSec == 0 {
			src.PreflightAsyncRetryAfterSec = def.PreflightAsyncRetryAfterSec
		}
	}
	return applyNCtxReloadEnvOverrides(src)
}

// applyNCtxReloadEnvOverrides — применяет ENV fallback (LB_NCTX_RELOAD_*).
// Поддерживаемые переменные:
//   - LB_NCTX_RELOAD_ENABLED (true/false) → AutoReloadNCtx
//   - LB_NCTX_RELOAD_MAX_N_CTX (int) → AutoReloadMaxNCtx
//   - LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR (float) → AutoReloadVRAMSafetyFactor
//   - LB_NCTX_RELOAD_TIMEOUT_SEC (int) → AutoReloadTimeoutSec
//   - LB_NCTX_PREFLIGHT_ENABLED (true/false) → PreflightEnabled (Round 34 Phase 0 fix)
//   - LB_NCTX_PREFLIGHT_ASYNC_RELOAD (true/false) → PreflightAsyncReload (Round 31 #2)
//   - LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC (int) → PreflightAsyncRetryAfterSec
//
// Приоритет: ENV > config.json. Если ENV не задан — оставляем значение из config.
//
// Round 34 follow-up: добавил LB_NCTX_PREFLIGHT_ENABLED override. Раньше
// preflight был выключен в config.json (нет поля preflight_enabled) →
// bool zero value = false → preflight helper сразу возвращался
// (runInferencePreflight проверяет PreflightEnabled ДО PreflightAsyncReload).
// С `LB_NCTX_PREFLIGHT_ENABLED=true` в compose + этот override,
// preflight действительно работает (Round 34 Phase 0 фикс изначально
// не запускался — Cline 65K → 413 sync вместо 503+Retry-After).
func applyNCtxReloadEnvOverrides(cfg NCtxReloadConfig) NCtxReloadConfig {
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AutoReloadNCtx = b
		}
	}
	// Round 34 follow-up: allow overriding PreflightEnabled via env.
	if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.PreflightEnabled = b
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_MAX_N_CTX"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AutoReloadMaxNCtx = n
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.AutoReloadVRAMSafetyFactor = f
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_TIMEOUT_SEC"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AutoReloadTimeoutSec = n
		}
	}
	// Round 31 #2 (2026-08-09): async reload mode для preflight.
	if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_ASYNC_RELOAD"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.PreflightAsyncReload = b
		}
	}
	if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PreflightAsyncRetryAfterSec = n
		}
	}
	return cfg
}
