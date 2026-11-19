// autotune_apply_result.go — R636 (2026-11-19): результат применения: свежая копия модели, расхождение, AutoTuneApplyResult, unload.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"time"
)

// AutoTuneApplyResult — R54.6 (2026-08-24): результат apply AutoTune plan.
type AutoTuneApplyResult struct {
	BackendID  string    `json:"backendId"`
	ModelName  string    `json:"modelName"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Unloaded — R283: модель РЕАЛЬНО выгружена в ходе apply.
	//
	// Зачем отдельное поле: пометка разрушительной операции (noteDestructiveOpR271)
	// ставится до apply, но apply может упасть ДО выгрузки — например воркер
	// ответил «model is busy with active inference requests». Тогда пометку надо
	// снять, иначе следующий оборванный стрим получит причину
	// `balancer_autotune` без всякой перезагрузки, и клиенту сообщат неправду о
	// причине обрыва. Если же выгрузка состоялась, пометка обязана остаться —
	// обрыв в этом окне действительно наш.
	Unloaded bool `json:"unloaded,omitempty"`
}

// autotuneModelManager — R54.6: accessor для ModelManager (используем existing p.modelManager).
// Метод переименован чтобы не конфликтовать с полем modelManager.
func (p *Proxy) autotuneModelManager() *ModelManager {
	if p == nil {
		return nil
	}
	return p.modelManager // existing field set in NewProxy
}

// executeLlamaCppUnload — R54.6: helper для unload через cppworker API.
// Использует существующий POST /api/models/unload.
//
// R412 (2026-11-03): аргумент — backendID, а НЕ имя модели.
//
// ЖИВОЙ ДЕФЕКТ. AutoTune apply передавал сюда plan.ModelName и дальше в
// ModelManager.executeLlamaCppUnload(host, port, backendID, req) — то есть имя
// модели попадало в параметр backendID. workerAdminToken(«gemma-4-…») не
// находил бэкенд, токен получался пустым, заголовок X-API-Token не ставился —
// воркер отвечал 401 «invalid or missing API token». На стенде это выглядело
// так: «autotune reload failed … unload failed: cppworker error (HTTP 401)» при
// одинаковом токене во всех .env, reload не применялся, а AutoTune повторял
// попытку на каждом запросе (kv_cache q4_0 → f16).
//
// Загрузка (executeLlamaCppLoad) тот же backendID получала правильно, поэтому
// «unload 401, load даже не начинался» — ровно эта асимметрия и была уликой.
func (p *Proxy) executeLlamaCppUnload(backendID, modelName string) *ModelOpResult {
	// Try ModelManager first
	if mm := p.autotuneModelManager(); mm != nil {
		backend := p.GetBackend(backendID)
		if backend == nil {
			return &ModelOpResult{
				Success:   false,
				Operation: "unload",
				ModelName: modelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("backend %s not found", backendID),
			}
		}
		return mm.executeLlamaCppUnload(backend.Host, backend.CppWorkerPort, backendID, ModelOpRequest{
			Operation: "unload",
			ModelName: modelName,
		})
	}
	return &ModelOpResult{
		Success:   false,
		Operation: "unload",
		ModelName: modelName,
		BackendID: backendID,
		Error:     "ModelManager not available; cannot unload",
	}
}
