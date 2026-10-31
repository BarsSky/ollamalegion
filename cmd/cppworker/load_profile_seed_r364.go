// load_profile_seed_r364.go — R364 (2026-10-31): профиль модели на legacy
// POST /api/models/load.
//
// ЖИВОЙ ДЕФЕКТ (стенд, первый запуск после пересборки контейнеров).
// Прямая загрузка `POST /api/models/load {"name":"gemma-4-E4B-it-Q4_K_M"}`
// подняла модель МИМО профиля оператора:
//
//	факт (API)            профиль модели
//	context_size=65536    32768
//	kv_cache_type=q8_0    q4_0
//	reasoning_enabled=f   true
//
// Следствие не «косметическое»: KV вырос почти вдвое, VRAM перестало хватать,
// memfit оставил на CPU часть слоёв, и модель выдавала 2.9 tok/s против 25.9 у
// инстанса, поднятого балансером (замер: 116 токенов, тот же профиль).
//
// ПРИЧИНА. Профиль применяли handleLoadWithParams (R26) и handleReloadModel, а
// ленивая загрузка — через applySyncedProfileToLazyLoadOpts (R83/v52). В
// комментарии к последней написано, что handleLoadModel профиль применяет, — но
// он его не применял никогда: в теле обработчика не было ни profileSyncer, ни
// applyProfileToLoadRequest. Комментарий описывал намерение, а не код, поэтому
// расхождение и прожило до этого замера.
//
// ПОЧЕМУ НЕЛЬЗЯ ПРОСТО ВЫЗВАТЬ applyProfileToLoadRequest. Он заполняет только
// ПУСТЫЕ поля (0 / ""), а legacy-обработчик к этому моменту уже подставил
// конфиг/env-дефолты (ContextSize = CPPWORKER_CTX_SIZE и т.п.). Для профиля
// «пусто» неотличимо от «оператор задал в конфиге», и профиль молча проигрывал
// бы дефолту — та же ошибка, только на другом слое.
//
// Поэтому здесь приоритет выражен явно и в одном месте:
//
//	явное поле запроса  >  профиль модели  >  конфиг/env воркера
//
// Это ровно контракт балансера (internal/balancer/model_management.go,
// applyProfileLoadParams: «явные поля запроса > профиль > дефолт cppworker»,
// R83/v62: профиль модели применяется ВСЕГДА, ignoreDefaults гасит только
// применение дефолтного профиля). Воркер обязан вести себя так же: иначе
// результат загрузки зависит от того, кто её попросил — балансер или оператор
// напрямую.
package main

import "ollama-loadbalancer/pkg/types"

// loadProfileSeedR364 — значения, которые профиль модели даёт загрузке.
//
// Нулевые значения означают «профиль это поле не задаёт» (семантика
// applyProfileToLoadRequest: 0 / "" = наследовать). Исключение — GPULayers:
// там 0 у профиля тоже значит «наследовать», а «все слои на GPU» это -1,
// а «авто» это -2, поэтому любое ненулевое значение — содержательное.
type loadProfileSeedR364 struct {
	ContextSize int
	BatchSize   int
	GPULayers   int
	// FlashAttnType — nil = профиль молчит; иначе -1 (auto) / 0 (выключено) /
	// 1 (включено). Указатель, а не int: 0 здесь содержательное значение, и
	// отдельный флаг «задано» иначе пришлось бы носить рядом.
	FlashAttnType *int
	KVCacheType   string
	Parallel      int
	// Reasoning — три состояния: nil (профиль молчит), &true / &false.
	Reasoning *bool
}

// loadDefaultsR364 — значения из конфига/env воркера, то есть ПОСЛЕДНИЙ слой
// приоритета. Вынесено в структуру, чтобы разрешение параметров было чистой
// функцией и его можно было проверить без запуска загрузки.
type loadDefaultsR364 struct {
	GPULayers     int
	ContextSize   int
	BatchSize     int
	FlashAttnType int
}

// loadParamsR364 — итог разрешения по полям, в которых участвует профиль.
type loadParamsR364 struct {
	GPULayers     int
	ContextSize   int
	BatchSize     int
	FlashAttnType int
	Parallel      int
	KVCacheType   string
	// KVCacheRaw — что просили (запрос или профиль), если тип оказался
	// неподдержанным: нужно для предупреждения с конкретным значением.
	KVCacheRaw string
	// KVCacheInvalid — тип KV не поддержан; caller предупреждает и грузит с
	// дефолтом llama.cpp (прежнее поведение legacy-пути).
	KVCacheInvalid bool
}

// profileForLoadR364 — найденный профиль модели вместе с ключом, по которому он
// нашёлся (ключ нужен логу: при загрузке под алиасом он отличается от имени
// запроса, см. R154).
type profileForLoadR364 struct {
	Key     string
	Profile *types.LlamaCppModelProfile
	Seed    loadProfileSeedR364
}

// readProfileForLoadR364 — профиль модели для этой загрузки. nil = профиля нет
// (или syncer не поднят: тогда поведение остаётся прежним, на дефолтах).
//
// Профиль ищется у ВЛАДЕЛЬЦА ФАЙЛА, а не у имени запроса (R154): загрузка под
// коротким алиасом (`gemma-4`) поднимает тот же файл, и профиль алиаса молча
// подменял бы параметры оператора.
func readProfileForLoadR364(modelName, modelPath string) *profileForLoadR364 {
	if profileSyncer == nil {
		return nil
	}
	key := profileLookupNameForPath(modelName, modelPath)
	prof := profileSyncer.applyProfileOnLoad(key)
	if prof == nil {
		return nil
	}
	return &profileForLoadR364{
		Key:     key,
		Profile: prof,
		Seed: loadProfileSeedR364{
			ContextSize:   prof.ContextLength,
			BatchSize:     prof.BatchSize,
			GPULayers:     prof.NumGPULayers,
			FlashAttnType: profileFlashAttnToIntR364(prof.FlashAttn),
			KVCacheType:   prof.KVCacheType,
			Parallel:      prof.Parallel,
			Reasoning:     prof.EnableReasoning,
		},
	}
}

// profileFlashAttnToIntR364 — bool из профиля в то, что понимает cppbackend.
//
// Семантика поля на стороне моста (c/bridge/bridge.go:176):
//
//	-1 = auto, 0 = disabled, 1 = enabled
//
// поэтому profileFlashAttn(false) — это 0 (ВЫКЛЮЧЕНО), а не -1 (auto). Здесь
// намеренно НЕ повторяется конверсия applyProfileToLoadRequest, которая отдаёт
// false → -1: «выключить flash attention» в авто-режиме может быть включено
// llama.cpp заново. У этой конверсии в общем помощнике есть и вторая проблема
// (её легко увидеть): он заполняет только пустые поля, а «0 = выключено» для
// него неотличимо от «не задано», поэтому профильный flashAttn=false там не
// применяется вовсе. Это отдельный дефект старых путей
// (load-with-params/reload/lazy), записанный как R365 — R364 его не трогает,
// чтобы не менять поведение трёх путей этим же коммитом.
func profileFlashAttnToIntR364(p *bool) *int {
	if p == nil {
		return nil
	}
	v := 0
	if *p {
		v = 1
	}
	return &v
}

// resolveLoadIntR364 — приоритет «явное значение клиента > профиль > дефолт».
func resolveLoadIntR364(explicit *int, profileVal, def int) int {
	if explicit != nil {
		return *explicit
	}
	if profileVal != 0 {
		return profileVal
	}
	return def
}

// resolveLoadIntPtrR364 — то же для полей, где 0 — содержательное значение
// (flash attention: 0 = выключено). nil у профиля = «профиль молчит».
func resolveLoadIntPtrR364(explicit, profileVal *int, def int) int {
	if explicit != nil {
		return *explicit
	}
	if profileVal != nil {
		return *profileVal
	}
	return def
}

// resolveLoadParamsR364 — собирает параметры загрузки по контракту приоритета.
func resolveLoadParamsR364(req loadModelRequest, seed loadProfileSeedR364, defs loadDefaultsR364) loadParamsR364 {
	out := loadParamsR364{
		GPULayers:     resolveLoadIntR364(req.GPULayers, seed.GPULayers, defs.GPULayers),
		ContextSize:   resolveLoadIntR364(req.ContextSize, seed.ContextSize, defs.ContextSize),
		BatchSize:     resolveLoadIntR364(req.BatchSize, seed.BatchSize, defs.BatchSize),
		FlashAttnType: resolveLoadIntPtrR364(req.FlashAttnType, seed.FlashAttnType, defs.FlashAttnType),
	}

	// Parallel: 0 и отрицательные значения по-прежнему означают «наследовать»
	// (CPPWORKER_N_PARALLEL) — так же, как вёл себя legacy-путь до R364, где
	// применялось только *req.Parallel > 0.
	if v := resolveLoadIntR364(req.Parallel, seed.Parallel, 0); v > 0 {
		out.Parallel = v
	}

	kv := ""
	switch {
	case req.KVCacheType != nil && *req.KVCacheType != "":
		kv = *req.KVCacheType
	case seed.KVCacheType != "":
		kv = seed.KVCacheType
	}
	if kv != "" && !isValidKVCacheType(kv) {
		// Неподдержанный тип НЕ подменяется профилем: клиент/профиль просили
		// конкретное, и подсунуть вместо него другое — значит соврать о
		// параметрах загрузки. Грузим на дефолте llama.cpp и говорим вслух.
		out.KVCacheInvalid = true
		out.KVCacheRaw = kv
		kv = ""
	}
	out.KVCacheType = kv
	return out
}
