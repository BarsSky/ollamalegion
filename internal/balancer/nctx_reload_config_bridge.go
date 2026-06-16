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
	}
	// Заполняем дефолты для незаданных полей
	if !src.AutoReloadNCtx && cfg.Balancing.NCtxReload.AutoReloadMaxNCtx == 0 &&
		cfg.Balancing.NCtxReload.AutoReloadVRAMSafetyFactor == 0 &&
		cfg.Balancing.NCtxReload.AutoReloadTimeoutSec == 0 {
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
	}
	return applyNCtxReloadEnvOverrides(src)
}

// applyNCtxReloadEnvOverrides — применяет ENV fallback (LB_NCTX_RELOAD_*).
// Поддерживаемые переменные:
//   - LB_NCTX_RELOAD_ENABLED (true/false) → AutoReloadNCtx
//   - LB_NCTX_RELOAD_MAX_N_CTX (int) → AutoReloadMaxNCtx
//   - LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR (float) → AutoReloadVRAMSafetyFactor
//   - LB_NCTX_RELOAD_TIMEOUT_SEC (int) → AutoReloadTimeoutSec
//
// Приоритет: ENV > config.json. Если ENV не задан — оставляем значение из config.
func applyNCtxReloadEnvOverrides(cfg NCtxReloadConfig) NCtxReloadConfig {
	if v, ok := os.LookupEnv("LB_NCTX_RELOAD_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AutoReloadNCtx = b
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
	return cfg
}
