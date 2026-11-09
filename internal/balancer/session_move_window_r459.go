// session_move_window_r459.go — R459 (2026-11-06): живую сессию НЕ уводят на
// копию с меньшим окном (и не уводят на холодный бэкенд, где модели нет).
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, A10 ×2, Qwen3.8-27B, Cline/DSH). Сессия работала
// на прогретой копии с окном 65536 на слот. Вторая копия была поднята вручную с
// окном МЕНЬШЕ (32768) и оказалась «менее загруженной». Ребалансировка увела
// туда ЖИВУЮ сессию — окно уменьшилось, агентский ход сломался на середине.
//
// Почему это дефект, а не «редкий случай»: правило ребалансировки смотрело
// ТОЛЬКО на загрузку, а окно — свойство, из-за которого ход либо работает, либо
// нет. Переезд, уменьшающий окно, нельзя «попробовать»: он уже стоил клиенту
// хода.
//
// ПРАВИЛО (для переезда живого запроса/сессии):
//  1. Клиент назвал окно (num_ctx) — копия на кандидате обязана его покрывать.
//  2. Не назвал — кандидат не должен дать МЕНЬШЕ, чем уже есть у сессии
//     (загруженное окно слота текущего бэкенда).
//  3. Модель на кандидате не загружена (окно неизвестно) — переезд хуже
//     очереди: там unload+load, то есть минуты тишины и, возможно, меньшее
//     окно. Такой кандидат отклоняется, и сессия ждёт СВОЙ слот (правило R139).
//
// Наблюдаемость: каждое отклонение пишется Debug-строкой с числами, чтобы
// «почему не переехали» не выяснялось чтением исходников.
package balancer

import (
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// sessionMoveKeepsWindowR459 — можно ли увести сессию с fromBackend на
// candidateBackend, не потеряв окно. Возвращает (можно, причина_отказа).
func (p *Proxy) sessionMoveKeepsWindowR459(candidateBackend, fromBackend, model string, requestedNCtx int) (bool, string) {
	if p == nil || candidateBackend == "" {
		return false, "кандидат не определён"
	}
	need := requestedNCtx
	if need <= 0 {
		// Клиент окно не называл — берём то, с которым уже работает сессия.
		need = p.loadedPerSeqNCtx(fromBackend, model)
	}
	if need <= 0 {
		// Ничего не известно (окно не загружено/метрики пусты) — прежнее
		// поведение: не выдумываем предел там, где его нет.
		return true, ""
	}
	// R478 (2026-11-09): у Ollama-бэкендов окна слота в наших метриках НЕТ
	// (num_ctx_resolver читает только llama.cpp-метрики). Прежний код считал
	// такой кандидат «холодным» ВСЕГДА — и сессия никогда не переезжала на
	// живую Ollama-копию. Это и было «перестало работать распределение».
	// Правило то же, что для неизвестного окна: не выдумываем предел там, где
	// его не знаем.
	if b := p.GetBackend(candidateBackend); b != nil && b.Type == types.BackendTypeOllama {
		return true, ""
	}
	perSeq := p.loadedPerSeqNCtx(candidateBackend, model)
	if perSeq <= 0 {
		return false, "на кандидате модель не загружена (переезд = выгрузка+загрузка)"
	}
	if perSeq < need {
		return false, "окно кандидата меньше нужного"
	}
	return true, ""
}

// sessionMoveGateR459 — предикат для поиска менее загруженного бэкенда: тот же
// проход отбора, но кандидаты, теряющие окно, отсеиваются сразу (см.
// findLessLoadedBackendWithModelWhere).
//
// R478 (2026-11-09): отклонения логируются на INFO, а не Debug. Аудит показал
// живое следствие: при штатном уровне info оператор не видел НИ ОДНОЙ причины
// «почему сессия не переехала», и это выглядело как «сломалось распределение
// нагрузки». Строка редкая (ребалансировка при высоком loadRatio), поэтому Info
// здесь оправдан.
func (p *Proxy) sessionMoveGateR459(fromBackend, model string, requestedNCtx int) func(string) bool {
	return func(candidate string) bool {
		ok, why := p.sessionMoveKeepsWindowR459(candidate, fromBackend, model, requestedNCtx)
		if !ok {
			logger.Get().Infow("R459: кандидат на ребалансировку отклонён",
				"session_backend", fromBackend, "candidate", candidate,
				"model", model, "requested_n_ctx", requestedNCtx,
				"candidate_per_seq_n_ctx", p.loadedPerSeqNCtx(candidate, model),
				"session_per_seq_n_ctx", p.loadedPerSeqNCtx(fromBackend, model),
				"reason", why)
		}
		return ok
	}
}
