// cold_candidate_ttl_r480.go — R480 (P1-5): «холодный кандидат» — это ФАКТ, а не
// догадка. Метрика окон имеет срок годности.
//
// ЗАЧЕМ. loadedPerSeqNCtx() возвращает 0 в трёх разных ситуациях:
//
//  1. модели на бэкенде действительно нет (свежая метрика, LoadedModels пуст);
//  2. поллер ещё ни разу не отработал по этому бэкенду (запись только создана);
//  3. поллер отстал/сломался — данные старше интервала (базовый 30 с).
//
// Гейт R459 трактовал все три как «модель не загружена» и отклонял переезд.
// Следствие: живые сессии стояли на занятой копии до ~30 с и дольше, пока
// метрика не обновится, — это и было «перестало работать распределение».
//
// ТЕПЕРЬ:
//
//	perSeq > 0                    -> окно известно (нужно сравнить с need);
//	perSeq = 0, метрика свежая    -> «точно нет модели» — переезд отклоняется
//	                                 (unload+load посреди сессии дороже очереди);
//	perSeq = 0, метрика устарела  -> «неизвестно» — кандидат ДОПУСКАЕТСЯ, но с
//	                                 предупреждением в лог: политика «не знаем —
//	                                 не выдумываем предел» та же, что для Ollama.
//
// Ручка: LB_R459_COLD_TTL_SEC (секунды, default 90 = 3× интервал поллера;
// 0 — не считать метрику устаревшей никогда, т.е. поведение «точно нет» везде,
// где запись вообще есть).
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// coldCandidateTTLDefaultSecR480 — 3× базовый интервал поллера (30 с).
const coldCandidateTTLDefaultSecR480 = 90

// coldCandidateTTLR480 — срок годности метрики окна.
//
// 0 — TTL выключен: любая существующая запись считается точной (0 = «модели
// нет»), а отсутствие записи — «неизвестно». Отрицательное/мусор — дефолт.
func coldCandidateTTLR480() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LB_R459_COLD_TTL_SEC"))
	if raw == "" {
		return time.Duration(coldCandidateTTLDefaultSecR480) * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return time.Duration(coldCandidateTTLDefaultSecR480) * time.Second
	}
	return time.Duration(n) * time.Second
}

// loadedPerSeqNCtxKnownR480 — окно на слот и достоверность вердикта.
//
// known=true — значение можно трактовать буквально: perSeq>0 — окно известно,
// perSeq==0 — модели действительно нет.
// known=false — данных нет или они устарели; трактовать 0 как «нет модели»
// нельзя.
func (p *Proxy) loadedPerSeqNCtxKnownR480(backendID, modelName string) (perSeq int, known bool) {
	perSeq = p.loadedPerSeqNCtx(backendID, modelName)
	if perSeq > 0 {
		return perSeq, true
	}
	if p == nil || p.metricsMgr == nil {
		return 0, false
	}
	ttl := coldCandidateTTLR480()
	age, hasAge := p.metricsMgr.LlamaCppPollAge(backendID, time.Now())
	if !hasAge {
		// Бэкенд ни разу не опрашивался — это «неизвестно», а не «нет модели».
		return 0, false
	}
	if ttl > 0 && age > ttl {
		return 0, false
	}
	return 0, true
}

// logColdCandidateUnknownR480 — предупреждение о допуске кандидата с устаревшей
// метрикой. Без него «переехали на холодный и ждали загрузку» снова выглядело
// бы загадкой: решение принято осознанно, но причина должна быть в логе.
func (p *Proxy) logColdCandidateUnknownR480(candidate, fromBackend, model string, need int) {
	if logger.Get() == nil {
		return
	}
	age, hasAge := time.Duration(0), false
	if p != nil && p.metricsMgr != nil {
		age, hasAge = p.metricsMgr.LlamaCppPollAge(candidate, time.Now())
	}
	fields := []interface{}{
		"session_backend", fromBackend, "candidate", candidate, "model", model,
		"need", need, "metrics_age_sec", int64(age.Seconds()),
		"metrics_ttl_sec", int64(coldCandidateTTLR480().Seconds()),
	}
	if !hasAge {
		fields = append(fields, "metrics_never_polled", true)
	}
	logger.Get().Warnw("R480: окно кандидата неизвестно (метрика устарела или отсутствует) — кандидат допущен",
		fields...)
}
