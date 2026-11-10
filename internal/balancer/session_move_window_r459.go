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
//  2. Не назвал — кандидат не должен дать МЕНЬШЕ «нужного окна» сессии.
//  3. Модель на кандидате не загружена (окно неизвестно) — переезд хуже
//     очереди: там unload+load, то есть минуты тишины и, возможно, меньшее
//     окно. Такой кандидат отклоняется, и сессия ждёт СВОЙ слот (правило R139).
//
// R480 (2026-11-09): «нужное окно» больше не равно окну сессии всегда — оно
// считается как max(потребность запроса, окно АКТИВНОЙ сессии), см.
// session_move_need_r480.go (P1-4, вариант C). Здесь остаются сами проверки.
//
// Наблюдаемость: каждое отклонение пишется INFO-строкой с числами, чтобы
// «почему не переехали» не выяснялось чтением исходников.
package balancer

import (
	"strconv"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// sessionMoveKeepsWindowR459 — можно ли увести сессию с fromBackend на
// candidateBackend, не потеряв окно. Возвращает (можно, причина_отказа).
//
// Оставлен как совместимый вход для тестов и путей, где потребность запроса
// неизвестна: need := заявленное окно клиента, иначе окно сессии (поведение
// R459 до R480). Новые пути зовут sessionMoveKeepsWindowNeedR480 с уже
// посчитанным need.
func (p *Proxy) sessionMoveKeepsWindowR459(candidateBackend, fromBackend, model string, requestedNCtx int) (bool, string) {
	need := requestedNCtx
	if need <= 0 {
		need = p.loadedPerSeqNCtx(fromBackend, model)
	}
	return p.sessionMoveKeepsWindowNeedR480(candidateBackend, model, need, "r459")
}

// sessionMoveKeepsWindowNeedR480 — проверка кандидата при известном need.
//
// needSource идёт в причину отказа: «окно кандидата меньше нужного» без чисел
// уже один раз стоило разбора «а почему, собственно».
func (p *Proxy) sessionMoveKeepsWindowNeedR480(candidateBackend, model string, need int, needSource string) (bool, string) {
	if p == nil || candidateBackend == "" {
		return false, "кандидат не определён"
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
	perSeq, known := p.loadedPerSeqNCtxKnownR480(candidateBackend, model)
	if !known {
		// R480 (P1-5): метрика устарела или её ещё нет. Это «неизвестно», а не
		// «модели нет»: отклонять переезд до обновления метрики (~30 с) значило
		// держать живую сессию на занятой копии без причины.
		p.logColdCandidateUnknownR480(candidateBackend, "", model, need)
		return true, ""
	}
	if perSeq <= 0 {
		return false, "на кандидате модель не загружена (переезд = выгрузка+загрузка)"
	}
	if perSeq < need {
		return false, "окно кандидата меньше нужного: candidate=" +
			strconv.Itoa(perSeq) + " need=" + strconv.Itoa(need) + " source=" + needSource
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
	return p.sessionMoveGateR480(fromBackend, model, "", requestedNCtx, 0)
}

// sessionMoveGateR480 — гейт R459 с «нужным окном» по варианту C (P1-4):
// потребность запроса + окно активной сессии в пределах TTL.
func (p *Proxy) sessionMoveGateR480(fromBackend, model, sessionID string, requestedNCtx, requestTokens int) func(string) bool {
	need, source := p.sessionMoveNeedR480(fromBackend, model, sessionID, requestedNCtx, requestTokens)
	p.logMoveNeedR480(fromBackend, model, sessionID, requestedNCtx, requestTokens, need, source)
	return func(candidate string) bool {
		ok, why := p.sessionMoveKeepsWindowNeedR480(candidate, model, need, source)
		if !ok {
			logger.Get().Infow("R459: кандидат на ребалансировку отклонён",
				"session_backend", fromBackend, "candidate", candidate,
				"model", model, "requested_n_ctx", requestedNCtx,
				"request_need", requestTokens, "need", need, "need_source", source,
				"candidate_per_seq_n_ctx", p.loadedPerSeqNCtx(candidate, model),
				"session_per_seq_n_ctx", p.loadedPerSeqNCtx(fromBackend, model),
				"reason", why)
		}
		return ok
	}
}
