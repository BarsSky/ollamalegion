// handlers_config.go ? CppWorker runtime configuration handlers (get, update, reload).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
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
	resp := map[string]interface{}{
		"config":      currentConfig,
		"nodeName":    os.Getenv("NODE_NAME"),
		"nodeLabels":  os.Getenv("NODE_LABELS"),
		"balancerUrl": os.Getenv("BALANCER_URL"),
		"uptime":      time.Since(uptimeStart).String(),
	}
	// R123 (2026-10-16, пункт 2): состояние узла раскладки — в КАЖДОМ чтении
	// конфига, а не только в ответе на его изменение.
	//
	// ЗАЧЕМ. Балансер и WebUI опрашивают именно GET /config; если статус
	// дочернего процесса отдавать лишь в ответе на PUT, оператор видел бы узел
	// «включённым» ровно в момент переключения, а после перезагрузки страницы —
	// ничего. Здесь же видно и факт (running/listening), и причину отказа.
	if rpcServerChild != nil {
		resp["rpcServer"] = rpcServerChild.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCppWorkerRuntimeConfig ? GET /api/v1/cppworker/config/runtime.
//
// Возвращает массив реально загруженных моделей с их фактическими параметрами
// (ContextSize, GPULayers, BatchSize, FlashAttnType, NUMA, UseMmap, SizeBytes,
// NLayers, NEmbd, NKvHeads, GGUFContextLength). Это то, что WebUI показывает
// рядом с default-значениями из /api/v1/cppworker/config, чтобы оператор
// видел "default: 8192, runtime: 32768 (gemma-4)" и мог применить изменения.
//
// Thread-safe: использует backend.ListModels() с RLock.
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

	// Преобразуем ModelInfo в JSON-дружественную форму с понятными именами полей
	// (snake_case для совместимости с WebUI/balancer).
	models := make([]map[string]interface{}, 0, len(loaded))
	for _, m := range loaded {
		models = append(models, map[string]interface{}{
			"name":                m.Name,
			"path":                m.Path,
			"state":               m.State,
			"architecture":        m.Architecture,
			"n_layers":            m.NLayers,
			"n_heads":             m.NHeads,
			"n_kv_heads":          m.NKvHeads,
			"n_embd":              m.NEmbd,
			"n_vocab":             m.NVocab,
			"context_size":        m.ContextSize,
			"gguf_context_length": m.GGUFContextLength,
			"size_bytes":          m.SizeBytes,
			"loaded_at":           m.LoadedAt,
			"gpu_count":           m.GPUCount,
			"gpu_layers":          m.GPULayers,
			// R248: фактический offload и признак деградации — наружу, а не
			// только в лог. Иначе оператор видит «loaded» и не понимает, почему
			// модель медленная (живой случай: 20/66 слоёв на GPU, остальное в RAM).
			"offload_degraded": m.OffloadDegraded,
			"offload_note":     m.OffloadNote,
			"tensor_split":     m.TensorSplit,
			"batch_size":       m.BatchSize,
			"flash_attn_type":  m.FlashAttnType,
			"numa":             m.NUMA,
			"use_mmap":         m.UseMmap,
			"active_queries":   m.ActiveQueries,
			"total_queries":    m.TotalQueries,
			"last_used_at":     m.LastUsedAt,
			// R83/v67 (2026-10-02): флаги, с которыми модель РЕАЛЬНО работает.
			//
			// Жалоба оператора: «не выводится информация о всех флагах в строке
			// состояния Загруженные у модели на странице GGUF Models». Здесь не
			// хватало kv-cache, параллельности, окна на слот и reasoning — то есть
			// ровно тех настроек, которые оператор выставляет в WebUI и хочет
			// видеть применёнными (в первую очередь галочку «размышления»).
			"kv_cache_type":     m.KVCacheType,
			"parallel":          m.Parallel,
			"context_per_seq":   m.ContextPerSeq,
			"reasoning_enabled": m.ReasoningEnabled,
			"batched_parallel":  m.BatchedParallel,
			"flash_attn":        m.FlashAttnType,
			// R118.16 (2026-10-14): ТОЧНАЯ раскладка слоёв по устройствам из
			// лога llama.cpp (`load_tensors: layer N assigned to device X`).
			//
			// Имя ключа с суффиксом _exact — потому что балансер декодирует
			// этот ответ в ту же структуру, которой отвечает WebUI, а там
			// `device_layers` уже занят ОБЪЕКТОМ-планом (см.
			// internal/balancer/llamacpp_runtime_config.go). Разные имена не
			// дают двум полям перекрыться в одном JSON.
			"device_layers_exact": func() interface{} {
				if len(m.DeviceLayers) == 0 {
					return nil
				}
				return m.DeviceLayers
			}(),
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
	// R115: список узлов раскладки приходит массивом строк (["host:port", …]).
	// Пустой массив — законное значение («узлов нет»), поэтому отличается от
	// «поле не прислали»: nil-указатель и пустой срез здесь разные вещи.
	getStringSlice := func(key string) ([]string, bool) {
		v, ok := updates[key]
		if !ok || v == nil {
			return nil, false
		}
		arr, ok := v.([]interface{})
		if !ok {
			validationErrors = append(validationErrors, key+": expected array of strings")
			return nil, false
		}
		out := make([]string, 0, len(arr))
		for i, item := range arr {
			s, ok := item.(string)
			if !ok {
				validationErrors = append(validationErrors,
					fmt.Sprintf("%s[%d]: expected string", key, i))
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
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

	// === Раскладка слоёв между машинами (RPC) — R115 ===
	//
	// ЗАЧЕМ ЗДЕСЬ. До R115 раскладка настраивалась ТОЛЬКО переменными окружения
	// при старте: ни WebUI, ни API не могли её ни увидеть, ни изменить. Оператор
	// правил .env и перезапускал контейнер — то есть «интерфейса настройки RPC»
	// не существовало.
	//
	// ПРАВИЛО ТО ЖЕ, ЧТО У ФЛАГА ПРИ СТАРТЕ (см. resolveRPCMode): явный
	// rpcEnabled=false — решающий и очищает список узлов, чтобы «выключено» нельзя
	// было перепутать с «список есть, но не используется». Непустой список без
	// явного false включает раскладку (совместимость с прежним поведением).
	//
	// Список узлов разбирается ТЕМ ЖЕ парсером, что и переменная окружения
	// (parseRPCServersR101): второй реализации правил «host:port» быть не должно.
	if raw, ok := getStringSlice("rpcServers"); ok {
		if len(raw) == 0 {
			currentConfig.RPCServers = nil
			applied = append(applied, "rpcServers")
		} else if parsed, err := parseRPCServersR101(strings.Join(raw, ",")); err != nil {
			validationErrors = append(validationErrors, "rpcServers: "+err.Error())
		} else {
			currentConfig.RPCServers = parsed
			applied = append(applied, "rpcServers")
		}
	}
	explicitRPCFalse := false
	if v, ok := updates["rpcEnabled"]; ok && v != nil {
		if b, isBool := v.(bool); isBool {
			currentConfig.RPCEnabled = b
			applied = append(applied, "rpcEnabled")
			explicitRPCFalse = !b
		} else {
			validationErrors = append(validationErrors, "rpcEnabled: expected boolean")
		}
	}
	if explicitRPCFalse && len(currentConfig.RPCServers) > 0 {
		currentConfig.RPCServers = nil
		applied = append(applied, "rpcServers(cleared)")
	}
	if !explicitRPCFalse && len(currentConfig.RPCServers) > 0 {
		currentConfig.RPCEnabled = true
	}
	// Раскладка применяется при СЛЕДУЮЩЕЙ загрузке: перезагружать модель молча
	// нельзя (это минуты простоя), но и молчать об этом нельзя — ответ несёт
	// rpc_reload_required, а WebUI говорит оператору «применится при следующей
	// загрузке».
	rpcChanged := false
	for _, k := range applied {
		if k == "rpcEnabled" || k == "rpcServers" || k == "rpcServers(cleared)" {
			rpcChanged = true
			break
		}
	}
	if rpcChanged && logger.Get() != nil {
		logger.Get().Infow(i18n.T("cppworker.handlers_config.rpc_mode_changed"),
			"rpc_enabled", currentConfig.RPCEnabled,
			"servers", currentConfig.RPCServers,
			"applied", applied)
	}
	if rpcChanged && backend != nil {
		// R115: донести режим до ЖИВОГО конфига бэкенда.
		//
		// `currentConfig` (пакет cppworker) и `b.cfg` (бэкенд) — РАЗНЫЕ объекты.
		// Живая проверка 2026-10-13: ответ был
		// `{"applied":["rpcEnabled","rpcServers(cleared)"]}`, а /api/models
		// продолжал отдавать rpc_enabled=true, узел и 7009 MB удалённой памяти —
		// то есть настройка сохранялась без эффекта, и WebUI показывал бы успех,
		// ничего не меняя.
		backend.SetRPCConfig(currentConfig.RPCEnabled, currentConfig.RPCServers)
	}
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

	// R123 (2026-10-16, пункт 2): узел раскладки (донор) — llama.cpp rpc-server
	// дочерним процессом. Включается здесь (и из WebUI), состояние процесса
	// возвращается в ответе как rpcServer.
	applyBool("rpcServerEnabled", &currentConfig.RPCServerEnabled)
	applyInt("rpcServerPort", &currentConfig.RPCServerPort, 0, 65535)

	// === РОЛЬ БЭКЕНДА — R304 (2026-10-30) ===
	//
	// ЗАЧЕМ ОТДЕЛЬНЫЙ КЛЮЧ. До R304 роль не доезжала до воркера: приказ нёс только
	// раскладку-потребитель (rpcEnabled/rpcServers) и свой узел (rpcServerEnabled),
	// а «эта машина — донор» приходилось выводить догадкой. Догадка ломается там,
	// где узел включают руками, — и донор одновременно отдаёт память мастеру и
	// грузит своё.
	//
	// ЗНАЧЕНИЕ НОРМАЛИЗУЕТСЯ ДО ВАЛИДАЦИИ: приказ может прийти как "Donor" или с
	// пробелом, и отказ «не одно из допустимых» на ровном месте означал бы для
	// оператора «настройка не применилась» — тот самый класс жалоб.
	//
	// ОТКУДА РОЛЬ НЕ ПРИХОДИТ: из окружения. Единственный источник — балансер
	// (см. комментарий у cppbackend.Config.BackendRole): env позволил бы включить
	// донора на машине, где им быть не должны.
	if raw, ok := updates["backendRole"]; ok && raw != nil {
		if s, isStr := raw.(string); isStr {
			if norm, known := cppbackend.NormalizeBackendRole(s); known {
				updates["backendRole"] = norm
			}
		}
	}
	applyString("backendRole", &currentConfig.BackendRole, cppbackend.BackendRoles())

	// Session 18 (2026-07-28): Reasoning/Thinking — для моделей gemma-4,
	// deepseek-r1, qwen3-thinking и др. Включает thinking-режим в chat
	// template (если GGUF содержит tokenizer.chat_template с поддержкой).
	// Парсер think-блоков уже работает в reasoning_content.go.
	applyBool("enableReasoning", &currentConfig.EnableReasoning)
	applyInt("reasoningBudget", &currentConfig.ReasoningBudget, 0, 100000)

	// R106b (2026-10-12): «размышления» для НОВОЙ загрузки.
	//
	// ЗАЧЕМ ОТДЕЛЬНЫЕ КЛЮЧИ. У reasoning в cppbackend.Config два поля с разной
	// ролью: EnableReasoning — runtime-политика (включает thinking-инструкцию в
	// промпте, действует сразу), DefaultEnableReasoning — значение, которое
	// НАСЛЕДУЕТ модель при загрузке, если клиент/балансер его не задал
	// (backend.go:1344: enableReasoning := b.cfg.DefaultEnableReasoning).
	//
	// ЖИВОЙ ДЕФЕКТ (жалоба оператора «поменял размышления с выключенного на
	// включенный, а модель грузится не с теми параметрами»): WebUI писал только
	// `enableReasoning`, а эти два ключа обработчик НЕ принимал вовсе — поле
	// загрузки оставалось false, и инстанс поднимался с reasoning_enabled=false.
	// Теперь WebUI пишет оба поля сразу (см. gguf-renderer-detail-render.js:
	// saveBackendOptions → defaultEnableReasoning).
	//
	// Reload загруженных моделей эти ключи НЕ запускают (в loadAffecting их нет):
	// смена политики размышлений не обязана перезагружать модель, а «применить
	// сейчас» делает явный apply профиля (POST .../model-profiles/{name}/apply).
	applyBool("defaultEnableReasoning", &currentConfig.DefaultEnableReasoning)
	applyInt("defaultReasoningBudget", &currentConfig.DefaultReasoningBudget, 0, 100000)

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
		// R304: роль бэкенда (донор не поднимает своих копий моделей).
		"backendRole": true,
		// Session 18 (Round 11/14): Reasoning/Thinking.
		// enableReasoning/reasoningBudget — runtime-политика (промпт);
		// defaultEnableReasoning/defaultReasoningBudget — то, что наследует
		// НОВАЯ загрузка (R106b, 2026-10-12). Оба набора — не load-affecting.
		"enableReasoning": true, "reasoningBudget": true,
		"defaultEnableReasoning": true, "defaultReasoningBudget": true,
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

	// Шаг 0 (R119 2026-10-15): настройки, которые обязаны действовать НА ХОДУ.
	//
	// idleUnloadMinutes раньше менял только currentConfig: таймаут был «зашит» в
	// IdleUnloadManager при его создании, поэтому UI показывал новые минуты, а
	// выгрузка шла по старому значению (живой замер: PUT 3 → конфиг 3, менеджер
	// 10). Теперь значение применяется к живому менеджеру и сохраняется в файл
	// операторских переопределений, который читается ПОСЛЕ env — то есть правка
	// из WebUI переживает пересоздание контейнера и побеждает
	// CPPWORKER_IDLE_UNLOAD_MINUTES.
	idleUnloadEffective := -1
	if appliedListContains(applied, "idleUnloadMinutes") {
		recordRuntimeOverride("idleUnloadMinutes", currentConfig.IdleUnloadMinutes)
		if backend != nil {
			effective := backend.SetIdleUnloadTimeout(
				time.Duration(currentConfig.IdleUnloadMinutes) * time.Minute)
			idleUnloadEffective = int(effective / time.Minute)
			logger.Get().Infow(i18n.T("cppworker.handlers_config.idle_unload_applied"),
				"requested_minutes", currentConfig.IdleUnloadMinutes,
				"effective_minutes", idleUnloadEffective)
		}
	}

	// R123 (2026-10-16, пункт 2): узел раскладки — применяем НА ХОДУ и сохраняем
	// как операторское переопределение (приоритет над env, переживает пересоздание
	// контейнера).
	//
	// R173 (2026-10-18): ПРИКАЗ НЕ ГАСИТ ЖИВОЙ УЗЕЛ. Живая авария, доказанная на
	// доноре: балансер присылает rpcServerEnabled/rpcServerPort на каждом
	// согласовании роли, а этот блок реагировал на ФАКТ ПОЛЯ в запросе, а не на
	// изменение значения — и убивал дочерний rpc-server прямо во время передачи
	// весов мастером. Мастер терял RPC-сессию и умирал (SIGPIPE/abort), контейнер
	// перезапускался; оператор видел «встроенный узел падает», хотя узел был
	// исправен, а его гасили. Отдельный контейнер узла не страдал ровно потому,
	// что его процесс никто не гасил (в режиме external Stop() только снимает
	// желание — R171).
	//
	// Теперь: настройки не изменились — процесса не касаемся вообще (SetEnabled
	// идемпотентен: уже запущенный узел он не перезапускает); сменилась только
	// «включённость» — SetEnabled; сменилась ИДЕНТИЧНОСТЬ (адрес/порт/бинарь/
	// внешний адрес/отладка) — только тогда пересоздание, потому что порт зашит в
	// argv, а GGML_RPC_DEBUG — в env процесса.
	rpcServerNote := ""
	if appliedListContains(applied, "rpcServerEnabled") || appliedListContains(applied, "rpcServerPort") {
		recordRuntimeOverride("rpcServerEnabled", currentConfig.RPCServerEnabled)
		recordRuntimeOverride("rpcServerPort", currentConfig.RPCServerPort)
		// R304 (2026-10-30): ФАКТ «свой узел включён» обязан доехать и до живого
		// конфига бэкенда.
		//
		// ЗАЧЕМ. По правилу cppbackend.DonorOnly поднятый узел — признак донора
		// (машина отдаёт память мастеру). Пока состояние узла жило только в
		// currentConfig и в супервизоре, решение о донорстве принималось бы по
		// `b.cfg`, где узел выключен, — то есть приказ балансера включал бы узел,
		// а загрузка своих копий на этой машине продолжала бы разрешаться до
		// пересоздания контейнера. Это ровно дефект «настройка применена, эффекта
		// нет», уже разобранный на rpcEnabled (см. SetRPCConfig).
		if backend != nil {
			backend.SetRPCServerEnabled(currentConfig.RPCServerEnabled)
		}
		recreated := false
		if rpcServerChild == nil {
			rpcServerChild = cppbackend.NewRPCServerChild(*currentConfig)
			recreated = true
		} else if rpcServerChild.RequiresRecreate(*currentConfig) {
			// Единственный случай, когда сессия раскладки рвётся осознанно —
			// поэтому и предупреждение, а не тихая пересборка процесса.
			prev := rpcServerChild.Status()
			logger.Get().Warnw(i18n.T("cppworker.handlers_config.rpc_identity_changed"),
				"was_bind", prev.Bind, "was_port", prev.Port, "was_bin", prev.Bin,
				"was_external", prev.External,
				"bind", currentConfig.RPCServerBind, "port", currentConfig.RPCServerPort,
				"bin", currentConfig.RPCServerBin)
			_ = rpcServerChild.Stop()
			rpcServerChild = cppbackend.NewRPCServerChild(*currentConfig)
			recreated = true
		}
		if err := rpcServerChild.SetEnabled(currentConfig.RPCServerEnabled); err != nil {
			// Ошибка запуска НЕ отменяет желаемое состояние: оператор увидит
			// enabled=true, running=false и причину — как в статусе супервизора.
			rpcServerNote = err.Error()
			logger.Get().Errorw(i18n.T("cppworker.handlers_config.rpc_start_failed"),
				"enabled", currentConfig.RPCServerEnabled,
				"port", currentConfig.RPCServerPort, "error", err)
		} else if recreated {
			logger.Get().Infow(i18n.T("cppworker.handlers_config.rpc_recreated"),
				"enabled", currentConfig.RPCServerEnabled,
				"port", currentConfig.RPCServerPort, "addr", rpcServerChild.Addr())
		} else {
			// Явно фиксируем «ничего не менялось»: иначе следующий разбор снова
			// будет искать, кто убил узел.
			logger.Get().Debugw(i18n.T("cppworker.handlers_config.rpc_confirmed_noop"),
				"enabled", currentConfig.RPCServerEnabled,
				"port", currentConfig.RPCServerPort, "addr", rpcServerChild.Addr())
		}
	}

	// R133 (2026-10-16): САМА раскладка (кто донор) — тоже решение, которое
	// обязано переживать пересоздание контейнера.
	//
	// ЖИВОЙ ДЕФЕКТ. В deployments/.env лежат CPPWORKER_RPC_ENABLE=true и
	// CPPWORKER_RPC_SERVERS=192.0.2.10:50052, поэтому при КАЖДОМ recreate
	// мастер снова включал раскладку на выключенного донора — хотя балансер
	// (единственный, кто теперь назначает доноров ролями) оставлял раскладку
	// выключенной. Оператор видел «настройка не сохраняется» и не понимал,
	// какой параметр обязателен. Теперь последнее применённое состояние лежит в
	// файле операторских переопределений и побеждает env — тот же контракт, что
	// у idleUnloadMinutes и rpcServerEnabled.
	if appliedListContains(applied, "rpcEnabled") ||
		appliedListContains(applied, "rpcServers") ||
		appliedListContains(applied, "rpcServers(cleared)") {
		recordRuntimeOverride("rpcEnabled", currentConfig.RPCEnabled)
		recordRuntimeOverride("rpcServers", currentConfig.RPCServers)
	}

	// R304 (2026-10-30): РОЛЬ БЭКЕНДА — действует НА ХОДУ и переживает
	// пересоздание контейнера.
	//
	// ПОЧЕМУ ПЕРЕОПРЕДЕЛЕНИЕ, А НЕ ТОЛЬКО ПАМЯТЬ ПРОЦЕССА. Роль объявляет балансер
	// при каждом согласовании состояния, но контейнер пересоздаётся независимо от
	// него — и без файла операторских переопределений роль после recreate
	// вернулась бы к «неизвестно». Донор снова начал бы грузить свои копии, хотя
	// мастер уже спланировал раскладку по его памяти. Тот же контракт, что у
	// idleUnloadMinutes, rpcServerEnabled и rpcEnabled.
	//
	// ПОЧЕМУ ДОНОСИТЬ ДО БЭКЕНДА ОТДЕЛЬНО. `currentConfig` и `b.cfg` — РАЗНЫЕ
	// объекты: без SetBackendRole строка «роль = donor» осталась бы в ответе API и
	// в файле, а решения о загрузке бэкенд принимал бы по старому состоянию.
	if appliedListContains(applied, "backendRole") {
		recordRuntimeOverride("backendRole", currentConfig.BackendRole)
		donor, reason := false, ""
		if backend != nil {
			backend.SetBackendRole(currentConfig.BackendRole)
			// Читаем ФАКТ после переноса: в логе должно быть решение воркера
			// («донор ли машина и почему»), а не отправленное значение — иначе
			// при role=main с поднятым узлом лог утверждал бы «не донор».
			donor, reason = backend.DonorOnly()
		}
		logger.Get().Infow(i18n.T("cppworker.donor.role_applied"),
			"role", currentConfig.BackendRole,
			"donor", donor,
			"reason", reason)
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
	// R119: показываем ФАКТ, а не отправленное значение — иначе UI снова будет
	// утверждать «сохранено», пока менеджер живёт по старому таймауту.
	if idleUnloadEffective >= 0 {
		resp["idleUnloadMinutesEffective"] = idleUnloadEffective
	}
	if backend != nil {
		if d, running := backend.IdleUnloadStatus(); d > 0 {
			resp["idleUnloadMinutesEffective"] = int(d / time.Minute)
			resp["idleUnloadRunning"] = running
		} else {
			resp["idleUnloadMinutesEffective"] = 0
			resp["idleUnloadRunning"] = false
		}
	}
	if len(validationErrors) > 0 {
		resp["validation_errors"] = validationErrors
	}
	// R123: состояние узла раскладки — ФАКТ (running/listening/pid/причина отказа),
	// а не отправленное значение: WebUI обязан показывать, поднялся ли процесс.
	if rpcServerChild != nil {
		resp["rpcServer"] = rpcServerChild.Status()
	}
	if rpcServerNote != "" {
		resp["rpcServerError"] = rpcServerNote
	}
	// R115: раскладка меняется не «на лету» — модель получит новую схему при
	// следующей загрузке. Поле говорит это явно, чтобы WebUI не показывал
	// «применено» как «уже работает».
	if rpcChanged {
		resp["rpc_reload_required"] = true
	}
	// R304 (2026-10-30): пометка донора в ответе на приказ — ФАКТ решения воркера,
	// а не отправленное значение.
	//
	// ЗАЧЕМ. Балансер объявляет роль именно этим запросом; без поля он узнавал бы
	// о последствиях только следующим опросом /api/models, а оператор в панели —
	// вообще никогда. Здесь же сразу видно и «стала ли машина донором», и почему,
	// и какие копии у неё уже загружены вопреки роли (их придётся выгрузить).
	if backend != nil &&
		(appliedListContains(applied, "backendRole") || appliedListContains(applied, "rpcServerEnabled")) {
		resp["donor"] = backend.DonorMark()
	}
	writeJSON(w, http.StatusOK, resp)
}

// hasReloadedDefaults — true, если среди применённых полей есть параметры,
// которые влияют на инференс и требуют reload модели.
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
		"defaultRopeFreqBase":      true,
		"defaultRopeFreqScale":     true,
		"defaultRopeScalingType":   true,
		"defaultRopeScalingFactor": true,
		"defaultYarnExtFactor":     true,
		"defaultYarnAttnFactor":    true,
		"defaultYarnBetaFast":      true,
		"defaultYarnBetaSlow":      true,
	}
	for _, k := range applied {
		if loadAffecting[k] {
			return true
		}
	}
	return false
}

// reloadAllLoadedWithDefaults — перезагружает каждую загруженную модель
// с новыми дефолтными параметрами (DefaultCtxSize, DefaultBatchSize и т.д.).
// Запускает reload в горутинах параллельно (каждая модель независимо)
// и возвращает имена успешно запущенных и упавших.
//
// Использует backend.LoadModelWithOpts → reload через Unload + LoadModelWithOpts.
// Если модель уже загружена с теми же параметрами — reload пропускается
// (backend.LoadModelWithOpts вернёт "already_loaded" без фактической работы).
func reloadAllLoadedWithDefaults() ([]string, []string) {
	if backend == nil || currentConfig == nil {
		return nil, []string{"backend or config not initialized"}
	}
	// R304 (2026-10-30): «reload всех загруженных под новые дефолты» — это тоже
	// ПОДНЯТИЕ копий (unload + load), поэтому донору он запрещён.
	//
	// ЗАЧЕМ ОТДЕЛЬНАЯ ПРОВЕРКА. Смена любого load-affecting параметра в настройках
	// (n_ctx, kv-cache, mmap) запускает этот путь автоматически. Без запрета
	// достаточно было бы одного «Сохранить» в панели, чтобы донор снова собрал
	// себе копии — причём молча, как служебную операцию, без запроса от клиента.
	// Отказ виден в ответе на приказ (reload_failed) и в логе.
	if refused, mark := donorOwnCopyRefusedR304("reload-defaults"); refused {
		return nil, []string{i18n.T("cppworker.donor.reload_defaults_skipped") + " (" + mark.Reason + ")"}
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
		// R66d (2026-09-23): расчёт стратегии по VRAM перенесён ВНУТРЬ горутины,
		// после выгрузки старой копии модели (см. комментарий ниже). Здесь только
		// фиксируем снимок ModelInfo — `m` это переменная цикла, а в Go 1.21 она
		// переиспользуется между итерациями, поэтому передаём копию параметром.
		modelInfo := m
		// Запускаем reload в горутине — не блокируем ответ WebUI на минуту.
		go func(name string, modelPath string, info cppbackend.ModelInfo, loadOpts cppbackend.LoadModelOpts) {
			// R83/v62 (2026-10-02): без капа 120 с. Служебная операция после смены конфига
			// ждёт терминального состояния (contextWithTimeout(0) = без таймаута),
			// иначе на большой модели (A10, qwen3.8 15.7 ГБ) она обрывалась по часам.
			_, cancel := contextWithTimeout(0)
			defer cancel()
			if err := backend.UnloadModel(name); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: unload failed",
					"model", name, "error", err)
				return
			}
			// R66d: стратегию (число GPU-слоёв, kv-cache, mmap) считаем ПОСЛЕ
			// выгрузки. Раньше SelectStrategy/calculateOptimalGPULayersForModel
			// вызывались ДО неё, и оценка видела VRAM, занятую этой же моделью,
			// занижая результат. Живой случай: Settings Save (reload с defaults)
			// поставил gpu_layers=27/42, хотя следом cppworker сам писал
			// "VRAM sufficient (vramFreeMB=7069, требуется 6537)" — 15 слоёв
			// остались на CPU и генерация шла медленно при свободной VRAM.
			if *autoOffload {
				ggufMeta := cppbackend.GGUFModelMeta{
					Architecture: info.Architecture,
					NLayers:      info.NLayers,
					NEmbd:        info.NEmbd,
					NHeads:       info.NHeads,
					NKvHeads:     info.NKvHeads,
					SizeBytes:    int64(info.SizeBytes),
				}
				if globalEnv != nil {
					env := globalEnv.Get()
					strategy := SelectStrategy(&env, "", ggufMeta, info.ContextSize, -2, currentConfig)
					loadOpts.GPULayers = strategy.GPULayers
					loadOpts.KVCacheType = strategy.KVCacheType
					loadOpts.UseMmap = strategy.UseMmap
				} else {
					loadOpts.GPULayers = calculateOptimalGPULayersForModel(info, "")
				}
			}
			// R60.57: config-update reload — context.Background() (не HTTP-triggered).
			if err := backend.LoadModelWithOpts(context.Background(), name, modelPath, loadOpts); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: reload with new defaults failed",
					"model", name, "error", err)
			} else {
				logger.Get().Infow("handleCppWorkerUpdateConfig: reloaded with new defaults",
					"model", name,
					"context_size", loadOpts.ContextSize,
					"gpu_layers", loadOpts.GPULayers)
			}
		}(m.Name, m.Path, modelInfo, opts)
		started = append(started, m.Name)
	}
	return started, failed
}

// resetAllReloadAttempts — сбрасывает счётчик RAM fallback attempts для всех моделей.
// Вызывается после apply config — старые циклы reload-loop больше неактуальны.
func resetAllReloadAttempts() {
	// ramFallbackAttempts — sync.Map с per-model состоянием.
	// Здесь просто пересоздаём map через подмену: инвалидируем все ключи.
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
	// RAM fallback flags НЕ сохраняются в cppbackend.Config, но при reload
	// из .env мы перечитываем их, чтобы runtime-конфиг соответствовал env.
	// R60.18 F4: единый helper с тем же приоритетом CLI flag > env > default,
	// что и на startup (main.go). Без этого CLI flag был бы silent-overwritten
	// на /config/reload.
	applyRAMFallbackFromEnvWithSkip(ramFallbackNCtx, ramFallbackGpuLayers, ramFallbackMaxNCtx,
		isFlagSet("ram-fallback-n-ctx"), isFlagSet("ram-fallback-gpu-layers"), isFlagSet("ram-fallback-max-n-ctx"))
	// R304 (2026-10-30): роль НЕ приходит из окружения, а этот путь перечитывает
	// ТОЛЬКО env — без переноса роль молча стала бы «неизвестно».
	//
	// ПОЧЕМУ ЭТО ОПАСНЕЕ, ЧЕМ КАЖЕТСЯ. Живой бэкенд остался бы донором (его конфиг
	// здесь не трогается), а currentConfig — нет: получились бы две правды об
	// одном состоянии, и следующий приказ балансера с полем backendRole
	// сравнивался бы с «неизвестно». Роль — приказ балансера, reload из .env её не
	// отменяет.
	newCfg.BackendRole = currentConfig.BackendRole
	*currentConfig = newCfg
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reloaded", "config": currentConfig})
}

// defaultsFilePath — путь файла runtime-настроек CppWorker
// (config/cppworker-defaults.json по умолчанию).
//
// База настраивается через CPPWORKER_CONFIG_DIR. Переменная нужна в двух случаях:
//   - контейнер: каталог можно смонтировать томом. Иначе настройка живёт в
//     writable-слое и теряется при пересоздании контейнера (docker compose up
//     после смены образа) — см. docs/operations/deployment-stack.ru.md;
//   - тесты: уводят запись в temp, чтобы прогон `go test ./cmd/cppworker/` не
//     затирал отслеживаемый config/cppworker-defaults.json — он копируется в
//     образ, поэтому загрязнение дерева равносильно загрязнению образа.
func defaultsFilePath() string {
	dir := strings.TrimSpace(os.Getenv("CPPWORKER_CONFIG_DIR"))
	if dir == "" {
		dir = "config"
	}
	return filepath.Join(dir, "cppworker-defaults.json")
}

// saveConfigToDefaultsFile сохраняет runtime-настройку CppWorker в файл
// defaultsFilePath() — единственный источник истины для значений, которые
// оператор меняет из WebUI (env CPPWORKER_* имеет приоритет над файлом).
func saveConfigToDefaultsFile(cfg *cppbackend.Config) error {
	defaultsPath := defaultsFilePath()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// R106b (2026-10-12): каталог config/ может отсутствовать.
	//
	// ЖИВОЙ ДЕФЕКТ: в контейнере cppworker рабочая директория — /app, и каталога
	// config там НЕТ (проверено: ls /app → только бинарники и models). Поэтому
	// каждое сохранение из WebUI отвечало 200, а на диск не попадало ничего:
	//
	//	failed to save config to cppworker-defaults.json:
	//	open config/cppworker-defaults.json: no such file or directory
	//
	// Настройка (в т.ч. галочка размышлений) терялась при рестарте контейнера —
	// ровно то, что оператор описывал как «Сохранить отрабатывает, но не
	// применяется». Создаём каталог сами.
	//
	// ВАЖНО: это writable-слой контейнера — файл переживает restart, но НЕ
	// пересоздание контейнера (docker compose up после смены образа). Для
	// долговременного хранения каталог надо смонтировать томом (см.
	// docs/operations/deployment-stack.ru.md); env-переменные CPPWORKER_* остаются источником
	// истины и имеют приоритет над файлом.
	if dir := filepath.Dir(defaultsPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(defaultsPath, data, 0644)
}
