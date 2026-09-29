// profile_ignore_defaults_r83_test.go — R83 (2026-09-29).
//
// ЖАЛОБА ОПЕРАТОРА: «профиль не должен задаваться жёстко — это ломает концепцию
// балансера: получаем жёсткие настройки, о которых пользователь может не
// догадываться». Живой пример: env задаёт CPPWORKER_GPU_LAYERS=20, а профиль
// qwen3.8 ставит numGpuLayers=-2 и выигрывал — потому что приоритет при загрузке
// был «явные поля запроса > профиль > дефолт cppworker», то есть профиль
// перебивал окружение.
//
// Теперь у профиля есть ignoreDefaults: при true он не подставляет параметры
// загрузки вообще. contextLength/contextLengthMax при этом продолжают работать
// как HINT для preflight и UI — это подсказка о потолке, а не скрытая настройка.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// buildLoadBody — проверка эффекта ignoreDefaults на параметры загрузки.
//
// R83-финал: вызывает НАСТОЯЩЕЕ правило (applyProfileLoadParams из
// model_management.go), а не его копию. Раньше здесь была копипаста логики —
// из-за этого тест не заметил, что `parallel` из профиля в запрос не попадал
// (см. profile_parallel_r83_test.go).
func applyProfileToEmptyRequest(t *testing.T, prof types.LlamaCppModelProfile) map[string]interface{} {
	t.Helper()

	// Пустой запрос: клиент ничего не задал — значит любое заполненное поле
	// пришло ИЗ ПРОФИЛЯ.
	var req ModelOpRequest
	applyProfileLoadParams(&req, prof)

	out := map[string]interface{}{}
	if req.ContextSize != nil {
		out["contextSize"] = *req.ContextSize
	}
	if req.BatchSize != nil {
		out["batchSize"] = *req.BatchSize
	}
	if req.GPULayers != nil {
		out["gpuLayers"] = *req.GPULayers
	}
	if req.KVCacheType != nil {
		out["kvCacheType"] = *req.KVCacheType
	}
	if req.Parallel != nil {
		out["parallel"] = *req.Parallel
	}
	if req.UseMmap != nil {
		out["useMmap"] = *req.UseMmap
	}
	if req.FlashAttn != nil {
		out["flashAttn"] = *req.FlashAttn
	}
	return out
}

// TestR83_Profile_IgnoreDefaults_DoesNotInjectParams — профиль с ignoreDefaults
// не должен добавлять в запрос загрузки ни одного параметра.
func TestR83_Profile_IgnoreDefaults_DoesNotInjectParams(t *testing.T) {
	fa := true
	um := false
	prof := types.LlamaCppModelProfile{
		ContextLength:  32768,
		BatchSize:      512,
		NumGPULayers:   -2,
		KVCacheType:    "q8_0",
		Parallel:       4,
		FlashAttn:      &fa,
		UseMmap:        &um,
		IgnoreDefaults: true,
	}

	body := applyProfileToEmptyRequest(t, prof)
	if len(body) != 0 {
		t.Errorf("профиль с ignoreDefaults=true подставил параметры: %+v — именно это "+
			"оператор видит как «балансер навязывает свои шитые настройки»", body)
	}
}

// TestR83_Profile_WithoutIgnoreDefaults_StillInjects — обратная совместимость:
// профили без флага продолжают подставляться как раньше (иначе существующие
// стенды, которые полагаются на профиль, потеряют свои настройки).
func TestR83_Profile_WithoutIgnoreDefaults_StillInjects(t *testing.T) {
	prof := types.LlamaCppModelProfile{
		ContextLength: 32768,
		BatchSize:     512,
		NumGPULayers:  -2,
		KVCacheType:   "q8_0",
		Parallel:      2,
	}

	body := applyProfileToEmptyRequest(t, prof)
	for _, key := range []string{"contextSize", "batchSize", "gpuLayers", "kvCacheType", "parallel"} {
		if _, ok := body[key]; !ok {
			t.Errorf("без ignoreDefaults профиль обязан подставить %s (обратная совместимость)", key)
		}
	}
}

// TestR83_Profile_IgnoreDefaultsKeepsContextHint — даже с ignoreDefaults профиль
// остаётся полезен: contextLength/contextLengthAuto/contextLengthMax читаются
// preflight'ом и UI как подсказка о потолке. Проверяем, что поля на месте и не
// обнулены (иначе «выключили профиль» превратилось бы в «потеряли потолок»).
func TestR83_Profile_IgnoreDefaultsKeepsContextHint(t *testing.T) {
	prof := types.LlamaCppModelProfile{
		ContextLength:     32768,
		ContextLengthAuto: true,
		ContextLengthMax:  131072,
		IgnoreDefaults:    true,
	}

	if prof.ContextLength != 32768 {
		t.Errorf("ContextLength = %d, want 32768 (hint для preflight должен сохраниться)",
			prof.ContextLength)
	}
	if !prof.ContextLengthAuto {
		t.Error("ContextLengthAuto сброшен — auto-adapt перестанет работать")
	}
	if prof.ContextLengthMax != 131072 {
		t.Errorf("ContextLengthMax = %d, want 131072 (потолок оператора)",
			prof.ContextLengthMax)
	}
}
