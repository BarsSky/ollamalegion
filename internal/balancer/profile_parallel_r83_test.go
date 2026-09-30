// profile_parallel_r83_test.go — R83 (2026-09-29).
//
// ДЕФЕКТ. Поле `parallel` в профиле модели (WebUI → «Параллельность») доезжало
// до cppworker ТОЛЬКО через POST /api/v1/cppworker/model-profiles/{name}/apply.
// При обычной загрузке — auto-load по запросу /api/chat, POST /api/models/load,
// /api/models/load-with-params — профиль применялся к contextLength/batchSize/
// gpuLayers/kvCacheType, а `parallel` терялся: cppworker поднимал single-slot, и
// второй параллельный клиент получал 503 («модель не загрузить»), хотя оператор
// выставил 2 слота и сохранил профиль.
//
// Тест проверяет настоящее правило (applyProfileLoadParams), а не его копию.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_ProfileParallel_ReachesLoadRequest — parallel из профиля обязан
// попасть в запрос загрузки.
func TestR83_ProfileParallel_ReachesLoadRequest(t *testing.T) {
	prof := types.LlamaCppModelProfile{
		ContextLength: 8192,
		BatchSize:     512,
		Parallel:      2,
	}
	var req ModelOpRequest

	applyProfileLoadParams(&req, prof)

	if req.Parallel == nil {
		t.Fatal("parallel из профиля не доехал до запроса загрузки — cppworker " +
			"поднимет single-slot (max_slots=1), а второй параллельный клиент " +
			"упрется в SlotManager")
	}
	if *req.Parallel != 2 {
		t.Errorf("parallel = %d, want 2", *req.Parallel)
	}
}

// TestR83_ProfileParallel_ExplicitRequestWins — явный запрос (WebUI-форма
// загрузки/клиент) сильнее профиля: не понижаем и не повышаем то, что
// оператор задал в конкретной операции.
func TestR83_ProfileParallel_ExplicitRequestWins(t *testing.T) {
	in := 4
	req := ModelOpRequest{Parallel: &in}
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{Parallel: 2})

	if req.Parallel == nil || *req.Parallel != 4 {
		t.Errorf("parallel = %v, want 4 (явное значение запроса неприкосновенно)", req.Parallel)
	}
}

// TestR83_ProfileParallel_ZeroMeansInherit — 0 в профиле = «не задано»
// (наследовать окружение cppworker, CPPWORKER_N_PARALLEL), а не «выключить
// параллельность»: иначе сохранение профиля без поля сбрасывало бы настройку.
func TestR83_ProfileParallel_ZeroMeansInherit(t *testing.T) {
	var req ModelOpRequest
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{ContextLength: 8192, Parallel: 0})

	if req.Parallel != nil {
		t.Errorf("parallel = %d, want nil (0 = не задано)", *req.Parallel)
	}
}

// TestR83_ProfileParallel_RespectsIgnoreDefaults — с ignoreDefaults=true профиль
// не подставляет параметры загрузки вообще (см. profile_ignore_defaults_r83_test.go):
// параллельность задаётся окружением контейнера CPPWORKER_N_PARALLEL.
func TestR83_ProfileParallel_RespectsIgnoreDefaults(t *testing.T) {
	var req ModelOpRequest
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength:  32768,
		Parallel:       4,
		IgnoreDefaults: true,
	})

	if req.Parallel != nil {
		t.Errorf("ignoreDefaults=true, а профиль подставил parallel=%d", *req.Parallel)
	}
	if req.ContextSize != nil {
		t.Errorf("ignoreDefaults=true, а профиль подставил contextSize=%d", *req.ContextSize)
	}
}

// TestR83_DefaultProfile_FillsGapsAfterModelProfile — R83 (2026-09-30):
// цепочка, которую выполняет executeLlamaCppLoad для модели с профилем:
// сначала профиль модели, затем — «настройки по умолчанию для новых моделей»
// (config.defaultModelProfile). Профиль модели выигрывает по заданным полям,
// дефолт добивает остальные; модель без профиля получает весь дефолт.
func TestR83_DefaultProfile_FillsGapsAfterModelProfile(t *testing.T) {
	var req ModelOpRequest

	// 1) профиль модели: только contextLength и batchSize.
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength: 8192,
		BatchSize:     256,
	})
	// 2) дефолт из конфига: остальное.
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength: 16384,
		BatchSize:     512,
		NumGPULayers:  20,
		KVCacheType:   "q4_0",
		Parallel:      2,
	})

	if req.ContextSize == nil || *req.ContextSize != 8192 {
		t.Errorf("contextSize = %v, want 8192 (профиль модели выигрывает у дефолта)", req.ContextSize)
	}
	if req.BatchSize == nil || *req.BatchSize != 256 {
		t.Errorf("batchSize = %v, want 256 (профиль модели выигрывает у дефолта)", req.BatchSize)
	}
	if req.GPULayers == nil || *req.GPULayers != 20 {
		t.Errorf("gpuLayers = %v, want 20 (добирается из дефолта)", req.GPULayers)
	}
	if req.Parallel == nil || *req.Parallel != 2 {
		t.Errorf("parallel = %v, want 2 (добирается из дефолта)", req.Parallel)
	}
	if req.KVCacheType == nil || *req.KVCacheType != "q4_0" {
		t.Errorf("kvCacheType = %v, want q4_0 (добирается из дефолта)", req.KVCacheType)
	}
}

// TestR83_DefaultProfile_IgnoreDefaultsKeepsEnvWins — если оператор поставил
// ignoreDefaults на САМ дефолтный профиль, он не подставляет ничего: выигрывает
// окружение cppworker. Тот же принцип «одно место», что и у per-model профиля.
func TestR83_DefaultProfile_IgnoreDefaultsKeepsEnvWins(t *testing.T) {
	var req ModelOpRequest
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength:  32768,
		Parallel:       4,
		IgnoreDefaults: true,
	})

	if req.ContextSize != nil || req.Parallel != nil {
		t.Errorf("дефолт с ignoreDefaults=true подставил параметры: ctx=%v parallel=%v",
			req.ContextSize, req.Parallel)
	}
}
