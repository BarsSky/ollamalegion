package api

// R72 (P0): GET /api/v1/placement — наблюдаемость placement-политики.
//
// Отдаёт:
//   - конфигурацию политики (enabled / allowRequestOverride / fallback);
//   - ошибки валидации конфига (понятные сообщения, не блокируют запуск);
//   - решения по всем описанным моделям + глобальный дефолт ("*");
//   - решение для конкретной модели: ?model=...&sizeGB=...&strategy=...;
//   - счётчики принятых решений (стратегия/источник).
//
// Требует X-API-Token (как остальные admin-endpoint'ы).
//
// План: docs/plans/2026-09-23-multi-backend-placement-policy.md (§5).

import (
	"net/http"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

func (s *Server) placementHandler(w http.ResponseWriter, r *http.Request) {
	// R100 (S5/S6 плана 2026-10-07): PUT — «запись balancing.placement»: применить
	// политику и логические бэкенды (см. handlers_placement_apply.go). GET остаётся
	// наблюдаемостью, поэтому метод решает, что именно делает ресурс.
	if r.Method == http.MethodPut {
		s.placementApplyR100(w, r)
		return
	}
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "method not allowed (GET — состояние, PUT — применить политику)",
		})
		return
	}

	q := r.URL.Query()
	cfg := s.proxy.PlacementSettings()

	globalMode := ""
	if s.proxy != nil {
		globalMode = s.proxy.OperatingMode()
	}

	resp := map[string]interface{}{
		"success":              true,
		"enabled":              cfg.Enabled,
		"allowRequestOverride": cfg.AllowRequestOverride,
		"fallback":             fallbackName(cfg.Fallback),
		"operatingMode":        globalMode,
		"strategies":           placementStrategyNames(),
		"executable":           executableStrategies(),
		"warnings":             warningsOrEmpty(s.proxy.PlacementWarnings()),
		"replication":          s.proxy.PlacementReplicationStatus(),
		"plan":                 "docs/plans/2026-09-23-multi-backend-placement-policy.md",
		"stage": "R92 (S2+S3 плана 2026-10-07): исполняются single/pool/replicated/sharded; " +
			"auto — VRAM-fit §4 (для sharded — по СУММЕ узлов группы); при fallback=error " +
			"неисполнимая СЕЙЧАС стратегия и degraded без allowDegraded отклоняются 503-й; " +
			"sharded требует logical backend (balancing.backendGroups[]); rpc — legacy-путь",
	}

	// R92 (S2): логические бэкенды — состав, готовность узлов, суммарный VRAM и
	// слоты группы. Нужны оператору и окну «Объединение бэкендов» (S6):
	// без этого невозможно показать, ПОЧЕМУ раскладка не собирается.
	resp["backendGroupWarnings"] = warningsOrEmpty(s.proxy.BackendGroupWarnings())
	resp["groups"] = s.proxy.BackendGroupStatuses()

	// R102 (S6): СЫРАЯ политика (те же поля, что принимает PUT).
	//
	// Зачем: PUT /api/v1/placement заменяет секции ЦЕЛИКОМ (иначе не выразить
	// удаление правила), а `decisions` — это уже РЕЗУЛЬТАТ резолва: по ним нельзя
	// восстановить, какие правила оператор написал и с какими причинами. Без
	// сырых правил UI не смог бы дописать одну группу, не стерев остальные.
	resp["placement"] = cfg
	// `groups` выше — это СОСТОЯНИЕ (health, VRAM, причина деградации), а здесь —
	// конфиг групп, который ждёт PUT. Для редактирования нужен именно он.
	resp["backendGroups"] = backendGroupsOrEmptyR102(s.proxy.RawBackendGroupsR102())

	// R97 (S7): если конфиг приехал из «режима» и был сконвертирован в политику,
	// оператор должен видеть это здесь, а не только в логе старта: причина
	// fallback=single и происхождение правил иначе выглядят необъяснимо.
	if notes := s.proxy.ModeMigrationNotes(); len(notes) > 0 {
		resp["modeMigration"] = notes
	}

	// Решение для конкретной модели (для отладки конфига и тестов).
	if model := strings.TrimSpace(q.Get("model")); model != "" {
		sizeGB, _ := strconv.ParseFloat(strings.TrimSpace(q.Get("sizeGB")), 64)
		override := strings.TrimSpace(q.Get("strategy"))
		resp["decision"] = s.proxy.ResolvePlacement(model, sizeGB, override)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	resp["decisions"] = s.proxy.PlacementDecisions()
	s.writeJSON(w, http.StatusOK, resp)
}

func fallbackName(f string) string {
	if strings.TrimSpace(f) == "" {
		return types.PlacementFallbackError
	}
	return f
}

// backendGroupsOrEmptyR102 — список групп (возможно пустой) для JSON-ответа.
//
// nil-слайс сериализуется в `null`, и UI пришлось бы отдельно проверять тип;
// пустой массив честнее: «групп сейчас нет».
func backendGroupsOrEmptyR102(groups []types.BackendGroupConfig) []types.BackendGroupConfig {
	if groups == nil {
		return []types.BackendGroupConfig{}
	}
	return groups
}

func warningsOrEmpty(w []string) []string {
	if w == nil {
		return []string{}
	}
	return w
}

func placementStrategyNames() []string {
	strategies := types.PlacementStrategies()
	out := make([]string, 0, len(strategies))
	for _, st := range strategies {
		out = append(out, string(st))
	}
	return out
}

func executableStrategies() []string {
	strategies := types.PlacementStrategies()
	out := make([]string, 0, len(strategies))
	for _, st := range strategies {
		if types.IsExecutablePlacementStrategy(st) {
			out = append(out, string(st))
		}
	}
	return out
}
