// Package tests — CONTRACT SPEC for Round 40 #2.
//
// ⚠️  ВАЖНО: реальный тест applyStrategyKVCacheOverride живёт в
//
//	cmd/cppworker/reload_kv_cache_override_test.go (//go:build llama_stub)
//
// Этот файл — DELETED-CONTRACT-MIRROR, оставлен как документация
// контракта merge-логики. cmd/... — package main, его нельзя
// импортировать в tests/, поэтому зеркало нужно для напоминания
// 4-веточного контракта. Реальный test run живёт в cmd/.
package tests

// 4-веточный контракт applyStrategyKVCacheOverride:
//
//	┌──────────────────────┬──────────────────────┬────────────────────┐
//	│ opts.KVCacheType     │ strategy.KVCacheType │ result.KVCacheType  │
//	├──────────────────────┼──────────────────────┼────────────────────┤
//	│ ""                   │ "f16"                │ "f16" (strategy)   │
//	│ "q4_0"               │ "f16"                │ "q4_0" (USER WINS) │
//	│ "q8_0"               │ "q4_0"               │ "q8_0" (USER WINS) │
//	│ "q4_0"               │ ""                   │ "q4_0" (no opinion)│
//	│ "q8_0"               │ "q8_0"               │ "q8_0" (agree)     │
//	│ ""                   │ ""                   │ "" (no source)     │
//	└──────────────────────┴──────────────────────┴────────────────────┘
//
// Регрессия: до Round 40 #2 в handlers_model.go:1304 стояла
// безусловная перезапись `opts.KVCacheType = strategy.KVCacheType`.
// Это превращало строку 2 в "f16" (strategy) — теряя user q4_0.
