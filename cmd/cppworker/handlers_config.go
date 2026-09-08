// handlers_config.go ? CppWorker runtime configuration handlers (get, update, reload).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// CppWorker Config handlers
// ============================================================

var currentConfig *cppbackend.Config

func handleCppWorkerGetConfig(w http.ResponseWriter, r *http.Request) {
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":      currentConfig,
		"nodeName":    os.Getenv("NODE_NAME"),
		"nodeLabels":  os.Getenv("NODE_LABELS"),
		"balancerUrl": os.Getenv("BALANCER_URL"),
		"uptime":      time.Since(uptimeStart).String(),
	})
}

// handleCppWorkerRuntimeConfig ? GET /api/v1/cppworker/config/runtime.
//
// ?????????? ?????? ??????? ??????????? ??????? ? ?? ???????????? ???????????
// (ContextSize, GPULayers, BatchSize, FlashAttnType, NUMA, UseMmap, SizeBytes,
// NLayers, NEmbd, NKvHeads, GGUFContextLength). ??? ??, ??? WebUI ??????????
// ????? ? default-?????????? ?? /api/v1/cppworker/config, ????? ????????
// ????? "default: 8192, runtime: 32768 (gemma-4)" ? ??? ????????? ?????????.
//
// Thread-safe: ?????????? backend.ListModels() ? RLock.
func handleCppWorkerRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	loaded := backend.ListModels()

	// ??????????? ModelInfo ? JSON-????????????? ????? ? ????????? ??????? ?????
	// (snake_case ??? ????????????? ? WebUI/balancer).
	models := make([]map[string]interface{}, 0, len(loaded))
	for _, m := range loaded {
		models = append(models, map[string]interface{}{
			"name":               m.Name,
			"path":               m.Path,
			"state":              m.State,
			"architecture":       m.Architecture,
			"n_layers":           m.NLayers,
			"n_heads":            m.NHeads,
			"n_kv_heads":         m.NKvHeads,
			"n_embd":             m.NEmbd,
			"n_vocab":            m.NVocab,
			"context_size":       m.ContextSize,
			"gguf_context_length": m.GGUFContextLength,
			"size_bytes":         m.SizeBytes,
			"loaded_at":          m.LoadedAt,
			"gpu_count":          m.GPUCount,
			"gpu_layers":         m.GPULayers,
			"tensor_split":       m.TensorSplit,
			"batch_size":         m.BatchSize,
			"flash_attn_type":    m.FlashAttnType,
			"numa":               m.NUMA,
			"use_mmap":           m.UseMmap,
			"active_queries":     m.ActiveQueries,
			"total_queries":      m.TotalQueries,
			"last_used_at":       m.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"loaded_models": models,
		"count":         len(models),
	})
}

// handleCppWorkerUpdateConfig — PUT /api/v1/cppworker/config/update.
//
// Принимает JSON с любым подмножеством полей cppbackend.Config и применяет
// их к currentConfig. Невалидированные / нераспознанные поля молча игнорируются
// (только в лог пишем). Если хотя бы одно поле, влияющее на загрузку модели
// (defaultCtxSize, defaultGpuLayers, defaultKVCacheType, defaultTensorSplit и
// т.п.), было обновлено — инициируется auto-reload всех загруженных моделей
// с новыми defaults (через reloadAllLoadedWithDefaults).
//
// Список принимаемых полей (полное покрытие cppbackend.Config без чувствительных
// HuggingFace-токенов и путей):
//
//	Базовые:
//	  defaultCtxSize (int >= 256)
//	  defaultBatchSize (int >= 1)
//	  defaultGpuLayers (int >= -1)
//	  defaultFlashAttnType (int >= -1)
//	  defaultNuma (bool)
//	  defaultUseMmap (bool)
//	  defaultUseMlock (bool)
//	  defaultNThreads (int >= 0)
//	  defaultNuma, defaultRmsNormEps (float)
//
//	Multi-GPU:
//	  autoGpuDistribution (bool)
//	  tensorSplitStrategy (string: "auto"|"vram-ratio"|"manual")
//	  defaultMainGpu (int >= 0)
//	  defaultRpcBackend (string: "cuda"|"vulkan"|"kompute"|"")
//	  defaultNoMemoryMap (bool)
//	  defaultTensorSplit ([]float, optional; nil = auto)
//	  defaultSplitMode (int >= -1)
//
//	KV-cache:
//	  defaultKvCacheType (string: "f16"|"f32"|"q8_0"|"q4_0"|"")
//	  defaultNoKvOffload (bool)
//
//	RoPE:
//	  defaultRopeFreqBase (float > 0)
//	  defaultRopeFreqScale (float > 0)
//	  defaultRopeScalingType (string: "none"|"linear"|"yarn")
//	  defaultRopeScalingFactor (float > 0)
//
//	YaRN:
//	  defaultYarnExtFactor (float)
//	  defaultYarnAttnFactor (float)
//	  defaultYarnBetaFast (float)
//	  defaultYarnBetaSlow (float)
//
//	Метрики и lifecycle:
//	  enableMetrics (bool)
//	  metricsRetentionSeconds (int >= 0)
//	  idleUnloadMinutes (int >= 0, 0 = off)
//
// Возврат: {status, applied, reload_started, reload_failed, validation_errors}.
// validation_errors содержит список полей, которые были отправлены, но не прошли
// валидацию (чтобы WebUI мог показать пользователю, что именно не принято).
func handleCppWorkerUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "use PUT")
		return
	}
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	// Декодируем в generic map — так WebUI может слать partial update
	// (только изменённые поля), а мы знаем точный список валидных ключей.
	var updates map[string]interface{}
	if err := decodeJSONRequest(r, &updates, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	applied := []string{}
	validationErrors := []string{}

	// === Хелперы для безопасного извлечения + валидации ===
	// Каждый helper пишет в applied[] при успехе и в validationErrors[] при ошибке.
	// Возвращают (value, ok). ok=false означает, что поле либо отсутствует,
	// либо неверного типа, либо не прошло валидацию.
	//
	// Session 17 P.6 (2026-07-27): JSON `null` для поля трактуется как "skip"
	// (same as key not present), а не "validation error". Раньше WebUI слал
	// `defaultTensorSplit: null` для пустого поля → "expected array of numbers".
	// Аналогично для defaultRopeFreqBase=0 (форма не заполнена, но default 0).
	// Теперь null/undefined-equivalent → "поле не обновлять".
	getInt := func(key string, min, max int) (int, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return 0, false
		}
		f, ok := v.(float64)
		if !ok {
			validationErrors = append(validationErrors, key+": expected number")
			return 0, false
		}
		n := int(f)
		if n < min {
			validationErrors = append(validationErrors,
				fmt.Sprintf("%s: must be >= %d, got %d", key, min, n))
			return 0, false
		}
		if max > 0 && n > max {
			validationErrors = append(validationErrors,
				fmt.Sprintf("%s: must be <= %d, got %d", key, max, n))
			return 0, false
		}
		return n, true
	}
	getFloat := func(key string, min float64) (float64, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return 0, false
		}
		f, ok := v.(float64)
		if !ok {
			validationErrors = append(validationErrors, key+": expected number")
			return 0, false
		}
		// Session 17 P.6: 0 трактуется как "use llama.cpp default" (skip)
		// для опциональных float-полей где 0 = sentinel default. Это легитимная
		// llama.cpp конвенция (0 в n_ctx, n_threads, rope_freq_base и т.д.
		// = "let llama.cpp decide"). Раньше форма с пустым полем шлёт 0
		// → "must be >= 1, got 0" → пользователь не мог сохранить.
		// Теперь: 0 = skip, валидные значения > min проходят нормально.
		if f == 0 {
			return 0, false
		}
		if f < min {
			validationErrors = append(validationErrors,
				fmt.Sprintf("%s: must be >= %g, got %g", key, min, f))
			return 0, false
		}
		return f, true
	}
	getBool := func(key string) (bool, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return false, false
		}
		b, ok := v.(bool)
		if !ok {
			validationErrors = append(validationErrors, key+": expected boolean")
			return false, false
		}
		return b, true
	}
	getString := func(key string, allowed []string) (string, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return "", false
		}
		s, ok := v.(string)
		if !ok {
			validationErrors = append(validationErrors, key+": expected string")
			return "", false
		}
		if len(allowed) > 0 {
			ok := false
			for _, a := range allowed {
				if s == a {
					ok = true
					break
				}
			}
			if !ok {
				validationErrors = append(validationErrors,
					fmt.Sprintf("%s: must be one of %v, got %q", key, allowed, s))
				return "", false
			}
		}
		return s, true
	}
	getFloatSlice := func(key string) ([]float32, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return nil, false
		}
		arr, ok := v.([]interface{})
		if !ok {
			validationErrors = append(validationErrors, key+": expected array of numbers")
			return nil, false
		}
		out := make([]float32, 0, len(arr))
		for i, item := range arr {
			f, ok := item.(float64)
			if !ok {
				validationErrors = append(validationErrors,
					fmt.Sprintf("%s[%d]: expected number", key, i))
				return nil, false
			}
			out = append(out, float32(f))
		}
		return out, true
	}
	// applyInt — если поле валидно, мутирует *target и пишет в applied[].
	applyInt := func(key string, target *int, min, max int) {
		if n, ok := getInt(key, min, max); ok {
			*target = n
			applied = append(applied, key)
		}
	}
	applyFloat := func(key string, target *float64, min float64) {
		if f, ok := getFloat(key, min); ok {
			*target = f
			applied = append(applied, key)
		}
	}
	applyBool := func(key string, target *bool) {
		if b, ok := getBool(key); ok {
			*target = b
			applied = append(applied, key)
		}
	}
	applyString := func(key string, target *string, allowed []string) {
		if s, ok := getString(key, allowed); ok {
			*target = s
			applied = append(applied, key)
		}
	}
	applyFloatSlice := func(key string, target *[]float32) {
		if arr, ok := getFloatSlice(key); ok {
			*target = arr
			applied = append(applied, key)
		}
	}

	// === Базовые параметры загрузки ===
	// defaultCtxSize: минимум 256 (lower bound C-bridge), максимум 262144 (gemma-4 256K).
	applyInt("defaultCtxSize", &currentConfig.DefaultCtxSize, 256, 262144)
	applyInt("defaultBatchSize", &currentConfig.DefaultBatchSize, 1, 4096)
	// defaultGpuLayers: -2 = auto/all (adaptive reload path), -1 = all, 0 = CPU only, >0 = N слоёв.
	// Без верхней границы (теоретически может быть 999 для какой-нибудь MoE).
	// Session 17 P.3 (2026-07-27): -2 был неявно отклоняем валидатором ("must be >= -1"),
	// хотя internal/cppbackend/backend.go:678 явно обрабатывает -2 как auto/all.
	// Фикс: min=-2.
	applyInt("defaultGpuLayers", &currentConfig.DefaultGPULayers, -2, 0)
	// defaultFlashAttnType: -1=auto, 0=disabled, 1=enabled (см. llama.cpp).
	// На 2026-07 cppworker использует только {0, 1, -1}, но оставляем запас.
	applyInt("defaultFlashAttnType", &currentConfig.DefaultFlashAttnType, -1, 2)
	applyBool("defaultNuma", &currentConfig.DefaultNUMA)
	applyBool("defaultUseMmap", &currentConfig.DefaultUseMmap)
	applyBool("defaultUseMlock", &currentConfig.DefaultUseMlock)
	applyInt("defaultNThreads", &currentConfig.DefaultNThreads, 0, 4096)
	// Round 12 (2026-07-28): defaultNParallel — n_parallel для batched generation.
	// 0 = bridge default (=1), 1..8 = multi-slot. Потолок 8 — больше никто
	// не использует, llama.cpp рекомендует ≤ batch_size/threads.
	// При изменении — load-affecting (нужен reload моделей).
	applyInt("defaultNParallel", &currentConfig.DefaultNParallel, 0, 8)
	applyFloat("defaultRmsNormEps", &currentConfig.DefaultRMSNormEps, 0)

	// === Multi-GPU / distribution ===
	applyBool("autoGpuDistribution", &currentConfig.AutoGPUDistribution)
	applyString("tensorSplitStrategy", &currentConfig.TensorSplitStrategy,
		[]string{"", "auto", "vram-ratio", "manual", "round-robin"})
	applyInt("defaultMainGpu", &currentConfig.DefaultMainGPU, 0, 0)
	applyString("defaultRpcBackend", &currentConfig.DefaultRPCBackend,
		[]string{"", "cuda", "vulkan", "kompute"})
	applyBool("defaultNoMemoryMap", &currentConfig.DefaultNoMemoryMap)
	applyFloatSlice("defaultTensorSplit", &currentConfig.DefaultTensorSplit)
	applyInt("defaultSplitMode", &currentConfig.DefaultSplitMode, -1, 3)

	// === KV cache ===
	// "" = inherit default (f16). "f16"/"f32" — full precision, "q8_0"/"q4_0" — quant.
	applyString("defaultKvCacheType", &currentConfig.DefaultKVCacheType,
		[]string{"", "f16", "f32", "q8_0", "q4_0"})
	applyBool("defaultNoKvOffload", &currentConfig.DefaultNoKVOffload)

	// === RoPE ===
	applyFloat("defaultRopeFreqBase", &currentConfig.DefaultRopeFreqBase, 1)
	applyFloat("defaultRopeFreqScale", &currentConfig.DefaultRopeFreqScale, 0)
	// ropeScalingType: "none"/"linear"/"yarn" + "" = none.
	applyString("defaultRopeScalingType", &currentConfig.DefaultRopeScalingType,
		[]string{"", "none", "linear", "yarn"})
	applyFloat("defaultRopeScalingFactor", &currentConfig.DefaultRopeScalingFactor, 0)

	// === YaRN ===
	// Все 4 float. Без нижней границы — llama.cpp сам валидирует семантику.
	applyFloat("defaultYarnExtFactor", &currentConfig.DefaultYarnExtFactor, 0)
	applyFloat("defaultYarnAttnFactor", &currentConfig.DefaultYarnAttnFactor, 0)
	applyFloat("defaultYarnBetaFast", &currentConfig.DefaultYarnBetaFast, 0)
	applyFloat("defaultYarnBetaSlow", &currentConfig.DefaultYarnBetaSlow, 0)

	// === Метрики и lifecycle ===
	applyBool("enableMetrics", &currentConfig.EnableMetrics)
	applyInt("metricsRetentionSeconds", &currentConfig.MetricsRetentionS, 0, 0)
	// idleUnloadMinutes: 0 = off. > 0 = выгружать после N минут простоя.
	// Без верхней границы (для long-running тестов можно поставить 99999).
	applyInt("idleUnloadMinutes", &currentConfig.IdleUnloadMinutes, 0, 0)

	// Session 18 (2026-07-28): Reasoning/Thinking — для моделей gemma-4,
	// deepseek-r1, qwen3-thinking и др. Включает thinking-режим в chat
	// template (если GGUF содержит tokenizer.chat_template с поддержкой).
	// Парсер think-блоков уже работает в reasoning_content.go.
	applyBool("enableReasoning", &currentConfig.EnableReasoning)
	applyInt("reasoningBudget", &currentConfig.ReasoningBudget, 0, 100000)

	// === Логируем нераспознанные ключи (помогает WebUI отлаживать) ===
	knownKeys := map[string]bool{
		// Базовые
		"defaultCtxSize": true, "defaultBatchSize": true,
		"defaultGpuLayers": true, "defaultFlashAttnType": true,
		"defaultNuma": true, "defaultUseMmap": true, "defaultUseMlock": true,
		"defaultNThreads": true, "defaultNParallel": true, "defaultRmsNormEps": true,
		// Multi-GPU
		"autoGpuDistribution": true, "tensorSplitStrategy": true,
		"defaultMainGpu": true, "defaultRpcBackend": true,
		"defaultNoMemoryMap": true, "defaultTensorSplit": true, "defaultSplitMode": true,
		// KV cache
		"defaultKvCacheType": true, "defaultNoKvOffload": true,
		// RoPE
		"defaultRopeFreqBase": true, "defaultRopeFreqScale": true,
		"defaultRopeScalingType": true, "defaultRopeScalingFactor": true,
		// YaRN
		"defaultYarnExtFactor": true, "defaultYarnAttnFactor": true,
		"defaultYarnBetaFast": true, "defaultYarnBetaSlow": true,
		// Метрики и lifecycle
		"enableMetrics": true, "metricsRetentionSeconds": true, "idleUnloadMinutes": true,
		// Session 18 (Round 11/14): Reasoning/Thinking — runtime fields, не load-affecting.
		"enableReasoning": true, "reasoningBudget": true,
	}
	for k := range updates {
		if !knownKeys[k] {
			logger.Get().Warnw("handleCppWorkerUpdateConfig: unknown field",
				"field", k, "value", updates[k])
		}
	}

	// Сохраняем в config/cppworker-defaults.json — единый источник истины.
	// Запись в .env удалена: .env не монтируется в bundled compose, и
	// дублирование значений между JSON и .env приводит к рассинхрону.
	if err := saveConfigToDefaultsFile(currentConfig); err != nil {
		logger.Get().Warnw("failed to save config to cppworker-defaults.json", "error", err)
	} else {
		logger.Get().Infow("config saved to cppworker-defaults.json",
			"applied", len(applied),
			"validationErrors", len(validationErrors),
			"defaultCtxSize", currentConfig.DefaultCtxSize,
			"defaultGpuLayers", currentConfig.DefaultGPULayers,
			"defaultKvCacheType", currentConfig.DefaultKVCacheType,
			"defaultUseMmap", currentConfig.DefaultUseMmap)
	}

	// Шаг 1: auto-reload загруженных моделей с новыми defaults, если изменены
	// параметры нагрузки. При изменении только метрик/lifecycle — reload не нужен.
	reloadStarted := []string{}
	reloadFailed := []string{}
	if backend != nil && hasReloadedDefaults(applied) {
		reloadStarted, reloadFailed = reloadAllLoadedWithDefaults()
		// Сбрасываем RAM fallback attempts во избежание зацикливания.
		resetAllReloadAttempts()
	}

	resp := map[string]interface{}{
		"status":         "updated",
		"applied":        applied,
		"reload_started": reloadStarted,
		"reload_failed":  reloadFailed,
	}
	if len(validationErrors) > 0 {
		resp["validation_errors"] = validationErrors
	}
	writeJSON(w, http.StatusOK, resp)
}

// hasReloadedDefaults ? true, ???? ????? ??????????? ????? ???? ?????????,
// ??????? ?????? ?? ???????? ? ??????? reload ??????.
func hasReloadedDefaults(applied []string) bool {
	// Load-affecting fields. Если хотя бы одно из них изменилось —
	// загруженные модели нужно перезагрузить с новыми defaults.
	// Метрики / lifecycle / debug-поля (enableMetrics, idleUnloadMinutes и т.п.)
	// НЕ требуют reload — они читаются из b.cfg без перезагрузки модели.
	loadAffecting := map[string]bool{
		// Базовые
		"defaultCtxSize":       true,
		"defaultBatchSize":     true,
		"defaultGpuLayers":     true,
		"defaultFlashAttnType": true,
		"defaultNuma":          true,
		"defaultUseMmap":       true,
		"defaultUseMlock":      true,
		"defaultNThreads":      true,
		"defaultNParallel":     true, // Round 12: load-affecting (n_seq_max)
		"defaultRmsNormEps":    true,
		// Multi-GPU
		"autoGpuDistribution": true,
		"tensorSplitStrategy": true,
		"defaultMainGpu":      true,
		"defaultNoMemoryMap":  true,
		"defaultTensorSplit":  true,
		"defaultSplitMode":    true,
		// KV cache
		"defaultKvCacheType": true,
		"defaultNoKvOffload": true,
		// RoPE/YaRN (меняются на лету, но reload безопаснее и явнее)
		"defaultRopeFreqBase":     true,
		"defaultRopeFreqScale":    true,
		"defaultRopeScalingType":  true,
		"defaultRopeScalingFactor": true,
		"defaultYarnExtFactor":    true,
		"defaultYarnAttnFactor":   true,
		"defaultYarnBetaFast":     true,
		"defaultYarnBetaSlow":     true,
	}
	for _, k := range applied {
		if loadAffecting[k] {
			return true
		}
	}
	return false
}

// reloadAllLoadedWithDefaults ? ????????????? ?????? ??????????? ??????
// ? ?????? ?????????? ??????????? (DefaultCtxSize, DefaultBatchSize ? ?.?.).
// ????????? reload ? ????????? ??????????? (?????? ?????? ??????????)
// ? ?????????? ????? ??????? ?????????? ? ???????.
//
// ?????????? backend.LoadModelWithOpts ? reload ????? Unload + LoadModelWithOpts.
// ???? ?????? ??? ????????? ? ???? ?? ??????????? ? reload ????????????
// (backend.LoadModelWithOpts ?????? "already_loaded" ??? ??????????? ??????).
func reloadAllLoadedWithDefaults() ([]string, []string) {
	if backend == nil || currentConfig == nil {
		return nil, []string{"backend or config not initialized"}
	}
	loaded := backend.ListModels()
	started := make([]string, 0, len(loaded))
	failed := make([]string, 0)
	for _, m := range loaded {
		if m.State != cppbackend.StateLoaded {
			continue
		}
		opts := cppbackend.LoadModelOpts{
			GPULayers:     currentConfig.DefaultGPULayers,
			ContextSize:   currentConfig.DefaultCtxSize,
			BatchSize:     currentConfig.DefaultBatchSize,
			FlashAttnType: currentConfig.DefaultFlashAttnType,
			NUMA:          currentConfig.DefaultNUMA,
			UseMmap:       currentConfig.DefaultUseMmap,
			TensorSplit:   m.TensorSplit,
			NThreads:      currentConfig.DefaultNThreads,
			KVCacheType:   currentConfig.DefaultKVCacheType,
			SplitMode:     currentConfig.DefaultSplitMode,
			// Round 12: Parallel (n_parallel) — load-affecting, reload подхватит.
			Parallel: currentConfig.DefaultNParallel,
		}
		// Если в currentConfig задан defaultTensorSplit и AutoGPUDistribution
		// отключён — используем явный split из конфига (приоритет над runtime m.TensorSplit).
		// Если AutoGPUDistribution=true — оставляем m.TensorSplit (auto-распределение в
		// LoadModelWithOpts само рассчитает split по VRAM).
		if !currentConfig.AutoGPUDistribution && len(currentConfig.DefaultTensorSplit) > 0 {
			opts.TensorSplit = currentConfig.DefaultTensorSplit
		}
		// Use adaptive SelectStrategy if available (tries f16→q8_0→q4_0)
		if *autoOffload {
			if globalEnv != nil {
				env := globalEnv.Get()
				ggufMeta := cppbackend.GGUFModelMeta{
					Architecture: m.Architecture,
					NLayers:      m.NLayers,
					NEmbd:        m.NEmbd,
					NHeads:       m.NHeads,
					NKvHeads:     m.NKvHeads,
					SizeBytes:    int64(m.SizeBytes),
				}
				strategy := SelectStrategy(&env, "", ggufMeta, m.ContextSize, -2, currentConfig)
				opts.GPULayers = strategy.GPULayers
				opts.KVCacheType = strategy.KVCacheType
				opts.UseMmap = strategy.UseMmap
			} else {
				opts.GPULayers = calculateOptimalGPULayersForModel(m, "")
			}
		}
		// ????????? reload ? ???????? ? ?? ????????? ????? WebUI ?? ??????.
		go func(name string, modelPath string, loadOpts cppbackend.LoadModelOpts) {
			_, cancel := contextWithTimeout(120 * time.Second)
			defer cancel()
			if err := backend.UnloadModel(name); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: unload failed",
					"model", name, "error", err)
				return
			}
			if err := backend.LoadModelWithOpts(name, modelPath, loadOpts); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: reload with new defaults failed",
					"model", name, "error", err)
			} else {
				logger.Get().Infow("handleCppWorkerUpdateConfig: reloaded with new defaults",
					"model", name,
					"context_size", loadOpts.ContextSize,
					"gpu_layers", loadOpts.GPULayers)
			}
		}(m.Name, m.Path, opts)
		started = append(started, m.Name)
	}
	return started, failed
}

// resetAllReloadAttempts ? ?????????? ??????? RAM fallback attempts ??? ???? ???????.
// ?????????? ????? apply config ? ?????? ????? reload-loop ?????? ???????????.
func resetAllReloadAttempts() {
	// ramFallbackAttempts ? sync.Map ? per-model ??????????.
	// ????? ?????? ??????????? map ????? ???????: ???????????? ??? ?????.
	ramFallbackAttempts.Range(func(key, value interface{}) bool {
		ramFallbackAttempts.Delete(key)
		return true
	})
}

func handleCppWorkerReloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if _, err := os.Stat(envPath); err != nil {
		writeError(w, http.StatusNotFound, "env file not found: "+envPath)
		return
	}
	if err := cppbackend.LoadDotEnvFile(envPath); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	newCfg := cppbackend.LoadConfigFromEnv()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			newCfg.Port = *port
		case "models-dir":
			newCfg.ModelsDir = *modelsDir
		case "ctx-size":
			newCfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			newCfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			newCfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			newCfg.DefaultFlashAttnType = *flashAttn
		case "numa":
			newCfg.DefaultNUMA = *numa
		case "no-mmap":
			newCfg.DefaultUseMmap = !*noMmap
		}
	})
	// RAM fallback flags ?? ??????????? ? cppbackend.Config, ?? ??? reload
	// ?? .env ?? ???????????? ??, ????? runtime-?????? ?????????????? env.
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_N_CTX"); envVal != "" {
		*ramFallbackNCtx = parseBoolEnv(envVal)
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_GPU_LAYERS"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= -1 {
			*ramFallbackGpuLayers = v
		}
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_MAX_N_CTX"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= 512 {
			*ramFallbackMaxNCtx = v
		}
	}
	*currentConfig = newCfg
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reloaded", "config": currentConfig})
}

// saveConfigToDefaultsFile ????????? ??????? ???????????? ?
// config/cppworker-defaults.json ? ?????? ???????? ?????? ???
// ????????? ?????????? CppWorker.
func saveConfigToDefaultsFile(cfg *cppbackend.Config) error {
	defaultsPath := "config/cppworker-defaults.json"
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsPath, data, 0644)
}

