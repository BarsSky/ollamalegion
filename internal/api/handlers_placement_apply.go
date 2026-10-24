package api

// R100 (S5/S6 плана 2026-10-07): применение политики размещения и логических
// бэкендов — «запись balancing.placement».
//
// ЗАЧЕМ. До R100 сервер умел только ПРЕДЛАГАТЬ: `POST /api/v1/placement/plan`
// строил план, `POST /api/v1/placement/validate` проверял композицию, а записать
// результат было нечем — AC S5 («пустой стенд → рабочая политика») и S6
// («объединить две машины») упирались в это. Оператор оставался с планом в руках
// и правкой config.json вручную.
//
// ЧТО ДЕЛАЕТ. Принимает `placement` + `backendGroups`, проверяет их (та же
// валидация, что у пустого стенда), применяет к ЖИВОМУ балансеру и сохраняет
// конфиг на диск. Ничего не применяется, пока валидация не прошла: §6 плана —
// «никогда не подменять молча», и запись заведомо сломанной политики была бы
// худшим вариантом той же ошибки.
//
// ПЕРСИСТЕНЦИЯ — BEST-EFFORT, КАК ВЕЗДЕ. В bundled-стеке config.json смонтирован
// только для чтения (см. комментарии в handlers_cluster.go:470), поэтому
// `configSaver()` может упасть. Политика при этом УЖЕ применена к живому
// балансеру, и ответ честно говорит: `persisted: false` + причину. Делать вид,
// что запись удалась, нельзя: после рестарта стенд вернётся к прежнему файлу.
//
// СЕМАНТИКА — ПОЛНАЯ ЗАМЕНА, А НЕ MERGE. Присланный `placement` и
// `backendGroups` заменяют соответствующие секции целиком. Иначе окно объединения
// не смогло бы УБРАТЬ группу («Разъединить»), а мастер настройки — снять
// устаревшее правило: merge-семантика не выражает удаление без отдельного
// протокола «чего не хватает».

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// placementApplyRequestR100 — тело PUT /api/v1/placement.
//
// Порядок полей — по требованию fieldalignment.
type placementApplyRequestR100 struct {
	Placement     types.PlacementSettings    `json:"placement"`
	BackendGroups []types.BackendGroupConfig `json:"backendGroups"`
	// DryRun — только проверить, ничего не применять (кнопка «Проверить»).
	DryRun bool `json:"dryRun"`
	// AllowWarnings — применить, несмотря на предупреждения валидации.
	// Ошибки (неизвестная стратегия, группа без мастера) не обходятся никогда.
	AllowWarnings bool `json:"allowWarnings"`
}

// placementApplyR100 — PUT /api/v1/placement.
func (s *Server) placementApplyR100(w http.ResponseWriter, r *http.Request) {
	var req placementApplyRequestR100
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "некорректное тело запроса: " + err.Error(),
		})
		return
	}

	issues := placementApplyIssuesR100(req)
	errors := make([]string, 0, len(issues))
	for _, issue := range issues {
		errors = append(errors, issue)
	}

	if req.DryRun {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":   true,
			"dryRun":    true,
			"valid":     len(errors) == 0,
			"warnings":  errors,
			"applied":   false,
			"persisted": false,
			"placement": req.Placement,
			"groups":    req.BackendGroups,
		})
		return
	}

	if len(errors) > 0 && !req.AllowWarnings {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error": "политика не применена: конфиг не проходит валидацию " +
				"(исправьте замечания или подтвердите allowWarnings)",
			"warnings": errors,
			"applied":  false,
			"hint":     "проверьте тело через POST /api/v1/placement/validate — тот же набор проверок с числами",
		})
		return
	}

	// Применение: сначала ЖИВОЙ балансер (решения начинают действовать сразу),
	// затем конфиг как источник для записи на диск.
	s.proxy.SetPlacementSettings(req.Placement)
	s.proxy.SetBackendGroupsR92(types.BalancingSettings{BackendGroups: req.BackendGroups})
	s.config.Balancing.Placement = req.Placement
	s.config.Balancing.BackendGroups = req.BackendGroups

	persisted, persistErr := s.persistPlacementR100()

	logger.Get().Infow("placement policy applied",
		"enabled", req.Placement.Enabled,
		"rules", len(req.Placement.Models), "classes", len(req.Placement.Classes),
		"groups", len(req.BackendGroups), "persisted", persisted,
		"warnings", len(errors))

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"applied":   true,
		"persisted": persisted,
		"warnings":  errors,
		// Состояние ПОСЛЕ применения: оператор должен видеть, что группы
		// действительно зарегистрированы, а не только записаны в файл.
		"placement":    s.proxy.PlacementSettings(),
		"groups":       s.proxy.BackendGroupStatuses(),
		"persistError": persistErr,
		"hint": "политика применена к живому балансеру; при persisted=false " +
			"после рестарта стенд вернётся к конфиг-файлу",
	})
}

// placementApplyIssuesR100 — замечания валидации применяемого конфига.
//
// Ошибки и предупреждения здесь в одном списке: PlacementSettings.Validate и
// ValidateBackendGroups не разделяют уровни (в отличие от валидатора композиции
// internal/composition, где уровень — часть контракта).
func placementApplyIssuesR100(req placementApplyRequestR100) []string {
	issues := make([]string, 0, 4)
	issues = append(issues, req.Placement.Validate()...)
	issues = append(issues, types.ValidateBackendGroups(req.BackendGroups)...)
	return issues
}

// persistPlacementR100 — сохранить конфиг на диск (best-effort).
//
// Возвращает (сохранено, причина отказа). Отсутствие configSaver — не ошибка
// запроса, а особенность окружения (тесты, конфиг из env), поэтому это тоже
// причина, а не паника.
func (s *Server) persistPlacementR100() (bool, string) {
	if s.configSaver == nil {
		return false, "configSaver не установлен: конфиг не сохраняется на диск " +
			"(изменения действуют до перезапуска)"
	}
	if err := s.configSaver(); err != nil {
		logger.Get().Warnw("placement apply: configSaver failed", "error", err)
		return false, err.Error()
	}
	return true, ""
}
