// handlers_model_list.go — R624 (2026-11-19): список моделей и их метрики
// (`/api/models`) — то, что читает балансер и WebUI.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_model.go` оставался «файлом на 2 000 строк»: найти в нём
// ручку списка можно было только чтением целиком. Здесь — ровно одна поверхность:
// `handleListModels`, её поля режима загрузки, `nctxModeFields`-сборка и «тень»
// резерва VRAM (R592–R594). Перенос дословный.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` (включая
// `handlers_model_round37_test.go`, наборы `nctx_feasibility_r83_test.go` и
// `nctx_shadow_fields_r592_test.go`) зелёные до и после.
package main

import (
	"net/http"
	"os"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
)

func handleListModels(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	mm := backend.ModelManager()

	// 1. Собираем VRAM/RAM лимиты для всех загруженных моделей.
	// Балансер выбирает МИНИМАЛЬНЫЙ max_vram_n_ctx, потому что несколько моделей
	// одновременно жить в VRAM не могут — общий лимит делится между ними.
	var totalVRAMMB, availableVRAMMB, totalRAMMB, availableRAMMB uint64
	maxVRAMNCtx := 0
	maxRAMNCtx := 0
	// R592: top-level «тень» резерва VRAM — минимум по моделям, как и max_vram_n_ctx.
	maxVRAMNCtxUnreserved := 0
	vramReserveMB := 0
	modelMaxContext := 0

	// Round 37 (2026-08-18): per-model feasible tracking for conservative-profile detection.
	type feasibleAgg struct {
		feasibleMax int
		ggufMax     int
		// R83 (2026-09-25): граница «влезает в VRAM» — нужна, чтобы показать
		// оператору, что загруженный n_ctx выше неё (режим partial_offload).
		vramMax int
		// R83 (2026-09-27): потолок n_ctx для загрузки ЦЕЛИКОМ в VRAM.
		// Именно по нему считается n_ctx_degraded: max_vram_n_ctx для 27B равен
		// нулю (веса не влезают даже одним слоем), и прежнее сравнение с ним
		// объявляло модель деградированной при любом контексте.
		exactFitMax int
		// R83 шаг 3: различать «VRAM неизвестна» и «известна, но веса не влезают».
		vramKnown bool
		// R592 (2026-11-19): «тень» резерва VRAM — тот же потолок без резерва
		// (так его считает C-мост) и размер самого резерва. Решения не меняют,
		// показывают оператору, из чего получилось применяемое число.
		unreservedMax int
		reserveMB     int
	}
	perModelFeasible := make(map[string]feasibleAgg, len(models))
	// R83: top-level vram_known отражает НАЛИЧИЕ GPU, а не наличие загруженных
	// моделей: раньше при count=0 поле было false, хотя GPU есть.
	// R131 (2026-10-16): справочная ручка НЕ ходит к RPC-узлам.
	//
	// ЖИВОЙ ДЕФЕКТ: здесь считался бюджет с RPC, недоступный узел уезжал в C-мост,
	// llama.cpp вызывал abort() — и МАСТЕР ПАДАЛ ОТ GET /api/models (обычного
	// чтения списка моделей). Ёмкость донора справочному ответу не нужна:
	// раскладку планирует путь загрузки, где есть явное намерение и гейт узлов.
	//
	// R132 (2026-10-16): «не ходить к узлам» ≠ «не знать про узлы». Берём
	// ПОСЛЕДНИЙ СОБРАННЫЙ снимок (MemfitBudgetCached): дозвона нет ни при каком
	// состоянии узла, но если путь загрузки только что измерял донора, ответ
	// показывает ту же ёмкость, что и решение о раскладке (иначе балансер видел
	// «не влезает» там, где раскладка на донора проходит).
	vramKnownAgg := backend.MemfitBudgetCached().VRAMKnown

	for _, m := range models {
		if m.State != cppbackend.StateLoaded {
			continue
		}
		limits := backend.CalculateResourceLimitsCached(m.Name)
		if limits.VRAMKnown {
			vramKnownAgg = true
		}
		if limits.TotalVRAMMB > totalVRAMMB {
			totalVRAMMB = limits.TotalVRAMMB
		}
		if limits.AvailableVRAMMB > availableVRAMMB {
			availableVRAMMB = limits.AvailableVRAMMB
		}
		if limits.TotalRAMMB > totalRAMMB {
			totalRAMMB = limits.TotalRAMMB
		}
		if limits.AvailableRAMMB > availableRAMMB {
			availableRAMMB = limits.AvailableRAMMB
		}
		if maxVRAMNCtx == 0 || limits.MaxVRAMNCtx < maxVRAMNCtx {
			maxVRAMNCtx = limits.MaxVRAMNCtx
		}
		// R592: та же логика минимума — для справочного «безрезервного» потолка.
		if maxVRAMNCtxUnreserved == 0 || (limits.MaxVRAMNCtxUnreserved > 0 && limits.MaxVRAMNCtxUnreserved < maxVRAMNCtxUnreserved) {
			maxVRAMNCtxUnreserved = limits.MaxVRAMNCtxUnreserved
		}
		if vramReserveMB == 0 && limits.VRAMReserveMB > 0 {
			vramReserveMB = int(limits.VRAMReserveMB)
		}
		if maxRAMNCtx == 0 || limits.MaxRAMNCtx < maxRAMNCtx {
			maxRAMNCtx = limits.MaxRAMNCtx
		}
		if modelMaxContext == 0 || limits.ModelMaxContext > modelMaxContext {
			modelMaxContext = limits.ModelMaxContext
		}

		// Round 37: per-model feasible calc. feasible_i = min(VRAM ctx, RAM ctx, GGUFMax).
		feasibleI := limits.MaxVRAMNCtx
		if limits.MaxRAMNCtx > 0 && (feasibleI == 0 || limits.MaxRAMNCtx < feasibleI) {
			feasibleI = limits.MaxRAMNCtx
		}
		if m.GGUFContextLength > 0 && (feasibleI == 0 || m.GGUFContextLength < feasibleI) {
			feasibleI = m.GGUFContextLength
		}
		perModelFeasible[m.Name] = feasibleAgg{
			feasibleMax:   feasibleI,
			ggufMax:       m.GGUFContextLength,
			vramMax:       limits.MaxVRAMNCtx,
			exactFitMax:   limits.MaxVRAMNCtx,
			vramKnown:     limits.VRAMKnown,
			unreservedMax: limits.MaxVRAMNCtxUnreserved,
			reserveMB:     int(limits.VRAMReserveMB),
		}
	}

	// 2. Fallback: если ни одна модель не загружена — берём модель из
	// GGUFModelMeta по имени name (берём только header, не грузим в llama.cpp).
	// GGUFModelMeta не содержит ContextLength — для расчёта лимитов нужно
	// вызвать cppbackend.ReadGGUFHeader(m.Path) отдельно. Здесь оставляем
	// modelMaxContext=0, балансер тогда использует дефолтные значения.
	if maxVRAMNCtx == 0 && mm != nil {
		_ = mm // заглушено: пусть лимиты остаются 0, preflight всё равно выключится
	}

	// 2a. Fallback: if no models loaded, query bridge directly for live VRAM/RAM.
	// Fixes /api/models returning available_vram_mb:0 when no model is loaded yet.
	if totalVRAMMB == 0 || availableVRAMMB == 0 {
		if dev, err := bridge.GetGPUInfo(0); err == nil && dev != nil {
			totalVRAMMB = uint64(dev.VRAMTotalMB)
			if availableVRAMMB == 0 {
				availableVRAMMB = uint64(dev.VRAMFreeMB)
			}
		}
	}
	if totalRAMMB == 0 || availableRAMMB == 0 {
		if ramGB := cppbackend.GetSystemRAMGB(); ramGB > 0 {
			totalRAMMB = uint64(ramGB) * 1024
			if availableRAMMB == 0 {
				availableRAMMB = totalRAMMB - 4096
			}
		}
	}

	// 3. Если лимиты остались 0 (нет загруженной модели, нет VRAM), пробуем
	// оценить через первую GGUF в каталоге — но для cppworker без моделей
	// балансер получит нули, и это корректно (preflight тогда отключён).
	// Round 37 (2026-08-18): build per-model enriched list with feasible_max_context.
	// R60.4 (2026-09-04): webui meta — добавлены size (alias для size_bytes,
	// webui-совместимость) и quantization (parse из path через parseQuantization).
	// До R60.4 webui получал size=0/quantization="" потому что notifyModelLoaded
	// callback не передавал их, а /api/models (который balancer poll'ит) тоже не
	// отдавал. Карточка модели в webui показывала пустые "Размер: -" и
	// "Квантизация: -".
	//
	// R60.4 fallback: m.SizeBytes иногда 0 (cppbackend берёт из llama.cpp gguf meta,
	// которая для некоторых моделей не заполнена). Делаем os.Stat на m.Path —
	// получаем реальный file size.
	enrichedModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		// R60.4: fallback для size — реальный file size через os.Stat.
		effectiveSize := m.SizeBytes
		if effectiveSize == 0 && m.Path != "" {
			if fi, statErr := os.Stat(m.Path); statErr == nil {
				effectiveSize = uint64(fi.Size())
			}
		}
		entry := map[string]interface{}{
			"name":         m.Name,
			"path":         m.Path,
			"state":        m.State,
			"architecture": m.Architecture,
			"n_layers":     m.NLayers,
			// R248 (2026-10-27): фактический offload и признак деградации.
			// Раньше /api/models показывал только запрошенное gpu_layers, и
			// тяжёлая модель на узле с малой VRAM выглядела «нормально
			// загруженной», хотя в логе было «offloaded 20/66 layers to GPU» и
			// ~10 ГБ в CPU_Mapped. Оператор не видел причину медленности.
			"gpu_layers_actual": m.GPULayers,
			"offload_degraded":  m.OffloadDegraded,
			"offload_note":      m.OffloadNote,
			"n_embd":            m.NEmbd,
			"n_vocab":           m.NVocab,
			// R660 (2026-11-20): ГЕОМЕТРИЯ KV — в ответ, а не только в расчёт.
			//
			// ЖИВОЙ ФАКТ (проверка R659 на локальном стенде): воркер считает
			// потолок окна по nLayers × nKvHeads × headDimK (max_vram_n_ctx,
			// max_exact_fit_n_ctx), то есть эти числа у него ЕСТЬ, но в /api/models
			// они не публиковались. Балансер, решающий «поднимется ли модель с
			// нужным окном на СВОБОДНОМ узле» (R659), получал пустую геометрию и
			// уходил в «проверить нечем» (verified=false) — то есть грузил на
			// свободный узел без проверки окна. Балансерный поллер уже разбирает
			// ровно эти имена полей (llamacpp_metrics_poller.go: n_kv_heads,
			// head_dim_k, head_dim_v), поэтому достаточно их отдать.
			"n_kv_heads":          m.NKvHeads,
			"head_dim_k":          m.HeadDimK,
			"head_dim_v":          m.HeadDimV,
			"context_size":        m.ContextSize,
			"gguf_context_length": m.GGUFContextLength,
			"size_bytes":          effectiveSize,
			"size":                effectiveSize,                               // R60.4: alias (webui gguf-renderer-detail.js:232)
			"size_vram":           estimateVRAMSizeForModel(&m, effectiveSize), // R66.5: VRAM estimate для balancer poller
			"quantization":        parseQuantization(m.Path),                   // R60.4
			"loaded_at":           m.LoadedAt,
			"gpu_count":           m.GPUCount,
			"kv_cache_type":       m.KVCacheType,
			// R83 (2026-09-29): число параллельных слотов модели (n_parallel).
			//
			// Зачем в ответе. Балансер НЕ мог узнать, что модель умеет держать
			// несколько сессий: поля не было в /api/models, поэтому он держал
			// MaxConcurrentReqs=1 и сериализовал запросы, даже когда модель
			// загружена с parallel=2/3 и физически готова их обслуживать
			// (проверено: два одновременных запроса напрямую в cppworker с
			// parallel=2 — оба 200).
			"parallel":   m.Parallel,
			"batch_size": m.BatchSize,
			// R83 (2026-09-30): окно, которое реально получит ОДИН клиент.
			//
			// context_size в этом же ответе — СУММАРНОЕ окно (llama.cpp при
			// kv_unified=false считает n_ctx_seq = n_ctx / n_seq_max). Без этого
			// поля «16384 + parallel=2» читалось как 16384 на клиента, хотя
			// каждый слот получает 8192.
			"context_per_seq": func() int {
				if m.ContextPerSeq > 0 {
					return m.ContextPerSeq
				}
				return cppbackend.ContextPerSlot(m.ContextSize, m.Parallel)
			}(),
			// R119 (2026-10-15): max_slots — сколько запросов модель РЕАЛЬНО
			// обслуживает ОДНОВРЕМЕННО, а не сколько слотов заведено.
			//
			// ЖИВОЙ ЗАМЕР (стенд, CPPWORKER_ENABLE_BATCHED_PARALLEL=false,
			// parallel=2): два одновременных /api/chat обслуживались
			// ПОСЛЕДОВАТЕЛЬНО — второй получил первый байт ровно тогда, когда
			// закончился первый (firstData 11.76 c против total 12.29 c).
			// Причина в самом cppworker: legacy-путь держит inst.mu на ВЕСЬ
			// инференс (backend.go, Round 8 race-fix), поэтому llama_decode
			// сериализуется независимо от числа слотов SlotManager. Так это и
			// описано у ModelInfo.BatchedParallel: «false = legacy Round 13
			// multi-slot path с serialized llama_decode».
			//
			// ПОЧЕМУ ЭТО КРИТИЧНО НАРУЖУ. Балансер берёт вместимость бэкенда из
			// этого числа (max_slots/parallel) и пропускает два клиента вперёд.
			// Второй молча блокируется внутри SlotManager.Acquire — без
			// X-Queue-Wait-Ms, без позиции в очереди и без строки в UI. Ровно
			// так выглядит жалоба «два запроса с одной машины не разделяются».
			//
			// Поэтому max_slots = способность: без batched-планировщика она
			// равна 1. Настроенное `parallel` не меняем — оно остаётся в ответе
			// как конфигурация (и как число слотов llama.cpp).
			"max_slots": func() int {
				if !m.BatchedParallel {
					return 1
				}
				if m.Parallel > 0 {
					return m.Parallel
				}
				return 1
			}(),
			// R119: режим планировщика — виден наружу, потому что именно он
			// объясняет разницу между parallel=2 и max_slots=1. UI показывает
			// «Batched: ON/OFF», оператор видит, что параллелизм не включён.
			"batched_parallel": m.BatchedParallel,
			// R118.16 (2026-10-14): точная раскладка слоёв по устройствам —
			// llama.cpp печатает её в лог при загрузке, мост ловит строки
			// (c/bridge/layers_plan.go). Здесь это ФАКТ, а не расчёт: WebUI
			// показывает именно его, и оператор сверяет с `docker logs`.
			// Пусто (ключ отсутствует) = перехватить не удалось; это не то же
			// самое, что «все слои на CPU».
			//
			// Имя `device_layers_exact` — то же, что в /config/runtime: один
			// факт называется одинаково везде. Отдельное имя нужно потому, что
			// `device_layers` в ответе балансера занято ОБЪЕКТОМ-планом
			// (estimated true/false), и путать факт с планом нельзя.
			"device_layers_exact": func() interface{} {
				if len(m.DeviceLayers) == 0 {
					return nil
				}
				return m.DeviceLayers
			}(),
			// R176 (2026-10-19): раскладка НЕ применилась, хотя узлы передали.
			// Поле показывает ФАКТ сверки «переданные серверы vs фактическая
			// раскладка слоёв»: без него тихое выпадение узла на загрузке
			// выглядело как обычная успешная загрузка (R174: 42 слоя на одной
			// карте вместо 21+17 с донором).
			"layout_degraded": m.LayoutDegraded,
			"layout_degraded_reason": func() string {
				if !m.LayoutDegraded {
					return ""
				}
				return m.LayoutDegradedReason
			}(),
		}
		// R83-политика (2026-10-01): режим reasoning ЗАГРУЖЕННОЙ модели.
		//
		// Зачем в ответе. Балансер обязан уметь сказать клиенту «модель уже
		// загружена с другими параметрами» вместо тихой подмены поведения:
		// если клиент требует think=true, а модель загружена с выключенным
		// reasoning, это разные ответы на один и тот же запрос. Поле
		// читается поллером балансера (llamacpp_metrics_poller.go) и
		// прокидывается в NCtxBackendState.CurrentReasoningEnabled.
		//
		// false здесь — ЗНАЧЕНИЕ, а не «неизвестно»: cppworker всегда его
		// отдаёт для загруженной модели, поэтому балансер отличает «выключено»
		// от «метрик ещё нет» (ключ отсутствует).
		if m.Capabilities != nil {
			entry["reasoning_enabled"] = m.Capabilities.Reasoning
		} else {
			entry["reasoning_enabled"] = false
		}
		// Round 51.6 (2026-08-20): per-model active_queries counter. WebUI
		// показывает "Generating N requests" badge чтобы оператор не выгружал
		// модель посреди inference (R51.6: handleUnloadModel теперь возвращает
		// 409 если active_queries > 0, см. handleUnloadModel).
		if inflight := backend.InFlight(); inflight != nil {
			entry["active_queries"] = inflight.Get(m.Name)
			// R303: возраст самого долгого активного запроса по модели. Нужен,
			// чтобы «мусор в in_flat» был виден как возраст, а не как вечный 1
			// (см. load_claims.go / inflight.go).
			if ages := inflight.AgesR303(); len(ages) > 0 {
				if age, ok := ages[m.Name]; ok {
					entry["active_query_age_sec"] = age
				}
			}
		}
		// R303: заявка на загрузку/перезагрузку — возраст и владелец. Пока она
		// жива, unload отвечает 409, а load-запросы ждут; оператор должен видеть,
		// СКОЛЬКО она висит, а не только «model is loading».
		if claim, ok := backend.LoadClaimR303(m.Name); ok {
			entry["load_claim_age_sec"] = claim.AgeSec
			entry["load_claim_requester"] = claim.Requester
			entry["load_claim_stale"] = !claim.LoadingInstance && claim.AgeSec >= loadStaleAfterR303().Seconds()
		}
		// R83 (2026-09-27): показываем РЕЖИМ загрузки, а не только факт загрузки.
		// context_size — фактический n_ctx, с которым работает модель;
		// max_vram_n_ctx — граница «всё в VRAM»; max_exact_fit_n_ctx — потолок
		// n_ctx для загрузки ЦЕЛИКОМ в VRAM (по нему считается n_ctx_degraded).
		// Раньше degraded считался от max_vram_n_ctx, и на 27B (веса не влезают
		// даже одним слоем → 0) он был true при любом контексте: оператор видел
		// «деградировано» и не понимал, во что именно упёрлось.
		vramMax, feasibleMax, ggufMax, exactFitMax := 0, 0, m.GGUFContextLength, 0
		vramKnown := false
		unreservedMax, reserveMB := 0, 0
		if fa, ok := perModelFeasible[m.Name]; ok {
			feasibleMax = fa.feasibleMax
			ggufMax = fa.ggufMax
			vramMax = fa.vramMax
			vramKnown = fa.vramKnown
			exactFitMax = fa.exactFitMax
			unreservedMax = fa.unreservedMax
			reserveMB = fa.reserveMB
		}
		for k, v := range nctxModeFields(vramMax, ggufMax, feasibleMax, exactFitMax, m.ContextSize, vramKnown) {
			entry[k] = v
		}
		// R592: справочная «тень» резерва VRAM (решения не меняет).
		for k, v := range nctxShadowFieldsR592(unreservedMax, reserveMB, m.ContextSize, vramKnown) {
			entry[k] = v
		}
		enrichedModels = append(enrichedModels, entry)
	}

	// Round 37 (2026-08-18): top-level feasible/GGUF — min across loaded models.
	topFeasible := 0
	topGGUF := 0
	for _, fa := range perModelFeasible {
		if topFeasible == 0 || (fa.feasibleMax > 0 && fa.feasibleMax < topFeasible) {
			topFeasible = fa.feasibleMax
		}
		if fa.ggufMax > topGGUF {
			topGGUF = fa.ggufMax
		}
	}

	resp := map[string]interface{}{
		"models":         enrichedModels,
		"count":          len(models),
		"max_vram_n_ctx": maxVRAMNCtx,
		"max_ram_n_ctx":  maxRAMNCtx,
		// R592: «тень» резерва VRAM. max_vram_n_ctx выше — то, что применяется
		// (с резервом CPPWORKER_VRAM_OVERHEAD_MB); здесь — потолок без резерва,
		// то есть как его считает мост, и размер самого резерва.
		"max_vram_n_ctx_unreserved": maxVRAMNCtxUnreserved,
		"vram_reserve_mb":           vramReserveMB,
		"available_vram_mb":         availableVRAMMB,
		"total_vram_mb":             totalVRAMMB,
		"available_ram_mb":          availableRAMMB,
		"total_ram_mb":              totalRAMMB,
		"model_max_context":         modelMaxContext,
		"gpu_count":                 backend.GetGPUCount(),
		// Round 37 (2026-08-18): auto-adapt fields.
		// feasible_max_context = min(VRAM, RAM, GGUF) — recommended max n_ctx.
		// gguf_max_context     = max GGUF training context — hard upper bound.
		"feasible_max_context": topFeasible,
		"gguf_max_context":     topGGUF,
		// R83 шаг 3: max_vram_n_ctx = 0 означает ЛИБО «данных о VRAM нет»
		// (vram_known=false), ЛИБО «веса модели не влезают в VRAM» (vram_known=true,
		// загрузка пойдёт в partial_offload/cpu_only). Раньше эти случаи были
		// неразличимы, и UI не мог предупредить о режиме загрузки.
		"vram_known": vramKnownAgg,
	}

	// R115: раскладка памяти по источникам. available_vram_mb выше — ИТОГ
	// (memfit считает бюджет как локальная карта + узлы раскладки), и по одному
	// числу нельзя понять, откуда гигабайты: «8 ГБ» и «8 ГБ + 7 ГБ донора»
	// выглядят одинаково, хотя это разные решения о загрузке. Смысл прежних полей
	// не меняется — балансер читает их как раньше.
	//
	// R132 (2026-10-16): раскладка берётся БЕЗ дозвона до узлов (LayoutBreakdownCached).
	// После R131 здесь оставался `LayoutBreakdown()` — то есть чтение списка моделей
	// всё ещё звонило на узел раскладки и на узле «принял и молчит» уходило в C-мост.
	// Источник вклада узлов отдаётся полем vram_remote_source: none (не измерялись
	// на этом пути) / cache (из уже собранного снимка) / probed (узлы спрошены).
	layout := backend.LayoutBreakdownCached()
	resp["rpc_enabled"] = layout.RPCEnabled
	resp["rpc_servers_active"] = layout.Servers
	// R229 (2026-10-22): узлы раскладки, ИСКЛЮЧЁННЫЕ как недоступные на время
	// работы с моделью (TCP-проба). Оператор видит, почему модель работает без
	// донора, вместо догадок по логу.
	resp["rpc_excluded"] = backend.ExcludedRPCServers()
	resp["vram_local_free_mb"] = layout.LocalFree >> 20
	resp["vram_local_total_mb"] = layout.LocalTotal >> 20
	resp["vram_remote_free_mb"] = layout.RemoteFree >> 20
	resp["vram_remote_total_mb"] = layout.RemoteTotal >> 20
	resp["vram_layout_free_mb"] = layout.LayoutFree >> 20
	resp["vram_layout_total_mb"] = layout.LayoutTotal >> 20
	resp["vram_remote_nodes"] = layout.Nodes
	resp["vram_remote_source"] = layout.RemoteSource
	// R348 (2026-10-31): ПОСЛЕДНИЙ измеренный вклад узлов — отдельно от текущего.
	//
	// ЗАЧЕМ. Через rpcDeviceTTL (30 с) снимок узлов протухает, и справочный путь
	// честно отдаёт vram_remote_* = 0 (R132: протухшее не выдаём за текущую
	// ёмкость). Но тогда «донор 7 ГБ, измеренный 10 минут назад» и «узлов в
	// раскладке нет» выглядят одинаково, и вопрос «влезет ли модель в два узла»
	// остаётся без ответа, хотя замер был.
	//
	// Эти поля — ТОЛЬКО для показа: в решениях о загрузке они не участвуют,
	// поэтому отдельные ключи, а не подмена vram_remote_*.
	resp["vram_remote_last_known_free_mb"] = layout.RemoteLastKnownFree >> 20
	resp["vram_remote_last_known_total_mb"] = layout.RemoteLastKnownTotal >> 20
	resp["vram_remote_last_known_nodes"] = layout.RemoteLastKnownNodes
	resp["vram_remote_last_known_age_sec"] = int64(layout.RemoteLastKnownAge.Seconds())

	// R133 (2026-10-16): состояние СОБСТВЕННОГО узла раскладки этой машины.
	//
	// ЗАЧЕМ ЗДЕСЬ. Донор обязан отчитываться о своём узле тем же ответом, который
	// балансер и так опрашивает (поллер /api/models), иначе «поднят ли узел, на
	// каком порту, что за ошибка» видно только в /api/v1/cppworker/config, куда
	// балансер не ходит. Без этого тумблер роли в панели выглядел бы как
	// «нажал — и ничего не произошло».
	if rpcServerChild != nil {
		resp["rpc_server"] = rpcServerChild.Status()
	}
	// R215 (2026-10-21): состояние РАСКЛАДКИ со стороны мастера.
	//
	// rpc_server выше описывает СВОЙ узел (мастер тоже может быть донором).
	// Здесь — про узлы, к которым подключаемся мы: смог ли гейт получить от них
	// устройства и сколько неудач подряд. stuck_suspected=true означает «узел
	// доступен по TCP, но команды не обслуживает несколько загрузок подряд»:
	// это зависший процесс, keepalive его не лечит, нужен перезапуск rpc-server
	// на доноре (docs/operations/rpc-donor-vram-lifecycle.ru.md).
	resp["rpc_layout"] = map[string]interface{}{
		"ready_timeout_streak": backend.RPCReadyTimeoutStreak(),
		"stuck_suspected":      backend.RPCLayoutStuckSuspected(),
	}

	// R304 (2026-10-30): пометка ДОНОРА — тем же ответом, который поллер и так
	// опрашивает.
	//
	// ЗАЧЕМ ЗДЕСЬ. Панель показывала «узел раскладки поднят» и по этому признаку
	// ДОГАДЫВАЛАСЬ о роли, блокируя (или не блокируя) кнопки загрузки. Догадка
	// расходится с решением воркера ровно в тех случаях, из-за которых появилась
	// R304: узел включён руками, роль объявлена без узла, роль снята у машины с
	// уже загруженной копией. Теперь панель читает ФАКТ решения (role, donor,
	// can_load_own_models, donor_reason) и видит список уже загруженных вопреки
	// роли копий (own_copies) — их придётся выгрузить, запрет их не убирает.
	donorMark := backend.DonorMark()
	ownCopies := donorMark.OwnCopies
	if ownCopies == nil {
		// Пустой срез, а не null: панель и агент читают поле как список, и
		// отсутствие значения не должно требовать отдельной ветки.
		ownCopies = []string{}
	}
	resp["role"] = donorMark.Role
	resp["donor"] = donorMark.Donor
	resp["can_load_own_models"] = donorMark.CanHostOwnModels
	resp["donor_reason"] = donorMark.Reason
	resp["own_copies"] = ownCopies

	// R83 (2026-09-25): причина последнего провала загрузки — машиночитаемо.
	//
	// Это ключевое звено связки cppworker → agent → webui: agent и так опрашивает
	// /api/models каждые 10 с, поэтому новых HTTP-запросов не появляется вообще,
	// а причина провала (код + числа) доезжает до WebUI без участия балансера в
	// сборе данных. Раньше наружу отдавался только свободный текст в
	// /api/models/load/progress, и уведомление оператору построить было нельзя.
	if fe, ok := loadFailures.latest(); ok {
		resp["load_failure"] = map[string]interface{}{
			"model":       fe.Model,
			"reason":      fe.Reason,
			"error":       fe.Err,
			"at":          fe.At.UTC().Format(time.RFC3339),
			"severity":    loadFailureSeverity(fe.Reason),
			"diagnostics": fe.Diagnostics,
		}
	}

	// R83 §9.4 шаг 1б, вариант D (2026-09-26): модель загружена, но без GPU.
	//
	// Это НЕ load_failure: загрузка удалась, модель работает. Отдаём отдельным
	// полем, потому что балансер по load_failure публикует «не хватило памяти»
	// и позже «загрузка восстановлена» — для cpu_only и то и другое ложь.
	// Agent забирает поле тем же циклом опроса (см. internal/agent/collector.go)
	// и передаёт балансеру, который публикует уведомление с severity=warning.
	if d, ok := degradedLoads.latest(); ok && !d.IsEmpty() {
		resp["load_degraded"] = map[string]interface{}{
			"model":       d.Model,
			"stage":       d.Stage,
			"reason":      d.Reason,
			"detail":      d.Detail,
			"at":          d.At.UTC().Format(time.RFC3339),
			"severity":    "warning",
			"diagnostics": d.Numbers,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
