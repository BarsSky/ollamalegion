// Package balancer — R92 (S3 плана 2026-10-07): исполнитель стратегии `sharded`.
//
// План: plans/2026-10-07-model-distribution-multi-gpu-next-session.md §6 (S3):
//
//	«Снять executable:false для sharded; исполнитель строит/находит группу под
//	 модель. VRAM-fit считает СУММУ по узлам, а не max. fallback: error → 503 с
//	 числами (как R76), single → деградация с X-LB-Placement-Fallback».
//
// ЧТО ЗДЕСЬ ПРОИСХОДИТ. Резолвер политики спрашивает: «есть ли у модели
// логический бэкенд (группа RPC-узлов), и влезает ли она в него В СУММЕ?» Если
// да — решение остаётся исполнимым, а в причину/поля попадает группа (id, число
// узлов), которую дальше использует маршрутизация (selectBackend →
// UsableMaster). Если нет — решение помечается Degraded с ЧИСЛАМИ, и дальше
// работает общий механизм §6 (R76): fallback=error → 503 с причиной,
// fallback=single → single + X-LB-Placement-Fallback.
//
// ПОЧЕМУ СУММА, А НЕ MAX. При layer/row split веса лежат на РАЗНЫХ картах:
// «влезает» означает «влезает в сумму свободного VRAM узлов группы», а не
// «влезает на каждой карте по отдельности». Именно этим раскладка отличается от
// репликации, где каждая копия обязана влезть целиком (см.
// placement_fit_r75.go).
//
// ЧЕГО ЗДЕСЬ НЕТ: гарантии, что мастер действительно считает на воркерах.
// Это S1 (rpc-server в образе + CPPWORKER_RPC_*). Пока транспорт не собран,
// группа будет не готова (воркеры без RPC-сервера не поднимаются, метрики
// мастера не покажут загруженную модель) — и резолвер честно об этом скажет.
package balancer

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/internal/backendgroup"
	"ollama-loadbalancer/pkg/types"
)

// shardedFitR92 — результат проверки «есть ли у модели логический бэкенд и
// влезает ли она в него в сумме».
//
// Порядок полей — по требованию fieldalignment.
type shardedFitR92 struct {
	// Group — состояние группы на момент проверки (пустое, если группы нет).
	Group          backendgroup.GroupState
	Reason         string
	AvailableBytes int64
	NeedBytes      int64
	SizeBytes      int64
	UnknownGPU     []string
	HasGroup       bool
	SizeKnown      bool
	VRAMKnown      bool
	OK             bool
}

// shardedFitR92 — проверка исполнимости раскладки для модели.
func (p *Proxy) shardedFitR92(model string) shardedFitR92 {
	var fit shardedFitR92
	if p == nil || p.backendGroups == nil || !p.backendGroups.Configured() {
		fit.Reason = "логические бэкенды не настроены: раскладка выполняется группой узлов, " +
			"а balancing.backendGroups[] пуст — нечего раскладывать"
		return fit
	}
	state, ok := p.backendGroups.StateForModel(model)
	if !ok {
		fit.Reason = "для модели нет логического бэкенда: balancing.backendGroups[] не описывает " +
			"ни одной группы под эту модель (маска model не совпала)"
		return fit
	}
	fit.HasGroup = true
	fit.Group = state

	// Сумма по узлам группы: при раскладке слои лежат на разных картах.
	fit.AvailableBytes = int64(state.VRAMFreeMB) * 1024 * 1024
	fit.VRAMKnown = state.VRAMKnownMems > 0

	size := p.getModelSizeBytes(model)
	fit.SizeBytes = size
	fit.SizeKnown = size > 0
	if fit.SizeKnown {
		fit.NeedBytes = int64(float64(size) * placementVRAMSafetyFactor)
	}

	if !state.Usable {
		fit.Reason = "группа " + state.ID + " не готова к раскладке: " + state.DegradeReason
		return fit
	}

	if fit.SizeKnown && fit.VRAMKnown && fit.AvailableBytes < fit.NeedBytes {
		fit.Reason = fmt.Sprintf(
			"нужно ≈%s (веса %s + запас %.0f%%), а по узлам группы %s доступно %s",
			humanBytesR75(fit.NeedBytes), humanBytesR75(fit.SizeBytes),
			(placementVRAMSafetyFactor-1)*100, state.ID, humanBytesR75(fit.AvailableBytes))
		return fit
	}

	fit.OK = true
	if !fit.SizeKnown {
		// Не гадаем: без размера модели фит не проверен — так же поступает auto
		// (placement_fit_r75.go). Это не отказ, но и не подтверждение.
		fit.Reason = "размер модели неизвестен (нет профиля sizeBytes и метрик загруженной модели) — " +
			"VRAM-fit раскладки не проверен"
	}
	return fit
}

// describeShardedR92 — человекочитаемое описание принятой раскладки (в Reason
// решения и в /api/v1/placement).
func describeShardedR92(fit shardedFitR92) string {
	g := fit.Group
	msg := fmt.Sprintf("sharded → группа %s (%s): узлов %d", g.ID, g.Strategy, len(g.Members))
	if fit.VRAMKnown {
		msg += fmt.Sprintf(", свободно по узлам %s", humanBytesR75(fit.AvailableBytes))
	} else {
		msg += ", метрик VRAM узлов нет"
	}
	if fit.SizeKnown {
		msg += fmt.Sprintf(", нужно ≈%s (веса %s + запас %.0f%%)",
			humanBytesR75(fit.NeedBytes), humanBytesR75(fit.SizeBytes),
			(placementVRAMSafetyFactor-1)*100)
	}
	return msg
}

// checkShardedFeasibilityR92 — пометить решение Degraded, если раскладку нельзя
// исполнить сейчас; при успехе — приложить к решению id группы и число узлов.
//
// Проверка НЕ зависит от источника решения (в отличие от R79, который намеренно
// пропускает global/request ради обратной совместимости): operatingMode=
// distributed_inference тоже должен получать честный результат, иначе стенд без
// групп молча поедет на single с заголовком X-LB-Placement: sharded.
func (p *Proxy) checkShardedFeasibilityR92(d PlacementDecision) PlacementDecision {
	if d.Strategy != types.PlacementSharded || d.Degraded {
		return d
	}
	if p == nil || p.config == nil {
		return d
	}

	fit := p.shardedFitR92(d.Model)
	note := fit.Reason
	if fit.OK {
		note = describeShardedR92(fit)
		d.GroupID = fit.Group.ID
		d.GroupMembers = len(fit.Group.Members)
		// Для auto описание раскладки уже добавлено в resolveAutoStrategy —
		// второй раз не приписываем (иначе reason дублируется).
		if !d.Refined {
			d.Reason = appendPlacementReasonR92(d.Reason, note)
		}
		return d
	}

	d.Degraded = true
	d.Reason = appendPlacementReasonR92(d.Reason, note)

	// Обратная совместимость: при placement.enabled=false механизм §6 не
	// работает (placementRejection выходит сразу), и решение дошло бы до
	// заголовков как «executable sharded», хотя маршрутизация уйдёт на single.
	// Помечаем стратегию неисполнимой — ровно как до R92, чтобы заголовок
	// X-LB-Placement-Fallback остался честным.
	if !p.PlacementSettings().Enabled {
		d.Executable = false
	}
	return d
}

// appendPlacementReasonR92 добавляет пояснение к причине решения, не заводя
// пустых «; » при пустой исходной причине.
func appendPlacementReasonR92(reason, note string) string {
	reason = strings.TrimSpace(reason)
	note = strings.TrimSpace(note)
	if note == "" {
		return reason
	}
	if reason == "" {
		return note
	}
	return reason + "; " + note
}

// homogeneousShardedNoteR92 — проверка однородности узлов группы для
// auto.requireHomogeneous.
//
// Возвращает (запрещено, пояснение). Неоднородная группа (sm_86 + sm_120) —
// валидная конфигурация, но оператор должен понимать, что раскладка ляжет по
// разным по скорости картам; правило requireHomogeneous прямо это запрещает.
func (p *Proxy) homogeneousShardedNoteR92(state backendgroup.GroupState) (bool, string) {
	if len(state.Members) <= 1 {
		return false, ""
	}
	byIdentity := map[string][]string{}
	for _, m := range state.Members {
		byIdentity[p.backendGPUIdentity(m.BackendID)] = append(byIdentity[p.backendGPUIdentity(m.BackendID)], m.BackendID)
	}
	if len(byIdentity) <= 1 {
		return false, ""
	}
	parts := make([]string, 0, len(byIdentity))
	for identity, ids := range byIdentity {
		if identity == "" {
			identity = "неизвестно"
		}
		parts = append(parts, identity+"="+strings.Join(ids, ","))
	}
	return true, "однородность: узлы группы " + state.ID + " различаются по GPU (" +
		strings.Join(parts, "; ") + ")"
}
