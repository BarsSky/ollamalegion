// handlers_effective_r116.go — R116: «что реально применится и почему».
//
// ЗАЧЕМ. На странице «GGUF models» в одном списке живут поля четырёх слоёв
// (дефолты бэкенда, runtime бэкенда, load-affecting и ПРОФИЛЬ модели), и по
// интерфейсу невозможно понять, какое значение победит при загрузке. Отсюда
// живые жалобы: «контекст не совпадает с явно указанным», «настройки перетёрли
// дефолтом для gemma4», «непонятно, где применится».
//
// ЭТОТ ЭНДПОИНТ ОТВЕЧАЕТ НА ЭТО ЧИСЛАМИ, не меняя логику загрузки:
//
//	GET /api/models/effective?model=<name>[&ctx=<n>][&kvCacheType=<t>]
//
// Возвращает:
//   - request  — то, что пришло в запросе (может отсутствовать);
//   - profile  — профиль модели (источник истины после запроса), как есть;
//   - default  — дефолты бэкенда;
//   - effective + source по каждому параметру: "request" | "profile" | "default";
//   - loaded   — что реально загружено СЕЙЧАС (может отличаться от effective:
//     тогда в memory.auto_reduced видно, что именно урезали и почему);
//   - memory   — вердикт memfit по ЭФФЕКТИВНОЙ конфигурации: влезает ли,
//     потолки n_ctx, и раскладка (memory_breakdown, R115).
//
// ПРИОРИТЕТ (единственный и видимый):
//
//	параметр запроса > профиль модели > дефолт бэкенда
//
// Профиль сериализуется целиком (json.Marshal), а не читается по полям: это
// избавляет от второй копии знания о структуре профиля — если поля изменятся,
// эндпоинт покажет их как есть, без правки здесь.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// effectiveSource — откуда взято победившее значение.
const (
	sourceRequest = "request"
	sourceProfile = "profile"
	sourceDefault = "default"
	sourceUnset   = "unset"
)

// resolveEffectiveInt — единственное правило приоритета для числового параметра.
//
// Чистая функция намеренно: правило приоритета — это контракт, и он обязан
// проверяться тестом, а не выводиться из чтения большого хендлера.
// Ноль и отрицательные значения означают «не задано» (в конфиге cppworker 0 —
// это «взять умолчание», а не «выключить»).
func resolveEffectiveInt(request, profile, def int) (int, string) {
	if request > 0 {
		return request, sourceRequest
	}
	if profile > 0 {
		return profile, sourceProfile
	}
	if def > 0 {
		return def, sourceDefault
	}
	return 0, sourceUnset
}

// resolveEffectiveStr — то же правило для строкового параметра (тип KV-cache).
func resolveEffectiveStr(request, profile, def string) (string, string) {
	clean := func(s string) string { return strings.TrimSpace(s) }
	if v := clean(request); v != "" {
		return v, sourceRequest
	}
	if v := clean(profile); v != "" {
		return v, sourceProfile
	}
	if v := clean(def); v != "" {
		return v, sourceDefault
	}
	return "", sourceUnset
}

// profileAsMap — профиль модели как есть (без знания его структуры здесь).
//
// nil-профиль даёт пустую карту: отсутствие профиля — законное состояние, и
// эндпоинт обязан это показывать, а не подставлять дефолт молча.
func profileAsMap(modelName string) map[string]interface{} {
	if profileSyncer == nil {
		return map[string]interface{}{}
	}
	prof := profileSyncer.applyProfileOnLoad(modelName)
	if prof == nil {
		return map[string]interface{}{}
	}
	raw, err := json.Marshal(prof)
	if err != nil {
		return map[string]interface{}{"error": "profile_marshal_failed"}
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]interface{}{"error": "profile_unmarshal_failed"}
	}
	return out
}

// intFromMap — числовое поле из карты профиля (json-числа приходят float64).
func intFromMap(m map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		case json.Number:
			if n, err := t.Int64(); err == nil {
				return int(n)
			}
		}
	}
	return 0
}

// strFromMap — строковое поле из карты профиля.
func strFromMap(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

// handleModelEffective — GET /api/models/effective?model=<name>.
func handleModelEffective(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("model"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "model query parameter is required")
		return
	}
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}

	q := r.URL.Query()
	// Запрос может задать контекст как числом, так и в виде «хочу столько».
	reqCtx, _ := strconv.Atoi(strings.TrimSpace(q.Get("ctx")))
	reqKV := q.Get("kvCacheType")

	profile := profileAsMap(name)
	// ВАЖНО ПРО ИМЕНА (живая находка на gemma4, 2026-10-14): профиль модели и
	// конфиг бэкенда называют одно и то же РАЗНЫМИ полями —
	//   профиль: contextLength, numGpuLayers, kvCacheType, batchSize, flashAttn, useMmap, ignoreDefaults
	//   конфиг:  defaultCtxSize, defaultGpuLayers, defaultKvCacheType, …
	// Именно из-за этого расхождения «настройки в UI» и «то, что применилось»
	// выглядели противоречиво: профиль с contextLength=8192 спорил с дефолтом
	// 65536, а интерфейс показывал дефолт.
	profCtx := intFromMap(profile, "contextLength", "contextSize", "nCtx", "n_ctx", "ctxSize")
	profKV := strFromMap(profile, "kvCacheType", "kv_cache_type")

	var defCtx, defGPU int
	defKV := ""
	if currentConfig != nil {
		defCtx = currentConfig.DefaultCtxSize
		defGPU = currentConfig.DefaultGPULayers
		defKV = currentConfig.DefaultKVCacheType
	}

	effCtx, ctxSource := resolveEffectiveInt(reqCtx, profCtx, defCtx)
	effKV, kvSource := resolveEffectiveStr(reqKV, profKV, defKV)

	resp := map[string]interface{}{
		"model":   name,
		"request": map[string]interface{}{"ctx": reqCtx, "kv_cache_type": reqKV},
		"profile": profile,
		"default": map[string]interface{}{
			"ctx": defCtx, "gpu_layers": defGPU, "kv_cache_type": defKV,
		},
		"effective": map[string]interface{}{
			"ctx": effCtx, "kv_cache_type": effKV,
			"sources": map[string]string{"ctx": ctxSource, "kv_cache_type": kvSource},
		},
	}

	// Что загружено СЕЙЧАС: если отличается от effective, это и есть та самая
	// «подмена настроек», которую нужно показывать, а не прятать.
	if info, err := backend.GetModel(name); err == nil && info != nil {
		loaded := map[string]interface{}{
			"state":                  string(info.State),
			"ctx":                    info.ContextSize,
			"gpu_layers":             info.GPULayers,
			"kv_cache_type":          info.KVCacheType,
			"reasoning":              info.ReasoningEnabled,
			"differs_from_effective": info.ContextSize > 0 && effCtx > 0 && info.ContextSize != effCtx,
		}
		resp["loaded"] = loaded
	}

	// Вердикт памяти по ЭФФЕКТИВНОЙ конфигурации: тот же расчёт, что и загрузка
	// (memfit), поэтому отчёт и решение не могут разойтись.
	//
	// R132 (2026-10-16): расчёт — БЕЗ дозвона до узлов раскладки. Это справочная
	// ручка (карточка модели в WebUI), а `MemfitBudget()` дозванивается: на узле,
	// который принимает TCP и молчит, этот вызов уходил в C-мост, то есть открытие
	// карточки модели могло уронить мастер. Кэш уже собранного снимка учитывается
	// (MemfitBudgetCached), так что вердикт совпадает с решением о загрузке.
	//
	// Второй дозвон был ВНУТРИ помощника (feasibilityFromMemfit →
	// RPCCapacityBytes) — живая проверка поймала его уже после первой правки:
	// ручка не отвечала 30 с, а дублёр узла принимал соединение. Поэтому здесь
	// именно feasibilityFromMemfitCached.
	if spec, ok := backend.MemfitSpec(name); ok {
		kvType := effKV
		if strings.TrimSpace(kvType) == "" {
			kvType = backend.EffectiveKVCacheType("")
		}
		if effCtx > 0 {
			if f, verdict, ok2 := feasibilityFromMemfitCached(spec, backend.MemfitBudgetCached(), kvType, effCtx); ok2 {
				mem := map[string]interface{}{
					"stage":                f.Stage,
					"feasible":             f.Stage != nctxStageInfeasible,
					"degraded":             f.Degraded,
					"max_vram_n_ctx":       f.MaxVRAMNCtx,
					"max_ram_n_ctx":        f.MaxRAMNCtx,
					"gguf_max_context":     f.GGUFMaxContext,
					"feasible_max_context": f.FeasibleMaxNCtx,
					"suggestion":           nctxSuggestion(f),
					"verdict":              verdict.String(),
				}
				if f.Breakdown != nil {
					mem["memory_breakdown"] = f.Breakdown.Fields()
				}
				resp["memory"] = mem
			}
		} else {
			resp["memory"] = map[string]interface{}{
				"error": "ctx_not_determined",
				"hint":  i18n.T("cppworker.handlers_effective_r116.ctx_hint"),
			}
		}
	} else {
		resp["memory"] = map[string]interface{}{"error": "model_metadata_unavailable"}
	}

	if logger.Get() != nil {
		logger.Get().Infow(i18n.T("cppworker.handlers_effective_r116.log_effective"),
			"model", name, "ctx", effCtx, "ctx_source", ctxSource,
			"kv_cache_type", effKV, "kv_source", kvSource)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ensure cppbackend import is used (тип ModelInfo приходит из него).
var _ = cppbackend.StateLoaded
