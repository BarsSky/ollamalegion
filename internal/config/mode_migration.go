// Package config — R97 (S7 плана 2026-10-07): миграция `operatingMode` →
// `balancing.placement`.
//
// План §9:
//
//	| Было (operatingMode)      | Стало (balancing.placement)      |
//	| standard                  | дефолт: auto (single → replicated по fit) |
//	| virtual_router            | pool для алиасов                 |
//	| rpc_coordinator           | sharded (транспорт RPC)          |
//	| группы репликации         | replicated                       |
//
//	«Автоконверт при загрузке конфига + запись в лог „mode X → placement Y“.
//	 Обратная совместимость чтения — да, записи — нет».
//
// ГДЕ ВЫЗЫВАЕТСЯ. В `config.Load` сразу после `setDefaults` — то есть на реальном
// пути загрузки конфига при старте балансера. Это важно: функция
// `ValidateConfigOnLoad` (куда напрашивалось добавить миграцию) в продакшене НЕ
// вызывается вообще — только из тестов, поэтому хук в ней не сработал бы никогда.
//
// ПОЧЕМУ КОНВЕРТ НЕ ВКЛЮЧАЕТ §6 «В ЛОБ». До R72 режим не мог отказать в запросе:
// при `placement.enabled=false` механизм §6 (503 вместо тихой подмены) не
// работает вообще, и запрос обслуживался тем, что есть. Если бы миграция просто
// включила политику с `fallback=error` (дефолт), обновление балансера
// превратило бы обслуживавшиеся запросы в 503 — то есть смена поведения «на
// апгрейде», ровно то, чего §6 не хочет. Поэтому миграция выставляет
// `fallback=single`: обслуживание сохраняется, а деградация становится ВИДНОЙ
// (заголовок X-LB-Placement-Fallback, метрики placement, причина в
// /api/v1/placement). Переключение на `error` — осознанный шаг оператора, и
// заметка в логе ему об этом говорит.
//
// ЧЕГО МИГРАЦИЯ НЕ ДЕЛАЕТ. Не удаляет `operatingMode` (обратная совместимость
// чтения: §9 — «через один релиз убрать из примеров, затем из кода») и не
// включает `auto` для `standard`: auto начал бы создавать копии моделей там, где
// оператор репликацию не просил.
package config

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// placementCatchAllMask — маска «все модели» для правила класса.
//
// `classes[]` умеет матчить по маске, поэтому глобальный режим выражается
// правилом класса с `model: "*"` — это и есть «стратегия по умолчанию» в терминах
// политики (у самой политики поля «глобальная стратегия» нет: глобальный дефолт
// исторически берётся из operatingMode).
const placementCatchAllMask = "*"

// MigrateOperatingModeToPlacement выражает текущий `operatingMode` политикой
// размещения и возвращает заметки для лога (пустой список — конвертировать было
// нечего).
//
// Функция идемпотентна: после первой конвертации политика становится настроенной,
// и повторный вызов (в том числе на следующем старте) ничего не меняет и
// сообщает об этом одной заметкой.
func MigrateOperatingModeToPlacement(cfg *types.LoadBalancerConfig) []string {
	if cfg == nil {
		return nil
	}
	bal := &cfg.Balancing
	mode := strings.ToLower(strings.TrimSpace(bal.OperatingMode))

	if placementConfiguredR97(bal.Placement) {
		return []string{fmt.Sprintf(
			"operatingMode=%s: placement-политика уже настроена — правила оператора "+
				"приоритетнее, конвертация не выполнялась",
			describeModeR97(mode))}
	}

	switch mode {
	case "", "standard":
		// single — уже дефолт (types.PlacementStrategyForOperatingMode), правило
		// не нужно. auto НЕ включаем: он бы начал реплицировать модели.
		return []string{
			"operatingMode=" + describeModeR97(mode) + " → placement не требуется: " +
				"стратегия single и есть дефолт для моделей без правил. " +
				"Адаптивность (auto: single → replicated по свободному VRAM) включается " +
				"явным правилом placement.classes[{match:{model:\"" + placementCatchAllMask +
				"\"},strategy:\"auto\"}]; автоконверт её не включает, чтобы не создавать " +
				"копии моделей там, где оператор их не просил",
		}

	case "virtual_router", "virtual-router":
		return migrateVirtualRouterR97(bal, mode)

	case "replication":
		return migrateReplicationR97(bal, mode)

	case "rpc_coordinator", "rpc-coordinator", "distributed_inference", "distributed-inference":
		return migrateShardedR97(bal, mode)

	default:
		return []string{fmt.Sprintf(
			"operatingMode=%q: неизвестный режим — конвертировать нечем; "+
				"стратегия для моделей без правил остаётся single", mode)}
	}
}

// migrateVirtualRouterR97 — режим virtual_router → правила `pool` по алиасам.
//
// Именно алиасы, а не catch-all: pool без пула бэкендов бессмыслен (валидатор
// политики предупреждает об этом), а виртуальные модели в pipeline-режиме
// (Slices) политикой не выражаются — их исполняет свой интерцептор, и трогать
// его миграция не должна.
func migrateVirtualRouterR97(bal *types.BalancingSettings, mode string) []string {
	models := make([]types.PlacementModelRule, 0, len(bal.VirtualModels.Models))
	for i := range bal.VirtualModels.Models {
		vm := &bal.VirtualModels.Models[i]
		if vm == nil || strings.TrimSpace(vm.Name) == "" || !vm.IsAliasOnPoolMode() {
			continue
		}
		models = append(models, types.PlacementModelRule{
			Model:     strings.TrimSpace(vm.Name),
			Strategy:  string(types.PlacementPool),
			Pool:      append([]string(nil), vm.BackendPool...),
			Selection: string(vm.Selection),
			Reason:    "R97: мигрировано из operatingMode=" + mode,
		})
	}
	if len(models) == 0 {
		return []string{
			"operatingMode=" + mode + " → конвертировать нечего: ни одной виртуальной " +
				"модели в alias-on-pool режиме (нужны backendPool[] и modelName). " +
				"Pipeline-режим (slices) политикой не выражается и остаётся как есть",
		}
	}
	bal.Placement = types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackSingle,
		Models:   models,
	}
	return []string{
		fmt.Sprintf("operatingMode=%s → placement: %d правил strategy=pool по виртуальным алиасам",
			mode, len(models)),
		fallbackNoteR97(),
	}
}

// migrateReplicationR97 — режим replication → правила `replicated` по группам.
//
// Берём только модели с описанными группами: сегодня режим репликации
// обслуживает через группы, а модели без группы идут обычным путём — конвертация
// catch-all-правилом изменила бы их поведение.
func migrateReplicationR97(bal *types.BalancingSettings, mode string) []string {
	groups := bal.ModelReplication.Groups
	models := make([]types.PlacementModelRule, 0, len(groups))
	for _, group := range groups {
		name := strings.TrimSpace(group.ModelName)
		if name == "" {
			continue
		}
		rule := types.PlacementModelRule{
			Model:    name,
			Strategy: string(types.PlacementReplicated),
			Reason:   "R97: мигрировано из operatingMode=" + mode,
		}
		if group.MinInstances > 0 {
			rule.Auto = &types.PlacementAutoRule{MinBackends: group.MinInstances}
		}
		models = append(models, rule)
	}
	if len(models) == 0 {
		return []string{
			"operatingMode=" + mode + " → конвертировать нечего: групп репликации " +
				"(balancing.modelReplication.groups[]) не описано",
		}
	}
	bal.Placement = types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackSingle,
		Models:   models,
	}
	return []string{
		fmt.Sprintf("operatingMode=%s → placement: %d правил strategy=replicated по группам репликации",
			mode, len(models)),
		fallbackNoteR97(),
	}
}

// migrateShardedR97 — rpc_coordinator / distributed_inference → `sharded`.
//
// §9 плана переводит оба режима в sharded: с R92/S3 у `sharded` есть исполнитель
// (логический бэкенд, balancing.backendGroups[]). Пока групп не описано,
// стратегия честно деградирует по fallback=single — как и раньше, когда
// неисполнимая стратегия уходила на обычный путь.
func migrateShardedR97(bal *types.BalancingSettings, mode string) []string {
	bal.Placement = types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackSingle,
		Classes: []types.PlacementClassRule{{
			Match:    types.PlacementMatch{Model: placementCatchAllMask},
			Strategy: string(types.PlacementSharded),
			Reason:   "R97: мигрировано из operatingMode=" + mode,
		}},
	}
	notes := []string{
		"operatingMode=" + mode + " → placement: правило класса на все модели strategy=sharded " +
			"(§9 плана; исполнитель раскладки — логический бэкенд)",
		fallbackNoteR97(),
	}
	if mode == "rpc_coordinator" || mode == "rpc-coordinator" {
		notes = append(notes,
			"operatingMode=rpc_coordinator: легаси-координатор (stub-mode) перехватывает "+
				"запросы РАНЬШЕ политики, поэтому поведение не меняется. Для настоящей "+
				"раскладки нужны balancing.backendGroups[] (мастер + воркеры) и транспорт "+
				"RPC (S1: rpc-server в образе, CPPWORKER_RPC_*)")
	} else {
		notes = append(notes,
			"operatingMode=distributed_inference: пока не описаны balancing.backendGroups[], "+
				"раскладка невозможна и запросы обслуживаются одним узлом (fallback=single). "+
				"Опишите группу и проверьте её через POST /api/v1/placement/validate")
	}
	return notes
}

// fallbackNoteR97 — почему миграция выставила fallback=single.
func fallbackNoteR97() string {
	return "placement.fallback=single выставлен автоматически: при error миграция " +
		"превратила бы обслуживавшиеся запросы в 503 (при operatingMode механизм §6 " +
		"не работал). Когда стенд проверен — переключите на error в balancing.placement"
}

// placementConfiguredR97 — настроена ли политика (тогда конвертация не нужна).
func placementConfiguredR97(p types.PlacementSettings) bool {
	return p.Enabled || len(p.Models) > 0 || len(p.Classes) > 0
}

// describeModeR97 — имя режима для лога (пустое значение = standard).
func describeModeR97(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "standard"
	}
	return mode
}
