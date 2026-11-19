package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// UpdateLastUsed продлевает время жизни модели в VRAM на duration d от now.
//
// Используется для обработки Ollama-семантики keep_alive:
//   - "5m" → lastUsedAt = now + 5 минут
//   - "0"  → unload модели (вызывается через UnloadModel напрямую, не через этот метод)
//
// Метод безопасен для concurrent вызовов (atomic.Value.Store lock-free).
// Если duration <= 0 — ставит lastUsedAt = now (модель только что использована).
// Если модель не загружена — тихий no-op.
func (b *Backend) UpdateLastUsed(name string, d time.Duration) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return
	}

	newTime := time.Now()
	if d > 0 {
		newTime = newTime.Add(d)
	}
	inst.lastUsedAt.Store(newTime)

	logger.Get().Debugw("model keep_alive extended",
		"name", name,
		"keepAliveDuration", d.String(),
		"newLastUsedAt", newTime.Format(time.RFC3339Nano))
}

// ResourceLimits — лимиты VRAM/RAM для загруженной модели.
//
// Используется cppworker'ом для расширения ответа /api/models
// (поля max_vram_n_ctx, max_ram_n_ctx, available_vram_mb, available_ram_mb)
// и балансировщиком для принятия решений по n_ctx reload preflight
// (см. internal/balancer/preflight_nctx.go:collectPreflightState).
//
// 2026-06-26 BUGFIX: до этой фичи балансер видел max_vram_n_ctx=0
// и model_max_context=0 для всех моделей (env_log.txt строки 121, 133),
// потому что cppworker не сообщал эти поля в /api/models. Без них
// preflight не мог расчитать target_n_ctx и ставил хардкод 8192,
// что приводило к ошибкам при reload на модели типа
// Qwen3.6-35B-A3B (training context ~32k, фактически нужно 16-32k).
type ResourceLimits struct {
	// VRAM
	TotalVRAMMB     uint64 // суммарный объём VRAM всех GPU
	AvailableVRAMMB uint64 // свободная VRAM (Total - уже занятое моделями)
	MaxVRAMNCtx     int    // макс. n_ctx, который влезет в свободную VRAM
	// RAM (для mmap fallback, если модель не влезает в VRAM)
	TotalRAMMB     uint64 // суммарный объём системной RAM
	AvailableRAMMB uint64 // свободная RAM (Total - занятое)
	MaxRAMNCtx     int    // макс. n_ctx с выгрузкой части слоёв в RAM (VRAM + RAM)
	// VRAMKnown — были ли вообще данные о свободной VRAM. Ноль в MaxVRAMNCtx при
	// VRAMKnown=true означает «веса не влезают в VRAM», при false — «неизвестно».
	// R83 шаг 3: различать эти случаи обязательно, иначе нельзя сказать, стоит ли
	// предупреждать о режиме загрузки (прежний перегруженный ноль это запрещал).
	VRAMKnown bool
	// R592 (2026-11-19): «тень» резерва VRAM — ВТОРОЕ число того же потолка,
	// посчитанное так, как его считает C-мост: вся свободная VRAM без резерва и
	// без скидки на утилизацию.
	//
	// ЗАЧЕМ. Одно и то же число «сколько окна влезает» имело два источника с
	// разными правилами: Go (это поле, MaxVRAMNCtx) вычитает
	// CPPWORKER_VRAM_OVERHEAD_MB (дефолт 2048) и умножает на долю утилизации, а
	// `c/bridge/bridge.c` (`g_max_vram_n_ctx`, попадает в текст ошибки декода) не
	// вычитает ничего. Живой факт 2026-11-19, 27B/A10: мост сообщил
	// max_vram_n_ctx=251254, окно поднялось до 201216, после загрузки свободно
	// 414 MB из 22 502, и декод промпта 33 812 токенов упал с `code 1` при
	// измеренном `CUDA0 compute buffer = 316.75 MiB`.
	//
	// РЕШЕНИЕ ВЛАДЕЛЬЦА (2026-11-19): сначала ТЕНЬ — показывать оба числа, решения
	// не менять. MaxVRAMNCtx остаётся тем, что применяется (поведение не меняется),
	// MaxVRAMNCtxUnreserved — справочное, для оператора и для WebUI.
	MaxVRAMNCtxUnreserved int
	// VRAMReserveMB — сколько именно MB было вычтено из свободной VRAM при расчёте
	// MaxVRAMNCtx (то самое CPPWORKER_VRAM_OVERHEAD_MB). Нужно, чтобы разницу двух
	// чисел выше можно было объяснить, а не гадать.
	VRAMReserveMB uint64
	// Прочее
	ModelMaxContext int // максимальный контекст из GGUF training metadata (gemma-3: 131072, llama-3: 8192, qwen3.6-72B: 32768)
}

// CalculateResourceLimits — потолки n_ctx для модели.
//
// R83 (шаг 3 миграции): расчёт идёт ТОЛЬКО через internal/memfit — правило
// KV-слоёв из kv_layers.go, доступная RAM из ProbeRAM, явная Policy. Прежняя
// собственная формула расходилась и с решением о загрузке, и с memfit: считала KV
// по всем блокам (а не по attention-слоям), брала head_dim из n_embd/n_heads (а не
// attention.key_length) и сравнивала с MemTotal. Теперь «в логе одно, в решении
// другое» невозможно по построению.
//
// Если метаданные модели недоступны, MaxVRAMNCtx/MaxRAMNCtx остаются 0, но
// VRAMKnown показывает, были ли данные о VRAM — ноль больше не означает
// одновременно «неизвестно» и «не влезает».
//
// Используется:
//   - cppworker: handleListModels (/api/models) и пред-загрузочный гейт n_ctx;
//   - тесты cppbackend.
func (b *Backend) CalculateResourceLimits(name string) ResourceLimits {
	return b.calculateResourceLimits(name, b.MemfitBudget())
}

// CalculateResourceLimitsLocal — то же, но БЕЗ обращения к RPC-узлам.
//
// R131 (2026-10-16): для справочных (read-only) ручек вроде GET /api/models.
// Раньше этот путь звал расчёт с RPC, отдавал недоступный узел в C-мост, и
// llama.cpp вызывал abort() — мастер падал от ЧТЕНИЯ СПИСКА МОДЕЛЕЙ. Ёмкость
// донора справочному ответу не нужна: раскладку планирует путь загрузки.
func (b *Backend) CalculateResourceLimitsLocal(name string) ResourceLimits {
	return b.calculateResourceLimits(name, b.MemfitBudgetLocal())
}

// CalculateResourceLimitsCached — лимиты с учётом ПОСЛЕДНЕГО СОБРАННОГО снимка
// узлов, без их опроса (R132, 2026-10-16).
//
// Для справочных ручек: они обязаны отвечать мгновенно и не зависеть от того,
// жив ли узел раскладки, но при этом показывать ту же ёмкость, что и решение о
// загрузке, — иначе WebUI и балансер видят «не влезает» там, где раскладка на
// донора проходит.
func (b *Backend) CalculateResourceLimitsCached(name string) ResourceLimits {
	return b.calculateResourceLimits(name, b.MemfitBudgetCached())
}

func (b *Backend) calculateResourceLimits(name string, budget memfit.Budget) ResourceLimits {
	var limits ResourceLimits

	limits.VRAMKnown = budget.VRAMKnown
	limits.TotalVRAMMB = uint64(budget.VRAMTotal.MiB())
	limits.AvailableVRAMMB = uint64(budget.VRAMFree.MiB())
	limits.TotalRAMMB = uint64(budget.RAMTotal.MiB())
	limits.AvailableRAMMB = uint64(budget.RAMAvail.MiB())

	spec, ok := b.MemfitSpec(name)
	if !ok {
		return limits
	}

	// Тип KV-cache: у загруженной модели — фактический, иначе дефолт конфига.
	loadedKV := ""
	var loadedInfo *ModelInfo
	if info, err := b.GetModel(name); err == nil && info != nil {
		loadedKV = info.KVCacheType
		loadedInfo = info
	}
	kvType := MemfitKVType(b.EffectiveKVCacheType(loadedKV))

	// R306 (2026-10-30): веса ЗАГРУЖЕННОЙ модели возвращаются в бюджет.
	//
	// ЗАЧЕМ. memfit.Ceilings считает потолок как (свободная VRAM − размер весов) /
	// KV на токен — то есть предполагает, что модель ещё не загружена. Для
	// загруженной модели веса УЖЕ вычтены из свободной VRAM, и Ceilings вычитает
	// их второй раз.
	//
	// ЖИВОЙ ФАКТ (стенд, gemma-4-E4B, ctx 32768, модель работает и отвечает):
	// /api/models отдавал max_vram_n_ctx=0 при vram_known=true и
	// gguf_max_context=131072. По контракту поля это «веса не влезают ни при каком
	// gpu_layers» — ложь: модель загружена и обслуживает запросы. Следствия были
	// ровно те, из-за которых появился этот разбор: отказ по окну не мог сослаться
	// на физический предел (он «0»), а оператор не понимал, поможет ли поднять
	// LB_NCTX_RELOAD_MAX_N_CTX — то есть какие числа реально связывают.
	budgetForCeilings := budget
	if restored := restoredWeightsBytesR306(spec, loadedInfo); restored.Positive() {
		budgetForCeilings.VRAMFree += restored
	}

	// R396 (2026-11-04): потолки считаются в ТОМ ЖЕ режиме mmap, в котором модель
	// загружена (или загрузится). Иначе справочные числа обещают недостижимое:
	// живой случай A10 — 27B Q4_K_M при RAM 20 GB показывал max_ram_n_ctx=82824
	// и 422 на n_ctx=131072, хотя с mmap веса читаются с диска и в RAM обязаны
	// поместиться только KV и буферы.
	ceilPolicy := MemfitPolicyForMmap(b.EffectiveUseMmapForMemfit(name))
	exact, hard := memfit.Ceilings(spec, kvType, budgetForCeilings, ceilPolicy)
	limits.MaxVRAMNCtx = exact
	limits.MaxRAMNCtx = hard
	limits.ModelMaxContext = spec.TrainCtx

	// R592: «тень» того же потолка без резерва VRAM — как считает C-мост.
	//
	// Почему отдельный вызов Ceilings, а не пересчёт вручную: формула KV/SWA-слоёв
	// живёт в memfit, и второй её экземпляр здесь разошёлся бы с первым (этот класс
	// дефекта уже стоил 16-кратной ошибки в мосте — см. c/bridge/kv_budget.h).
	limits.VRAMReserveMB = uint64(budgetForCeilings.VRAMReserve.MiB())
	_, limits.MaxVRAMNCtxUnreserved = CeilingsWithReserveShadowR592(spec, kvType, budgetForCeilings, ceilPolicy)

	if logger.Get() != nil {
		logger.Get().Debugw(i18n.T("cppbackend.backend.resource_limits_ceilings"),
			"model", name,
			"kv_type", string(kvType),
			"kv_layers", spec.EffectiveKVLayers(),
			"kv_head_dim", spec.EffectiveKVHeadDim(),
			"vram_known", budget.VRAMKnown,
			"vram_free_mb", budget.VRAMFree.MiB(),
			"ram_avail_mb", budget.RAMAvail.MiB(),
			"ram_source", budget.Source,
			"use_mmap", ceilPolicy.WeightsMmap,
			"max_exact_fit_n_ctx", exact,
			"max_hard_n_ctx", hard,
			"gguf_max_context", spec.TrainCtx)
	}
	return limits
}

// CeilingsWithReserveShadowR592 — применяемый и справочный потолки окна по VRAM.
//
// ЗАЧЕМ. Одно и то же «сколько окна влезает» считалось двумя источниками по
// разным правилам: Go (см. calculateResourceLimits: свободная VRAM МИНУС резерв
// CPPWORKER_VRAM_OVERHEAD_MB, умноженная на долю утилизации) и C-мост
// (`c/bridge/bridge.c`, g_max_vram_n_ctx: вся свободная VRAM, без резерва).
// Живой факт 2026-11-19, 27B/A10: мост сообщил max_vram_n_ctx=251254 (без
// резерва), окно подняли до 201216, после загрузки свободно 414 MB из 22 502, и
// декод промпта 33 812 токенов упал с `code 1` при измеренном
// `CUDA0 compute buffer = 316.75 MiB`.
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА (2026-11-19): сначала тень. Поэтому первый результат —
// ровно то, что применялось и раньше (поведение не меняется), второй —
// справочное число «как считает мост», для оператора и WebUI.
//
// Второй расчёт — тот же memfit.Ceilings, отличаются ровно ДВА входа политики:
// резерв → 0 и доля утилизации → 1.0. Своей формулы KV здесь нет намеренно:
// второй экземпляр формулы уже один раз разошёлся с первым на 16 раз
// (c/bridge/kv_budget.h).
func CeilingsWithReserveShadowR592(spec memfit.ModelSpec, kvType memfit.KVType,
	budget memfit.Budget, policy memfit.Policy) (applied int, unreserved int) {
	applied, _ = memfit.Ceilings(spec, kvType, budget, policy)

	unreservedBudget := budget
	unreservedBudget.VRAMReserve = memfit.Bytes(0)
	unreservedPolicy := policy
	unreservedPolicy.VRAMUtil = 1.0
	if v, _ := memfit.Ceilings(spec, kvType, unreservedBudget, unreservedPolicy); v > 0 {
		unreserved = v
	}
	return applied, unreserved
}

// InjectLoadedModelForTest — test helper, помечает model как loaded
// в in-memory map без реальной C-bridge загрузки. Используется в
// handlers_unload_safety_test.go для тестирования inflight safety check
// без необходимости загружать реальную GGUF.
//
// Round 51.6 (2026-08-20): добавлен для тестов R51.6 (handleUnloadModel
// safety). Вызывает handle.FreeModel() в init — но handle == nil (мы
// создаём modelInstance вручную, без C-bridge), см. обёртку.
func (b *Backend) InjectLoadedModelForTest(name string) error {
	if b == nil {
		return fmt.Errorf("nil backend")
	}
	if name == "" {
		return fmt.Errorf("empty name")
	}
	// Инициализируем счётчики если не были
	if b.inFlight == nil {
		b.inFlight = NewInFlightCounter()
	}
	if b.activeGenerations == nil {
		b.activeGenerations = NewActiveGenerations()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.models[name]; exists {
		return fmt.Errorf("model %s already injected", name)
	}
	// modelInstance.handle = nil — это ОК потому что UnloadModel() проверяет
	// `if inst.handle != nil` перед FreeModel() (backend.go:1506-1509).
	b.models[name] = &modelInstance{
		info: ModelInfo{
			Name:        name,
			Path:        "/test/" + name,
			State:       StateLoaded,
			ContextSize: 4096,
		},
		handle: nil,
	}
	return nil
}

// SetModelReasoningEnabledForTest — test helper: выставить resolved per-model
// reasoning у уже инжектированного инстанса (см. InjectLoadedModelForTest).
//
// Зачем: R83/v67 (2026-10-02) — живой дефект «галочка размышлений в WebUI
// включена, а в ответе размышлений нет». Проверка причины требует состояния
// inst.reasoningEnabled=true без реальной загрузки GGUF, иначе тест
// воспроизводит только ветку «выключено».
func (b *Backend) SetModelReasoningEnabledForTest(name string, enabled bool) error {
	if b == nil {
		return fmt.Errorf("nil backend")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	inst, ok := b.models[name]
	if !ok || inst == nil {
		return fmt.Errorf("model %s is not injected", name)
	}
	inst.info.ReasoningEnabled = enabled
	return nil
}
