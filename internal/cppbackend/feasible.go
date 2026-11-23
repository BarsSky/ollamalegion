// internal/cppbackend/feasible.go — Round 37 (2026-08-18): Feasible n_ctx calculation.
//
// SINGLE SOURCE OF TRUTH for "what n_ctx is achievable on this hardware".
//
// Используется:
//   - /api/models endpoint (Task 2: expose to balancer)
//   - -feasible CLI (Task 3: operator inspection)
//   - -auto-load CLI (Task 4: auto-detect + load)
//   - background sync (Task 5: warn when profile is conservative)
//
// 4 источника, в порядке убывания авторитетности (см. balancer/preflight_nctx.go):
//  1. user request (manual override)        — out of scope here
//  2. profile (operator policy)             — caller responsibility
//  3. feasible (this function)              — что hardware реально может
//  4. GGUF metadata (training context)      — hard upper bound
//
// ПРЕДОТВРАЩАЕТ класс багов:
//   - Profile conservative (32K) vs hardware allows 64K → 413 preflight crash
//   - Profile value 32768 vs _note "64K fits" → silently truncated
//   - GGUF max 262K не используется → operator не знает, что можно больше
package cppbackend

import (
	"fmt"

	"ollama-loadbalancer/c/bridge"
)

// FeasibleInfo описывает, какие n_ctx значения достижимы на текущем hardware.
//
// Все поля в ТОКЕНАХ (не байтах). Zero = "unknown / not computed"
// (caller должен fallback на GGUF metadata или fail safe).
type FeasibleInfo struct {
	GGUFMax     int    `json:"gguf_max"`      // из GGUF metadata (training context)
	MaxVRAMCtx  int    `json:"max_vram_ctx"`  // free VRAM / kv_per_token
	MaxRAMCtx   int    `json:"max_ram_ctx"`   // free RAM / kv_per_token
	KVPerToken  int    `json:"kv_per_token"`  // bytes per token для KV cache
	KVCacheType string `json:"kv_cache_type"` // "f16" (default) / "q8_0" / "q4_0"
	FreeVRAMMB  uint64 `json:"free_vram_mb"`  // observed free VRAM at calc time
	FreeRAMMB   uint64 `json:"free_ram_mb"`   // observed free RAM at calc time
	Source      string `json:"source"`        // "gguf" / "loaded_info" / "fallback"

	// R705 (2026-11-24): ширина головы K/V, по которой посчитан KVPerToken, и
	// ИСТОЧНИК этой ширины (gguf — <arch>.attention.key_length из модели,
	// computed — прежняя формула n_embd/n_heads). Публикуется, чтобы в отчёте и
	// в `-feasible` было видно, на чём стоит «сколько влезет», а не только итог.
	HeadDimK      int    `json:"head_dim_k"`
	HeadDimV      int    `json:"head_dim_v"`
	HeadDimSource string `json:"head_dim_source"`
}

// ComputeFeasible возвращает FeasibleInfo для модели. Порядок предпочтения:
//
//  1. Уже загруженная модель — берём GGUFContextLength + KVCacheType из ModelInfo
//     (НЕ перечитываем GGUF файл — это дорого для больших моделей).
//  2. ModelManager знает путь к файлу — read GGUF header (быстро, ~10ms).
//  3. Error (caller решает fallback — обычно "use profile as-is").
//
// kvCacheType берётся из (в порядке приоритета):
//
//	a) Per-model profile override (если synced из balancer через profileSyncer)
//	b) Backend.cfg.DefaultKVCacheType (config.bundled.json global default)
//	c) "f16" (safe upper bound — больше KV memory, но гарантированно работает)
//
// Round 37 (2026-08-18): auto-adapt n_ctx для балансера (3-tier resolution).
// Раньше был только profile.contextLength — если оператор поставил консервативное
// значение (32768) для модели с GGUF max 262144, balancer не знал, что можно 64K.
// Теперь feasibleMax = min(GGUFMax, MaxVRAMCtx, MaxRAMCtx) — авто-рекомендация.
func (b *Backend) ComputeFeasible(modelName string) (*FeasibleInfo, error) {
	if b == nil {
		return nil, fmt.Errorf("backend not initialized")
	}

	h, kvType, source, err := b.resolveGGUFAndKVType(modelName)
	if err != nil {
		return nil, err
	}

	// Round 37 (2026-08-18): HeadDimK estimation для pre-load пути.
	// C-bridge парсит head_dim только ПОСЛЕ load. Для предварительной
	// рекомендации (профиль не загружен, файла на диске достаточно) оцениваем:
	//   head_dim = embedding_length / head_count
	// Это верно для всех standard transformer архитектур (Llama, Qwen, Mistral).
	// Примеры:
	//   Qwen3.6-35B-A3B:  2048 / 16 = 128 ✓
	//   Llama-3-8B:        4096 / 32 = 128 ✓
	//   Mistral-7B:        4096 / 32 = 128 ✓
	//
	// R704 (2026-11-24) — ОЦЕНКА ЗАМЕНЕНА МЕТАДАННЫМИ.
	//
	// ЖИВОЙ ФАКТ. В заголовке GGUF есть ТОЧНЫЙ размер головы K/V —
	// <arch>.attention.key_length / value_length (llama.cpp читает именно их,
	// src/llama-hparams.cpp). Формула n_embd/n_heads с ними не совпадает, и KV
	// занижался, а рекомендованное окно — завышалось:
	//   Inkling-2-Mini: 2304/24 = 96 при key_length = 128 → KV на 25 % меньше
	//                   (36 864 против 49 152 Б/токен при q4_0);
	//   gemma-4-E4B:    2560/8  = 320 при key_length = 512 → KV на 37.5 %
	//                   меньше (проверено по тензорам: blk.4.attn_q = 8×512);
	//   Qwen3-4B:       2560/32 = 80 при key_length = 128 → KV на 60 % меньше
	//                   (blk.0.attn_q = [2560, 4096] = 32×128).
	//
	// Поля приходят из того же парсера, где уже читаются head_count/head_count_kv
	// (backend_gguf.go: ggufFieldSuffixes/ggufSetField, поля KeyLength/ValueLength;
	// загрузчик пути "loaded_info" кладёт туда измеренные C-мостом HeadDimK/V).
	//
	// K и V могут быть РАЗНОЙ ширины (gemma-4: глобальные слои 512, SWA — 256;
	// <arch>.attention.key_length_swa), поэтому байты на токен считаются как
	// (keyLength + valueLength), а не как «2 × одна ширина». Если value_length нет —
	// V считается равной K, как и раньше.
	//
	// Fallback chain сохранён: KeyLength > HeadDimK (C-мост) > NEmbd/NHeads > 128.
	//
	// R705 (2026-11-24): вместе с шириной запоминается её ИСТОЧНИК. Резолвер
	// (resolveGGUFAndKVType) уже знает его для пути loaded_info («мост сказал:
	// key_length или формула») и для пути GGUF-заголовка. Если источник не задан,
	// выводим его из того, какое поле дало число: KeyLength/HeadDimK из модели —
	// факт, NEmbd/NHeads — прежняя формула, дефолт 128 — «неизвестно» (пусто).
	headDimSource := h.HeadDimSource
	if headDimSource == "" {
		switch {
		case h.KeyLength > 0:
			headDimSource = bridge.HeadDimSourceGGUF
		case h.HeadDimK > 0, h.NEmbd > 0 && h.NHeads > 0:
			headDimSource = bridge.HeadDimSourceComputed
		}
	}

	headDimK := h.KeyLength
	if headDimK <= 0 {
		headDimK = h.HeadDimK
	}
	if headDimK <= 0 && h.NEmbd > 0 && h.NHeads > 0 {
		headDimK = h.NEmbd / h.NHeads
	}
	if headDimK <= 0 {
		headDimK = 128 // safe upper bound (over-estimate → recommend less ctx, but safe)
	}
	headDimV := h.ValueLength
	if headDimV <= 0 {
		headDimV = headDimK
	}
	if headDimV <= 0 {
		headDimV = 128
	}

	// KV per token: (K + V) * NLayers * NKvHeads * 2 bytes (fp16 baseline)
	//   q4_0: / 4   (4-bit vs 16-bit)
	//   q8_0: / 2   (8-bit vs 16-bit)
	//   f16:  * 1   (baseline)
	//
	// При key_length == value_length == headDimK это ровно прежняя формула
	// 2 * NLayers * NKvHeads * headDimK * 2 — для моделей, где метаданные
	// совпадают с оценкой (Qwen3.8-9B: 4096/16 = 256 = key_length), числа не
	// меняются (тест «для модели с совпадающим head_dim числа не изменились»).
	kvPerToken := (headDimK + headDimV) * h.NLayers * h.NKvHeads * 2
	switch kvType {
	case "q4_0":
		kvPerToken = kvPerToken / 4
	case "q8_0":
		kvPerToken = kvPerToken / 2
	}
	// Defensive: don't divide by zero
	if kvPerToken <= 0 {
		kvPerToken = 16384 // typical q4_0 для 7B модели (fallback)
	}

	freeVRAMMB, freeRAMMB := b.getFreeMemoryMB()

	// R521 (§8.3.1): потолок окна считается ПО ПАМЯТИ, а не «половина свободной
	// VRAM».
	//
	// ЖИВОЙ ФАКТ (архив 2026-11-18, A10 24 ГБ, 27B Q4_K_M, kv q8_0): здесь
	// стояло `safeVRAMMB := freeVRAMMB / 2` и `maxVRAMCtx = safeVRAMMB/kvPerToken`
	// — без вычета весов модели и compute buffer. Расчёт разрешил окно 131072,
	// после загрузки осталось 2508 МБ, и первый же llama_decode на 80k-промпте
	// упал с CUDA OOM («code 1» без причины). Половина свободной VRAM — не
	// резерв на буферы, а просто доля от того, что ещё не занято.
	weightsMB := b.weightsSizeMB(modelName)
	reserveMB := computeBufferReserveMB(weightsMB)
	maxVRAMCtx := maxVRAMCtxFromMemory(freeVRAMMB, weightsMB, reserveMB, kvPerToken, h.ContextLength)

	maxRAMCtx := 0
	if freeRAMMB > 0 && kvPerToken > 0 {
		maxRAMCtx = int(freeRAMMB*1024*1024) / kvPerToken
		if maxRAMCtx > h.ContextLength {
			maxRAMCtx = h.ContextLength
		}
	}

	return &FeasibleInfo{
		GGUFMax:     h.ContextLength,
		MaxVRAMCtx:  maxVRAMCtx,
		MaxRAMCtx:   maxRAMCtx,
		KVPerToken:  kvPerToken,
		KVCacheType: kvType,
		FreeVRAMMB:  freeVRAMMB,
		FreeRAMMB:   freeRAMMB,
		Source:      source,
		// R705: видно, какая ширина головы дала KVPerToken и откуда она.
		HeadDimK:      headDimK,
		HeadDimV:      headDimV,
		HeadDimSource: headDimSource,
	}, nil
}

// resolveGGUFAndKVType возвращает GGUF header + kv cache type + source.
// Используется ComputeFeasible.
//
// 3 пути (в порядке предпочтения):
//  1. Загруженная модель (Backend.GetModel) — GGUFContextLength + KVCacheType из ModelInfo
//  2. ModelManager + read GGUF header (для не-загруженных моделей)
//  3. Error: модель не найдена нигде
func (b *Backend) resolveGGUFAndKVType(modelName string) (*GGUFHeaderInfo, string, string, error) {
	// Path 1: loaded model — данные уже в памяти (no GGUF re-read)
	if mi, err := b.GetModel(modelName); err == nil && mi != nil {
		h := &GGUFHeaderInfo{
			Architecture: mi.Architecture,
			NLayers:      mi.NLayers,
			NHeads:       mi.NHeads,
			NKvHeads:     mi.NKvHeads,
			// R704: у загруженной модели ширина головы K/V ИЗМЕРЕНА C-мостом
			// (HeadDimK/HeadDimV после llama.cpp); это те же поля, что парсер
			// заполняет из <arch>.attention.key_length/value_length, поэтому
			// кладём их в KeyLength/ValueLength — ComputeFeasible читает один
			// набор полей на всех путях (см. комментарий про head_dim там).
			KeyLength:     mi.HeadDimK,
			ValueLength:   mi.HeadDimV,
			HeadDimK:      mi.HeadDimK,
			HeadDimV:      mi.HeadDimV,
			NEmbd:         mi.NEmbd,
			ContextLength: mi.GGUFContextLength, // Round 37: 0 если не проставлено
			// R705: источник ширины — тот, что сообщил мост при загрузке
			// (gguf = <arch>.attention.key_length из модели, computed = прежняя
			// формула). Пусто → ComputeFeasible выведет источник сам.
			HeadDimSource: resolveHeadDimSourceR705(mi.HeadDimSource, mi.HeadDimK),
		}
		// KV type: prefer per-model override, fall back to backend default
		kvType := mi.KVCacheType
		if kvType == "" {
			kvType = b.cfg.DefaultKVCacheType
		}
		if kvType == "" {
			kvType = "f16"
		}
		if h.ContextLength > 0 {
			return h, kvType, "loaded_info", nil
		}
		// Model loaded but ContextLength unknown — fall through to ModelManager
		// (header re-read OK here, мы уже знаем что файл существует)
	}

	// Path 2: ModelManager + read GGUF header
	mm := b.ModelManager()
	if mm == nil {
		return nil, "", "", fmt.Errorf("model %q: backend has no ModelManager and model is not loaded", modelName)
	}
	meta, err := mm.GetModelMeta(modelName)
	if err != nil {
		// Try by filename (caller may pass "model.gguf" or "model")
		meta, err = mm.GetModelMeta(modelName + ".gguf")
		if err != nil {
			return nil, "", "", fmt.Errorf("model %q not found in ModelManager: %w", modelName, err)
		}
	}
	if meta.Path == "" {
		return nil, "", "", fmt.Errorf("model %q: no path in ModelManager", modelName)
	}

	h, err := ReadGGUFHeader(meta.Path)
	if err != nil {
		return nil, "", "", fmt.Errorf("read GGUF header for %q: %w", modelName, err)
	}
	// R705: источник ширины для пути «заголовок файла»: ключ
	// <arch>.attention.key_length есть → gguf, иначе прежняя формула.
	h.HeadDimSource = headDimSourceForKeyLengthR705(h.KeyLength, h.KVHeadDim())

	// KV type: prefer per-model profile override, fall back to default
	kvType := meta.KVCacheType
	if kvType == "" {
		kvType = b.cfg.DefaultKVCacheType
	}
	if kvType == "" {
		kvType = "f16"
	}

	return h, kvType, "gguf", nil
}

// getFreeMemoryMB возвращает free VRAM и free RAM в MB.
// VRAM берётся из gpuDevices[0] (если есть). RAM — из системы.
func (b *Backend) getFreeMemoryMB() (uint64, uint64) {
	var freeVRAM uint64
	if len(b.gpuDevices) > 0 {
		// Берём первый GPU (cppworker convention: 1 GPU на 1 backend instance).
		// Multi-GPU: каждый instance видит свой GPU.
		freeVRAM = uint64(b.gpuDevices[0].VRAMFreeMB)
	} else if dev, err := bridge.GetGPUInfo(0); err == nil && dev != nil {
		// Fallback: спросить C-bridge напрямую (если gpuDevices не инициализирован,
		// например в CLI mode до полного init)
		freeVRAM = uint64(dev.VRAMFreeMB)
	}
	freeRAM := GetSystemRAMGB() * 1024
	// Approximate available RAM = total - 4GB system reserve (cppworker convention)
	if freeRAM > 4*1024 {
		freeRAM -= 4 * 1024
	}
	return freeVRAM, freeRAM
}
