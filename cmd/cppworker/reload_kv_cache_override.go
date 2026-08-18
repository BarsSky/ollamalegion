package main

import (
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// applyStrategyKVCacheOverride merges SelectStrategy.KVCacheType into opts while
// respecting user-explicit values (from reload body or profile).
//
// Round 40 #2 (2026-08-18): Previously the equivalent code at
// handlers_model.go:1304 was an UNCONDITIONAL override
//
//	opts.KVCacheType = strategy.KVCacheType
//
// which silently discarded user-sent kvCacheType (e.g. q4_0) from the
// /api/models/reload body. Symptom: user POSTed kvCacheType="q4_0" with
// force=true, the model reloaded, /api/model still reported f16. The C-bridge
// did log "KV cache type: Q4_0 (-75% VRAM)" but the state-reporting path
// saw a different (post-override) value, so the user observed that the
// setting "didn't stick".
//
// New contract:
//
//   - opts.KVCacheType == ""  AND strategy.KVCacheType != "" → strategy wins
//     (caller did not request a specific KV cache type).
//   - opts.KVCacheType != ""  AND strategy.KVCacheType == "" → user wins
//     (strategy had no opinion — happens for fallback_no_meta stage with
//     auto_kv disabled).
//   - opts.KVCacheType != ""  AND strategy.KVCacheType != "" → user wins,
//     and we log an INFO entry so operators can see the divergence
//     (this is the load-OK-but-different-from-strategy case; useful when
//     diagnosing future "VRAM too tight" failures).
//
// The bridge/backend still receives opts.KVCacheType verbatim:
// internal/cppbackend/backend.go:712-716 falls back to
// b.cfg.DefaultKVCacheType when opts.KVCacheType == "", so leaving
// the field empty when neither user nor strategy set it is safe.
//
// Returns true if the post-call opts.KVCacheType came from the user (or
// profile — by the time we get here, the profile has already been applied
// to opts.KVCacheType at handlers_model.go:1254), false if it came from
// strategy. Used by tests to assert the precedence invariant.
func applyStrategyKVCacheOverride(opts *cppbackend.LoadModelOpts, strategy LoadStrategyResult, modelName string) (fromUser bool) {
	switch {
	case opts.KVCacheType == "" && strategy.KVCacheType != "":
		// Neither user nor profile set kvCacheType; let strategy pick.
		opts.KVCacheType = strategy.KVCacheType
		return false

	case opts.KVCacheType != "" && strategy.KVCacheType == "":
		// User/province set a type, strategy had no opinion.
		return true

	case opts.KVCacheType != "" && strategy.KVCacheType != "" && opts.KVCacheType != strategy.KVCacheType:
		// User/province set one type, strategy picked another. Honor user.
		logger.Get().Infow("reload: SelectStrategy kvCacheType differs from user choice, honoring user",
			"name", modelName,
			"user_kv_cache_type", opts.KVCacheType,
			"strategy_kv_cache_type", strategy.KVCacheType,
			"strategy_stage", strategy.Stage)
		return true

	default:
		// Both empty, or both equal — user wins by default.
		return opts.KVCacheType != ""
	}
}
