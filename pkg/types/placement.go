// Package types — Placement Policy (R72, этап P0).
//
// Placement policy — слой, который для КАЖДОЙ модели решает, как её
// обслуживать при нескольких бэкендах (`single` / `pool` / `replicated` /
// `sharded` / `rpc` / `auto`), вместо одного глобального
// `balancing.operatingMode`, обслуживающего все модели одинаково.
//
// План: docs/plans/2026-09-23-multi-backend-placement-policy.md
//
//	P0 (этот этап) — типы, разбор конфига, валидация, resolution,
//	     наблюдаемость. Поведение маршрутизации НЕ меняется.
//	P1 — pool/replicated/auto из политики.
//	P2 — sharded/rpc на реальном транспорте (открытый вопрос).
//
// Обратная совместимость: при `placement.enabled=false` (default) резолвер
// всегда возвращает стратегию, соответствующую текущему `operatingMode`.
package types

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/i18n"
)

// PlacementStrategy — способ обслуживания модели при нескольких бэкендах.
type PlacementStrategy string

const (
	// PlacementSingle — модель живёт на одном бэкенде, дальше обычная
	// балансировка (текущий `standard`).
	PlacementSingle PlacementStrategy = "single"
	// PlacementPool — набор бэкендов-кандидатов (виртуальная модель/алиас).
	PlacementPool PlacementStrategy = "pool"
	// PlacementReplicated — N копий модели на N бэкендах, запрос идёт в любую.
	PlacementReplicated PlacementStrategy = "replicated"
	// PlacementSharded — модель разрезана (слои/тензоры) между бэкендами.
	PlacementSharded PlacementStrategy = "sharded"
	// PlacementRPC — запрос уходит координатору распределённого инференса.
	PlacementRPC PlacementStrategy = "rpc"
	// PlacementAuto — стратегия выбирается по правилам (свободный VRAM,
	// требуемый контекст, число бэкендов).
	PlacementAuto PlacementStrategy = "auto"
)

// placementStrategies — допустимые значения (порядок = порядок в сообщениях).
var placementStrategies = []PlacementStrategy{
	PlacementSingle, PlacementPool, PlacementReplicated, PlacementSharded, PlacementRPC, PlacementAuto,
}

// PlacementStrategies возвращает список допустимых стратегий.
func PlacementStrategies() []PlacementStrategy {
	out := make([]PlacementStrategy, len(placementStrategies))
	copy(out, placementStrategies)
	return out
}

// ParsePlacementStrategy нормализует строку в PlacementStrategy.
// Регистр и пробелы не важны; пустая строка → PlacementSingle? нет —
// пустая строка возвращается как "" (значит «не задано»), чтобы вызывающий
// код отличал «не описано» от «явно single».
func ParsePlacementStrategy(s string) PlacementStrategy {
	return PlacementStrategy(strings.ToLower(strings.TrimSpace(s)))
}

// IsValidPlacementStrategy — допустима ли стратегия в конфиге.
func IsValidPlacementStrategy(s string) bool {
	st := ParsePlacementStrategy(s)
	for _, valid := range placementStrategies {
		if st == valid {
			return true
		}
	}
	return false
}

// IsExecutablePlacementStrategy — реализована ли стратегия в текущей версии.
//
// R72 (P0): single (обычный путь), pool (virtual_router), replicated
// (modelreplication), auto — подключены к политике на этапах P1/P1.5.
//
// R92 (S3 плана 2026-10-07-model-distribution-multi-gpu-next-session.md):
// `sharded` стал исполнимым — сборка умеет находить логический бэкенд (группу
// RPC-узлов, `balancing.backendGroups[]`) и считать VRAM-fit по СУММЕ узлов
// (internal/balancer/placement_sharded_r92.go). Это СТАТИЧЕСКИЙ ответ «в сборке
// есть исполнитель»: готовность конкретной модели здесь проверить нечем, её
// считает резолвер и помечает решение Degraded с числами, когда группы для
// модели нет. Так сохраняется §6 плана: отсутствие исполнителя не подменяется
// молча (fallback=error → 503, fallback=single → single + явный заголовок).
//
// `rpc` остаётся неисполнимым намеренно: это legacy-путь координатора-заглушки
// (internal/rpccoordinator, stub-mode). §9 плана переводит режим
// rpc_coordinator в стратегию sharded, поэтому `rpc` — deprecated-алиас.
func IsExecutablePlacementStrategy(s PlacementStrategy) bool {
	switch s {
	case PlacementSingle, PlacementPool, PlacementReplicated, PlacementAuto, PlacementSharded:
		return true
	default:
		return false
	}
}

// PlacementStrategyForOperatingMode отображает текущий глобальный режим
// балансера в эквивалентную placement-стратегию (§10 плана).
func PlacementStrategyForOperatingMode(mode string) PlacementStrategy {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "standard":
		return PlacementSingle
	case "virtual_router", "virtual-router":
		return PlacementPool
	case "replication":
		return PlacementReplicated
	case "rpc_coordinator", "rpc-coordinator":
		return PlacementRPC
	case "distributed_inference", "distributed-inference":
		return PlacementSharded
	default:
		// Неизвестный режим: не выдумываем стратегию — отдаём single
		// (это ровно то, что делает неизвестный режим сегодня: обычный путь).
		return PlacementSingle
	}
}

// PlacementFallbackError / PlacementFallbackSingle — значения
// `placement.fallback` (§6 плана): что делать, если выбранная стратегия
// недоступна.
const (
	PlacementFallbackError  = "error"
	PlacementFallbackSingle = "single"
)

// PlacementSettings — секция `balancing.placement` (R72).
//
// Порядок полей — по требованию fieldalignment (строки перед слайсами,
// скаляры в конце): CI линтует новые структуры.
type PlacementSettings struct {
	// Fallback — "error" (default) | "single".
	Fallback string `json:"fallback,omitempty"`
	// Classes — правила по классу моделей (например «все модели > 12 ГБ»).
	Classes []PlacementClassRule `json:"classes,omitempty"`
	// Models — правила по конкретной модели (точное имя или маска).
	Models []PlacementModelRule `json:"models,omitempty"`
	// Enabled — если false (default), политика выключена: резолвер
	// возвращает стратегию текущего operatingMode (обратная совместимость).
	Enabled bool `json:"enabled"`
	// AllowRequestOverride — разрешить переопределение стратегии per-request
	// заголовком `X-LB-Placement` (по умолчанию выключено).
	AllowRequestOverride bool `json:"allowRequestOverride"`
}

// PlacementClassRule — правило для класса моделей.
type PlacementClassRule struct {
	// Match — условия (все заданные должны совпасть).
	Match PlacementMatch `json:"match"`
	// Strategy — стратегия для подходящих моделей.
	Strategy string `json:"strategy"`
	// Reason — человекочитаемая причина (идёт в метрики и логи).
	Reason string `json:"reason,omitempty"`
}

// PlacementMatch — условия правила класса. Все заданные поля должны совпасть;
// пустое правило не матчит ничего (и это ошибка валидации).
type PlacementMatch struct {
	// Model — маска имени модели: `*` (любые символы), `?` (один символ),
	// регистр не важен. Пример: "Qwen3*".
	Model string `json:"model,omitempty"`
	// SizeGB — выражение размера модели в ГБ: ">12", ">=8", "<4", "<=8",
	// "8", "=8", "4-8" (диапазон включительно). Требует известного размера:
	// если размер модели неизвестен, условие НЕ совпадает (§4 плана).
	SizeGB string `json:"sizeGB,omitempty"`
}

// PlacementModelRule — правило для конкретной модели.
//
// Порядок полей — по требованию fieldalignment (указатель, строки, слайс).
type PlacementModelRule struct {
	// Auto — параметры выбора для strategy=auto.
	Auto *PlacementAutoRule `json:"auto,omitempty"`
	// Model — имя модели или маска (`Qwen3*`, `virtual:prod-chat`).
	Model string `json:"model"`
	// Strategy — single | pool | replicated | sharded | rpc | auto.
	Strategy string `json:"strategy"`
	// Reason — причина (для логов/UI).
	Reason string `json:"reason,omitempty"`
	// Selection — алгоритм выбора внутри pool: round_robin | least_loaded.
	Selection string `json:"selection,omitempty"`
	// Pool — список бэкендов для strategy=pool (`host:port`).
	Pool []string `json:"pool,omitempty"`
}

// PlacementAutoRule — параметры стратегии auto (§4 плана).
type PlacementAutoRule struct {
	// Prefer — порядок предпочтения стратегий, например
	// ["sharded","replicated","single"].
	Prefer []string `json:"prefer,omitempty"`
	// MinBackends — минимум подходящих бэкендов для репликации/раскладки.
	MinBackends int `json:"minBackends,omitempty"`
	// MaxVramPerBackendMB — сколько VRAM на бэкенд считаем допустимым.
	MaxVramPerBackendMB int `json:"maxVramPerBackendMB,omitempty"`
	// MaxShardCount — максимум шардов для sharded.
	MaxShardCount int `json:"maxShardCount,omitempty"`
	// RequireHomogeneous — требовать одинаковые GPU.
	RequireHomogeneous bool `json:"requireHomogeneous,omitempty"`
	// AllowDegraded — разрешить деградацию на single вместо ошибки.
	AllowDegraded bool `json:"allowDegraded,omitempty"`
}

// Validate проверяет секцию placement и возвращает список понятных ошибок
// (пустой список = конфиг корректен). Ошибки не блокируют запуск: они
// попадают в предупреждения при загрузке конфига и в `/api/v1/placement`.
func (p *PlacementSettings) Validate() []string {
	if p == nil {
		return nil
	}
	var errs []string

	switch strings.ToLower(strings.TrimSpace(p.Fallback)) {
	case "", PlacementFallbackError, PlacementFallbackSingle:
	default:
		errs = append(errs, i18n.T("types.placement.invalid_fallback",
			p.Fallback, PlacementFallbackError, PlacementFallbackSingle))
	}

	if p.Enabled && len(p.Models) == 0 && len(p.Classes) == 0 {
		errs = append(errs, i18n.T("types.placement.enabled_without_rules"))
	}

	for i, rule := range p.Models {
		prefix := fmt.Sprintf("placement.models[%d]", i)
		if strings.TrimSpace(rule.Model) == "" {
			errs = append(errs, prefix+": "+i18n.T("types.placement.empty_model"))
		}
		if !IsValidPlacementStrategy(rule.Strategy) {
			errs = append(errs, i18n.T("types.placement.rule_invalid_strategy",
				prefix, rule.Strategy, placementStrategyList()))
			continue
		}
		switch ParsePlacementStrategy(rule.Strategy) {
		case PlacementPool:
			if len(rule.Pool) == 0 {
				// R74: для виртуального алиаса пул берётся из registry
				// (balancing.virtualModels[].backendPool), поэтому pool[] в
				// правиле не обязателен — это предупреждение, а не ошибка.
				errs = append(errs, prefix+": "+i18n.T("types.placement.pool_without_list"))
			}
		case PlacementSharded:
			// R92 (S3): стратегия исполняется, но требует логического бэкенда
			// (`balancing.backendGroups[]`). Проверить это здесь нечем —
			// PlacementSettings не видит balancing целиком: состав группы
			// проверяет ValidateBackendGroups, а готовность КОНКРЕТНОЙ модели —
			// резолвер (Degraded с числами, §6). Ошибкой это не объявляем, иначе
			// валидация политики ломала бы стенды, где группа появится позже.
		case PlacementRPC:
			errs = append(errs, i18n.T("types.placement.rpc_not_executable",
				prefix, rule.Strategy, fallbackOrDefault(p.Fallback)))
		}
		switch strings.ToLower(strings.TrimSpace(rule.Selection)) {
		case "", "round_robin", "least_loaded":
		default:
			errs = append(errs, i18n.T("types.placement.invalid_selection",
				prefix, rule.Selection))
		}
		if rule.Auto != nil {
			for j, st := range rule.Auto.Prefer {
				if !IsValidPlacementStrategy(st) {
					errs = append(errs, i18n.T("types.placement.auto_prefer_invalid_strategy",
						prefix, j, st))
				}
			}
		}
	}

	for i, rule := range p.Classes {
		prefix := fmt.Sprintf("placement.classes[%d]", i)
		if strings.TrimSpace(rule.Match.Model) == "" && strings.TrimSpace(rule.Match.SizeGB) == "" {
			errs = append(errs, prefix+": "+i18n.T("types.placement.empty_match"))
		}
		if strings.TrimSpace(rule.Match.SizeGB) != "" {
			if _, err := ParseSizeGBExpression(rule.Match.SizeGB); err != nil {
				errs = append(errs, fmt.Sprintf("%s.match.sizeGB: %v", prefix, err))
			}
		}
		if !IsValidPlacementStrategy(rule.Strategy) {
			errs = append(errs, i18n.T("types.placement.rule_invalid_strategy",
				prefix, rule.Strategy, placementStrategyList()))
		}
	}

	return errs
}

// ValidatePlacementStrategyName — удобный хелпер для per-request override.
func ValidatePlacementStrategyName(s string) error {
	if s == "" {
		return nil
	}
	if !IsValidPlacementStrategy(s) {
		return errors.New(i18n.T("types.placement.invalid_strategy", s, placementStrategyList()))
	}
	return nil
}

func fallbackOrDefault(fallback string) string {
	if strings.TrimSpace(fallback) == "" {
		return PlacementFallbackError
	}
	return fallback
}

func placementStrategyList() string {
	names := make([]string, 0, len(placementStrategies))
	for _, s := range placementStrategies {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// ParseSizeGBExpression разбирает выражение размера модели в ГБ.
//
// Поддерживаются: ">12", ">=8", "<4", "<=8", "8", "=8", "4-8" (диапазон
// включительно). Возвращённая функция отвечает на вопрос «подходит ли размер».
func ParseSizeGBExpression(expr string) (func(sizeGB float64) bool, error) {
	e := strings.TrimSpace(expr)
	if e == "" {
		return nil, errors.New(i18n.T("types.placement.empty_size_expression"))
	}

	parse := func(s string) (float64, error) {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, errors.New(i18n.T("types.placement.not_a_number", strings.TrimSpace(s)))
		}
		return v, nil
	}

	switch {
	case strings.HasPrefix(e, ">="):
		bound, err := parse(strings.TrimPrefix(e, ">="))
		if err != nil {
			return nil, err
		}
		return func(sizeGB float64) bool { return sizeGB >= bound }, nil
	case strings.HasPrefix(e, "<="):
		bound, err := parse(strings.TrimPrefix(e, "<="))
		if err != nil {
			return nil, err
		}
		return func(sizeGB float64) bool { return sizeGB <= bound }, nil
	case strings.HasPrefix(e, ">"):
		bound, err := parse(strings.TrimPrefix(e, ">"))
		if err != nil {
			return nil, err
		}
		return func(sizeGB float64) bool { return sizeGB > bound }, nil
	case strings.HasPrefix(e, "<"):
		bound, err := parse(strings.TrimPrefix(e, "<"))
		if err != nil {
			return nil, err
		}
		return func(sizeGB float64) bool { return sizeGB < bound }, nil
	case strings.HasPrefix(e, "="):
		bound, err := parse(strings.TrimPrefix(e, "="))
		if err != nil {
			return nil, err
		}
		return func(sizeGB float64) bool { return sizeGB == bound }, nil
	}

	if idx := strings.Index(e, "-"); idx > 0 {
		lo, err := parse(e[:idx])
		if err != nil {
			return nil, err
		}
		hi, err := parse(e[idx+1:])
		if err != nil {
			return nil, err
		}
		if lo > hi {
			return nil, errors.New(i18n.T("types.placement.range_reversed", e))
		}
		return func(sizeGB float64) bool { return sizeGB >= lo && sizeGB <= hi }, nil
	}

	exact, err := parse(e)
	if err != nil {
		return nil, errors.New(i18n.T("types.placement.unknown_size_expression", e))
	}
	return func(sizeGB float64) bool { return sizeGB == exact }, nil
}
