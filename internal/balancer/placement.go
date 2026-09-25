// Package balancer — Placement Policy resolver (R72, этап P0).
//
// Слой, который для каждой модели решает, КАК её обслуживать при нескольких
// бэкендах, вместо одного глобального `balancing.operatingMode`.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md
//
//	P0 (этот файл) — resolution по приоритету запрос → модель → класс →
//	   глобальный дефолт, наблюдаемость (лог, заголовки ответа, счётчики,
//	   /api/v1/placement). Маршрутизация НЕ меняется: исполнение стратегий
//	   (pool/replicated/auto) подключается на этапе P1.
//
// Почему отдельный слой: сегодня режим — один глобальный переключатель, и
// специфичные режимы взаимоисключающие («первый подходящий интерцептор
// выигрывает», proxy.go). Политика делает standard дефолтом, а раскладку —
// свойством конкретной модели.
package balancer

import (
	"sort"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// PlacementSource — откуда взялась стратегия (для логов/метрик/UI).
type PlacementSource string

const (
	// PlacementSourceDisabled — политика выключена, стратегия = operatingMode.
	PlacementSourceDisabled PlacementSource = "disabled"
	// PlacementSourceRequest — per-request override (X-LB-Placement).
	PlacementSourceRequest PlacementSource = "request"
	// PlacementSourceModel — правило placement.models[].
	PlacementSourceModel PlacementSource = "model"
	// PlacementSourceClass — правило placement.classes[].
	PlacementSourceClass PlacementSource = "class"
	// PlacementSourceGlobal — глобальный дефолт (operatingMode).
	PlacementSourceGlobal PlacementSource = "global"
)

// PlacementDecision — решение о размещении одной модели.
//
// Порядок полей — по требованию fieldalignment (строки, затем скаляры).
type PlacementDecision struct {
	// Model — модель, для которой принято решение.
	Model string `json:"model"`
	// Strategy — выбранная стратегия.
	Strategy types.PlacementStrategy `json:"strategy"`
	// Source — источник решения: request | model | class | global | disabled.
	Source PlacementSource `json:"source"`
	// Reason — человекочитаемая причина (из правила либо сформированная).
	Reason string `json:"reason"`
	// MatchedRule — текстовое описание сработавшего правила (для UI).
	MatchedRule string `json:"matchedRule,omitempty"`
	// Fallback — что делать, если стратегия недоступна (error|single).
	Fallback string `json:"fallback"`
	// RuleIndex — индекс правила в конфиге (-1 = правило не применялось).
	RuleIndex int `json:"ruleIndex"`
	// Executable — реализована ли стратегия в этой версии (sharded/rpc — нет).
	Executable bool `json:"executable"`
	// Refined — R74 (P1): strategy=auto превращена в конкретную стратегию.
	Refined bool `json:"refined,omitempty"`
	// Degraded — R75 (P1.5): auto не смогла выбрать стратегию (ни один вариант
	// не подошёл) и деградировала на single — причина в Reason.
	Degraded bool `json:"degraded,omitempty"`
}

// placementCounters — счётчики решений (наблюдаемость P0).
//
// В P0 счётчиков нет: они появятся вместе с исполнением стратегий (P1), когда
// решение начнёт влиять на маршрутизацию. Сейчас наблюдаемость — это лог
// решения, заголовки ответа X-LB-Placement* и GET /api/v1/placement.

// PlacementSettings возвращает секцию `balancing.placement` текущего конфига.
func (p *Proxy) PlacementSettings() types.PlacementSettings {
	if p == nil || p.config == nil {
		return types.PlacementSettings{}
	}
	return p.config.Balancing.Placement
}

// SetPlacementSettings заменяет placement-политику (reload конфига, тесты).
//
// ВНИМАНИЕ: не для конкурентного вызова из горутин обработки запросов —
// на живом стенде политика меняется вместе с перезагрузкой конфига.
func (p *Proxy) SetPlacementSettings(cfg types.PlacementSettings) {
	if p == nil || p.config == nil {
		return
	}
	p.config.Balancing.Placement = cfg
	// R74 (P1): после смены политики приводим группы репликации в соответствие.
	for _, action := range p.syncPlacementReplicationGroups() {
		logger.Get().Infow("placement: replication sync", "action", action)
	}
}

// OperatingMode возвращает текущий глобальный режим балансера
// ("standard" если не задан) — используется в отчёте /api/v1/placement.
func (p *Proxy) OperatingMode() string {
	if p == nil || p.config == nil {
		return "standard"
	}
	return operatingModeName(p.config.Balancing.OperatingMode)
}

// PlacementWarnings — ошибки валидации placement-конфига.
//
// Считается на месте (правил в конфиге единицы — кеш не нужен) и вызывается
// только из GET /api/v1/placement, а не из горячего пути запросов.
func (p *Proxy) PlacementWarnings() []string {
	cfg := p.PlacementSettings()
	return cfg.Validate()
}

// ResolvePlacement принимает решение о размещении для модели.
//
// sizeGB — размер модели в ГБ (0 = неизвестен: правила класса с sizeGB тогда
// не совпадают, см. §4 плана). override — значение заголовка X-LB-Placement
// (учитывается только при placement.allowRequestOverride=true).
func (p *Proxy) ResolvePlacement(model string, sizeGB float64, override string) PlacementDecision {
	if p == nil || p.config == nil {
		return PlacementDecision{
			Model:      model,
			Strategy:   types.PlacementSingle,
			Source:     PlacementSourceDisabled,
			Reason:     "конфигурация недоступна",
			RuleIndex:  -1,
			Executable: true,
			Fallback:   types.PlacementFallbackError,
		}
	}
	return p.refinePlacementDecision(ResolvePlacementDecision(
		p.PlacementSettings(),
		p.config.Balancing.OperatingMode,
		model, sizeGB, override,
	))
}

// refinePlacementDecision — R74 (P1): превратить strategy=auto в конкретную
// стратегию, используя то, что известно самому процессу.
//
// R75 (P1.5): выбор идёт по правилу §4 плана — VRAM-fit:
//  1. модель — виртуальный алиас (registry, alias-on-pool) → pool;
//  2. иначе перебираем `auto.prefer` (по умолчанию только single):
//     single — если модель влезает хотя бы на один бэкенд;
//     replicated — если влезает на `auto.minBackends` бэкендов;
//     sharded/rpc — пропускаются (этап P2);
//  3. если ничего не подошло — деградация: single + `degraded=true` и причина
//     с числами (need ≈ веса × 1.25, максимум свободного VRAM).
//
// «Влезает» = модель уже загружена на бэкенде ИЛИ свободный VRAM ≥ need
// (см. placementFitForModel). Точный расчёт KV под конкретный n_ctx остаётся в
// cppworker — здесь грубая, но консервативная оценка.
func (p *Proxy) refinePlacementDecision(d PlacementDecision) PlacementDecision {
	if d.Strategy != types.PlacementAuto {
		return d
	}
	d.Refined = true

	rule, ruleFound := p.matchingModelRule(d.Model)
	strategy, degraded, autoReason := p.resolveAutoStrategy(d, rule, ruleFound)
	d.Strategy = strategy
	d.Degraded = degraded
	d.Executable = types.IsExecutablePlacementStrategy(d.Strategy)
	if autoReason != "" {
		d.Reason = strings.TrimSpace(d.Reason + "; " + autoReason)
	}
	return d
}

// PlacementMetricsSummary — R78 (P3): компактная сводка политики размещения для
// GET /api/v1/metrics (полные решения — в GET /api/v1/placement).
//
// Показывает, что политика активна и не деградирует молча: сколько моделей
// описано, сколько решений деградировало/неисполнимо, по каким стратегиям
// раскладываются решения, сколько предупреждений у конфига и готова ли
// репликация.
func (p *Proxy) PlacementMetricsSummary() map[string]interface{} {
	cfg := p.PlacementSettings()
	summary := map[string]interface{}{
		"enabled":       cfg.Enabled,
		"fallback":      p.placementFallbackMode(),
		"operatingMode": p.OperatingMode(),
		"rules":         len(cfg.Models),
		"classes":       len(cfg.Classes),
	}
	if !cfg.Enabled && len(cfg.Models) == 0 && len(cfg.Classes) == 0 {
		// Политика не настроена — метрики не засоряем.
		summary["configured"] = false
		return summary
	}
	summary["configured"] = true

	byStrategy := map[string]int{}
	degraded := 0
	notExecutable := 0
	decisions := p.PlacementDecisions()
	for _, d := range decisions {
		byStrategy[string(d.Strategy)]++
		if d.Degraded {
			degraded++
		}
		if !d.Executable {
			notExecutable++
		}
	}
	summary["byStrategy"] = byStrategy
	summary["decisions"] = len(decisions)
	summary["degraded"] = degraded
	summary["notExecutable"] = notExecutable

	warnings := p.PlacementWarnings()
	summary["warnings"] = len(warnings)
	if len(warnings) > 0 {
		summary["warningList"] = warnings
	}

	repl := p.PlacementReplicationStatus()
	summary["replicationReady"] = repl["managerReady"]
	if groups, ok := repl["groups"].(map[string]interface{}); ok {
		withGroup := 0
		for _, g := range groups {
			if entry, ok := g.(map[string]interface{}); ok && entry["hasGroup"] == true {
				withGroup++
			}
		}
		summary["replicationGroups"] = withGroup
	}
	return summary
}

// matchingModelRule — правило модели, которое сработало бы для этого имени
// (нужно refinement'у auto: там лежат auto.prefer/minBackends).
func (p *Proxy) matchingModelRule(model string) (types.PlacementModelRule, bool) {
	for _, rule := range p.PlacementSettings().Models {
		if matchModelMask(rule.Model, model) {
			return rule, true
		}
	}
	return types.PlacementModelRule{}, false
}

// healthyBackendCount — сколько бэкендов сейчас healthy (для auto).
func (p *Proxy) healthyBackendCount() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if state.Backend.Status == types.StatusHealthy {
			n++
		}
	}
	return n
}

func itoaR74(v int) string { return strconv.Itoa(v) }

// ResolvePlacementDecision — чистая функция resolution (без Proxy):
// приоритет по §3.1 плана: запрос → модель → класс → глобальный дефолт.
func ResolvePlacementDecision(
	cfg types.PlacementSettings,
	operatingMode, model string,
	sizeGB float64,
	override string,
) PlacementDecision {
	globalStrategy := types.PlacementStrategyForOperatingMode(operatingMode)
	fallback := strings.TrimSpace(cfg.Fallback)
	if fallback == "" {
		fallback = types.PlacementFallbackError
	}

	decision := PlacementDecision{
		Model:    model,
		Strategy: globalStrategy,
		Source:   PlacementSourceGlobal,
		Reason: "глобальный режим балансера " + operatingModeName(operatingMode) +
			" (модель не описана в placement-политике)",
		RuleIndex: -1,
		Fallback:  fallback,
	}

	// 1. Per-request override.
	if override != "" && cfg.AllowRequestOverride && types.IsValidPlacementStrategy(override) {
		decision.Strategy = types.ParsePlacementStrategy(override)
		decision.Source = PlacementSourceRequest
		decision.Reason = "переопределение запросом X-LB-Placement=" + override
		decision.Executable = types.IsExecutablePlacementStrategy(decision.Strategy)
		return decision
	}

	if !cfg.Enabled {
		decision.Source = PlacementSourceDisabled
		decision.Reason = "placement-политика выключена (placement.enabled=false): " +
			"стратегия взята из operatingMode=" + operatingModeName(operatingMode)
		decision.Executable = types.IsExecutablePlacementStrategy(decision.Strategy)
		return decision
	}

	// 2. Правило по модели.
	for i, rule := range cfg.Models {
		if !matchModelMask(rule.Model, model) {
			continue
		}
		decision.Strategy = types.ParsePlacementStrategy(rule.Strategy)
		decision.Source = PlacementSourceModel
		decision.RuleIndex = i
		decision.MatchedRule = placementRuleRef("models", i) + ": " + rule.Model
		decision.Reason = rule.Reason
		if decision.Reason == "" {
			decision.Reason = "правило модели " + rule.Model
		}
		decision.Executable = types.IsExecutablePlacementStrategy(decision.Strategy)
		return decision
	}

	// 3. Правило класса.
	for i, rule := range cfg.Classes {
		if !classMatches(rule.Match, model, sizeGB) {
			continue
		}
		decision.Strategy = types.ParsePlacementStrategy(rule.Strategy)
		decision.Source = PlacementSourceClass
		decision.RuleIndex = i
		decision.MatchedRule = placementRuleRef("classes", i) + ": " + describeMatch(rule.Match)
		decision.Reason = rule.Reason
		if decision.Reason == "" {
			decision.Reason = "правило класса " + describeMatch(rule.Match)
		}
		decision.Executable = types.IsExecutablePlacementStrategy(decision.Strategy)
		return decision
	}

	// 4. Глобальный дефолт (operatingMode).
	decision.Executable = types.IsExecutablePlacementStrategy(decision.Strategy)
	return decision
}

// PlacementDecisions — решения для всех описанных в политике моделей плюс
// синтетическая запись "*" (глобальный дефолт) — для /api/v1/placement.
func (p *Proxy) PlacementDecisions() []PlacementDecision {
	cfg := p.PlacementSettings()

	seen := make(map[string]bool, len(cfg.Models))
	out := make([]PlacementDecision, 0, len(cfg.Models)+1)
	// Глобальный дефолт — первой строкой: сразу видно, что получат модели
	// без явного правила.
	out = append(out, p.ResolvePlacement("*", 0, ""))
	tail := make([]PlacementDecision, 0, len(cfg.Models))
	for _, rule := range cfg.Models {
		if rule.Model == "" || seen[rule.Model] {
			continue
		}
		seen[rule.Model] = true
		sizeGB := float64(p.getModelSizeBytes(rule.Model)) / (1024 * 1024 * 1024)
		tail = append(tail, p.ResolvePlacement(rule.Model, sizeGB, ""))
	}
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].Model < tail[j].Model })
	return append(out, tail...)
}

// recordPlacementDecision — заглушка наблюдаемости P0: решения видны в логах
// (Debug), в заголовках ответа X-LB-Placement/X-LB-Placement-Source и в
// GET /api/v1/placement. Счётчики появятся на этапе P1 вместе с исполнением
// стратегий (когда решение начнёт влиять на маршрутизацию).
func (p *Proxy) recordPlacementDecision(_ PlacementDecision) {}

// classMatches — все заданные условия класса должны совпасть.
func classMatches(m types.PlacementMatch, model string, sizeGB float64) bool {
	if strings.TrimSpace(m.Model) == "" && strings.TrimSpace(m.SizeGB) == "" {
		return false // пустое правило ничего не матчит
	}
	if strings.TrimSpace(m.Model) != "" && !matchModelMask(m.Model, model) {
		return false
	}
	if strings.TrimSpace(m.SizeGB) != "" {
		// Размер неизвестен → условие по размеру не считается выполненным
		// (не угадываем: §4 плана — решение должно быть обоснованным).
		if sizeGB <= 0 {
			return false
		}
		matcher, err := types.ParseSizeGBExpression(m.SizeGB)
		if err != nil {
			return false
		}
		if !matcher(sizeGB) {
			return false
		}
	}
	return true
}

// matchModelMask — сопоставление имени модели с маской.
//
// Поддерживаются `*` (любая последовательность) и `?` (один символ),
// регистр не важен. Сравниваем и полное имя, и имя без расширения `.gguf`
// (cgpt-подобные клиенты присылают и то, и другое).
func matchModelMask(pattern, model string) bool {
	pat := strings.ToLower(strings.TrimSpace(pattern))
	if pat == "" {
		return false
	}
	candidates := []string{strings.ToLower(strings.TrimSpace(model))}
	if trimmed := strings.TrimSuffix(candidates[0], ".gguf"); trimmed != candidates[0] {
		candidates = append(candidates, trimmed)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if wildcardMatch(pat, candidate) {
			return true
		}
	}
	return false
}

// wildcardMatch — простой матчер `*`/`?` (path.Match не подходит: он
// останавливается на `/`, а имена моделей содержат `hf.co/...`).
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			mark = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func describeMatch(m types.PlacementMatch) string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(m.Model) != "" {
		parts = append(parts, "model="+m.Model)
	}
	if strings.TrimSpace(m.SizeGB) != "" {
		parts = append(parts, "sizeGB"+strings.TrimSpace(m.SizeGB))
	}
	if len(parts) == 0 {
		return "(пусто)"
	}
	return strings.Join(parts, " ")
}

func operatingModeName(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "standard"
	}
	return mode
}

// placementRuleRef — ссылка на правило для логов/UI: "models[2]".
func placementRuleRef(kind string, index int) string {
	return kind + "[" + strconv.Itoa(index) + "]"
}
