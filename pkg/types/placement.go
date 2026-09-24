// Package types — Placement Policy (R72, этап P0).
//
// Placement policy — слой, который для КАЖДОЙ модели решает, как её
// обслуживать при нескольких бэкендах (`single` / `pool` / `replicated` /
// `sharded` / `rpc` / `auto`), вместо одного глобального
// `balancing.operatingMode`, обслуживающего все модели одинаково.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md
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
	"fmt"
	"strconv"
	"strings"
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
// R72 (P0): исполнимы single (обычный путь), pool (virtual_router) и
// replicated (modelreplication) — их подключение к политике идёт в P1.
// sharded/rpc требуют транспорта раскладки (P2) и пока только
// резолвятся/логируются: честная ошибка вместо тихой деградации — §6 плана.
func IsExecutablePlacementStrategy(s PlacementStrategy) bool {
	switch s {
	case PlacementSingle, PlacementPool, PlacementReplicated, PlacementAuto:
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
		errs = append(errs, fmt.Sprintf(
			"placement.fallback: неизвестное значение %q (допустимо: %q, %q)",
			p.Fallback, PlacementFallbackError, PlacementFallbackSingle))
	}

	if p.Enabled && len(p.Models) == 0 && len(p.Classes) == 0 {
		errs = append(errs, "placement.enabled=true, но не задано ни одного правила "+
			"(models[] или classes[]) — политика не на что не влияет")
	}

	for i, rule := range p.Models {
		prefix := fmt.Sprintf("placement.models[%d]", i)
		if strings.TrimSpace(rule.Model) == "" {
			errs = append(errs, prefix+": пустое поле model (нужно имя модели или маска)")
		}
		if !IsValidPlacementStrategy(rule.Strategy) {
			errs = append(errs, fmt.Sprintf("%s: неизвестная стратегия %q (допустимо: %s)",
				prefix, rule.Strategy, placementStrategyList()))
			continue
		}
		switch ParsePlacementStrategy(rule.Strategy) {
		case PlacementPool:
			if len(rule.Pool) == 0 {
				errs = append(errs, prefix+": strategy=pool требует непустой список pool[]")
			}
		case PlacementSharded, PlacementRPC:
			errs = append(errs, fmt.Sprintf(
				"%s: стратегия %q пока не исполняется (нужен транспорт раскладки, этап P2) — "+
					"запросы пойдут по fallback=%s", prefix, rule.Strategy, fallbackOrDefault(p.Fallback)))
		}
		switch strings.ToLower(strings.TrimSpace(rule.Selection)) {
		case "", "round_robin", "least_loaded":
		default:
			errs = append(errs, fmt.Sprintf("%s: неизвестный selection %q (допустимо: round_robin, least_loaded)",
				prefix, rule.Selection))
		}
		if rule.Auto != nil {
			for j, st := range rule.Auto.Prefer {
				if !IsValidPlacementStrategy(st) {
					errs = append(errs, fmt.Sprintf("%s.auto.prefer[%d]: неизвестная стратегия %q",
						prefix, j, st))
				}
			}
		}
	}

	for i, rule := range p.Classes {
		prefix := fmt.Sprintf("placement.classes[%d]", i)
		if strings.TrimSpace(rule.Match.Model) == "" && strings.TrimSpace(rule.Match.SizeGB) == "" {
			errs = append(errs, prefix+": пустое match — нужно хотя бы одно условие "+
				"(match.model или match.sizeGB)")
		}
		if strings.TrimSpace(rule.Match.SizeGB) != "" {
			if _, err := ParseSizeGBExpression(rule.Match.SizeGB); err != nil {
				errs = append(errs, fmt.Sprintf("%s.match.sizeGB: %v", prefix, err))
			}
		}
		if !IsValidPlacementStrategy(rule.Strategy) {
			errs = append(errs, fmt.Sprintf("%s: неизвестная стратегия %q (допустимо: %s)",
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
		return fmt.Errorf("неизвестная стратегия %q (допустимо: %s)", s, placementStrategyList())
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
		return nil, fmt.Errorf("пустое выражение размера (примеры: \">12\", \"<4\", \"4-8\")")
	}

	parse := func(s string) (float64, error) {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, fmt.Errorf("не число: %q", strings.TrimSpace(s))
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
			return nil, fmt.Errorf("диапазон %q перевёрнут: нижняя граница больше верхней", e)
		}
		return func(sizeGB float64) bool { return sizeGB >= lo && sizeGB <= hi }, nil
	}

	exact, err := parse(e)
	if err != nil {
		return nil, fmt.Errorf("неизвестное выражение размера %q (примеры: \">12\", \"<4\", \"4-8\")", e)
	}
	return func(sizeGB float64) bool { return sizeGB == exact }, nil
}
