// capacity.go — R501 (2026-11-19), аудит S3: ОДНО правило вместимости.
//
// ЗАЧЕМ. Вместимость бэкенда сегодня приходит из пяти источников:
// MaxConcurrentReqs (конфиг), RuntimeMaxConcurrentRequests (оператор),
// RuntimeModelSlots (факт модели), NodeClaimedCapacity (заявка узла) и
// RuntimeCapacityFromNode (признак «это не оператор, а нода»). Отсюда два
// живых дефекта (§1.1 C-1/C-6 аудита):
//
//  1. заявка узла, посчитанная ОДИН раз при старте агента, стояла в резолвере
//     выше операторского лимита и замораживалась до рестарта агента: оператор
//     ставил «1», а узел продолжал получать «4» (и наоборот);
//  2. `reconcileNodeCapacity` объявлял операторский лимит «от ноды», как только
//     значения совпадали, после чего заявка узла перетирала его.
//
// ЧТО ДЕЛАЕТ ЭТОТ ФАЙЛ. Вводит (а) одну трактовку чисел (ноль = «не задано»,
// отрицательное = «без лимита», положительное = значение) и (б) резолвер, в
// котором ОПЕРАТОР СТОИТ ВЫШЕ ЗАЯВКИ УЗЛА. Включение — осознанное: по умолчанию
// режим `shadow` (новый резолвер считается, расхождения пишутся в лог, решения
// остаются прежними), `on` переключает решения, `off` — прежнее поведение.
package types

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// CapacityModeR501 — режим единого источника вместимости.
type CapacityModeR501 int

const (
	// CapacityModeOffR501 — прежнее поведение (исторический резолвер).
	CapacityModeOffR501 CapacityModeR501 = iota
	// CapacityModeShadowR501 — считать новое правило, писать расхождения, не менять решения.
	CapacityModeShadowR501
	// CapacityModeOnR501 — решения принимает единый резолвер.
	CapacityModeOnR501
)

// String — для логов и публикации.
func (m CapacityModeR501) String() string {
	switch m {
	case CapacityModeOffR501:
		return "off"
	case CapacityModeShadowR501:
		return "shadow"
	case CapacityModeOnR501:
		return "on"
	}
	return "unknown"
}

// capacityModeEnvR501 — имя флага.
const capacityModeEnvR501 = "LB_CAPACITY_SINGLE_SOURCE"

var (
	capacityModeOnceR501 sync.Once
	capacityModeR501     CapacityModeR501
)

// CapacitySingleSourceModeR501 — режим из LB_CAPACITY_SINGLE_SOURCE.
//
// Дефолт — shadow: диагноз виден, решения прежние (§4.2 плана R495, тот же приём).
// Неизвестное значение → shadow + ОДНО предупреждение (молча игнорировать
// настройку нельзя — оператор должен знать, что его правка не применилась).
func CapacitySingleSourceModeR501() CapacityModeR501 {
	capacityModeOnceR501.Do(func() {
		raw := strings.ToLower(strings.TrimSpace(os.Getenv(capacityModeEnvR501)))
		switch raw {
		case "off", "0", "false":
			capacityModeR501 = CapacityModeOffR501
		case "", "shadow":
			capacityModeR501 = CapacityModeShadowR501
		case "on", "1", "true":
			capacityModeR501 = CapacityModeOnR501
		default:
			capacityModeR501 = CapacityModeShadowR501
			logger.Get().Warnw("LB_CAPACITY_SINGLE_SOURCE: неизвестное значение — включён shadow",
				"value", raw, "allowed", "off|shadow|on")
		}
	})
	return capacityModeR501
}

// ResetCapacitySingleSourceModeR501R501 — сброс для тестов (sync.Once иначе залипнет).
func ResetCapacitySingleSourceModeR501R501() {
	capacityModeOnceR501 = sync.Once{}
	capacityModeR501 = CapacityModeOffR501
}

// CapacityValueR501 — одна трактовка числа вместимости.
type CapacityValueR501 struct {
	// Value — значение лимита (имеет смысл только при Known).
	Value int
	// Known — число вообще задано (0 = «не задано», а не «ноль слотов»).
	Known bool
	// Unlimited — отрицательное значение: «без лимита» (документировано в
	// handlers_backends.go как -1).
	Unlimited bool
}

// CapacityFromValueR501 — ЕДИНСТВЕННАЯ трактовка числа вместимости (§1.1 C-3).
//
// До этой правки ноль читался пятью способами: «без лимита» (slot_manager),
// «слотов нет» (backend_selector), «лимит 1» (availability_first) и «10» (UI).
// Правило:
//
//	n > 0  → Known, Value=n
//	n == 0 → не задано (Known=false): вызывающий применяет дефолт своего типа
//	n < 0  → Unlimited=true (Known=true): «без лимита», а не «слотов нет»
func CapacityFromValueR501(n int) CapacityValueR501 {
	switch {
	case n > 0:
		return CapacityValueR501{Value: n, Known: true}
	case n < 0:
		return CapacityValueR501{Unlimited: true, Known: true}
	default:
		return CapacityValueR501{}
	}
}

// ResolveEffectiveCapacityV2R501 — единый резолвер: оператор выше заявки узла.
//
// Порядок (и почему именно такой):
//  1. модель загружена (modelSlots > 0) — ФАКТ: оператор может только понизить
//     (R83), превысить физические слоты нельзя;
//  2. операторский лимит (operatorMax > 0) — решение человека;
//  3. заявка узла (nodeClaim > 0) — посчитана ОДИН раз при старте агента и
//     замерзает, поэтому стоит НИЖЕ оператора (аудит C-1);
//  4. статика конфига (staticMax) — последний резерв.
//
// Unlimited («без лимита») означает «не ограничиваем этим источником»: если
// оператор поставил -1, заявка узла больше не урезает его до n_parallel.
func ResolveEffectiveCapacityV2R501(modelSlots, operatorMax, nodeClaim, staticMax int) int {
	v, _ := ResolveEffectiveCapacityV2WithSourceR501(modelSlots, operatorMax, nodeClaim, staticMax)
	return v
}

// ResolveEffectiveCapacityV2WithSourceR501 — то же решение И его источник.
//
// Источник считается ЗДЕСЬ, а не отдельной функцией по полям бэкенда: в прошлой
// версии R501 источник вычислялся своей веткой и разошёлся с решением при
// «без лимита» у оператора. Один расчёт — одна правда (аудит C-7).
func ResolveEffectiveCapacityV2WithSourceR501(modelSlots, operatorMax, nodeClaim, staticMax int) (int, string) {
	slots := CapacityFromValueR501(modelSlots)
	oper := CapacityFromValueR501(operatorMax)
	node := CapacityFromValueR501(nodeClaim)
	static := CapacityFromValueR501(staticMax)

	if slots.Known && !slots.Unlimited {
		if oper.Known && !oper.Unlimited && oper.Value <= slots.Value {
			return oper.Value, CapacitySourceOperator
		}
		return slots.Value, CapacitySourceModelSlots
	}
	if oper.Known {
		if oper.Unlimited {
			// «Без лимита»: если есть заявка узла — она и есть верхняя граница
			// (физика важнее формулировки), иначе не ограничиваем.
			if node.Known && !node.Unlimited {
				return node.Value, CapacitySourceNode
			}
			return 0, CapacitySourceOperator
		}
		return oper.Value, CapacitySourceOperator
	}
	if node.Known && !node.Unlimited {
		return node.Value, CapacitySourceNode
	}
	if static.Known && !static.Unlimited {
		return static.Value, CapacitySourceStatic
	}
	return 0, CapacitySourceStatic
}

// capacityNodeClaimTTLEnvR501 — «пометка возраста» заявки узла (аудит C-1).
const capacityNodeClaimTTLEnvR501 = "LB_CAPACITY_NODE_CLAIM_TTL_SEC"

// capacityNodeClaimTTLDefaultR501 — 15 минут. Heartbeat агента приходит раз в
// несколько секунд, регистрация — раз в 30 с, так что на живом стенде заявка
// обновляется задолго до истечения. Значение выбрано так, чтобы «мёртвая»
// запись (агент выключен, контейнер удалён) перестала влиять на решения, но
// ручной рестарт агента с новым n_parallel успевал перерегистрироваться.
const capacityNodeClaimTTLDefaultR501 = 15 * time.Minute

var (
	capacityNodeClaimTTLOnceR501 sync.Once
	capacityNodeClaimTTLR501     time.Duration
)

// CapacityNodeClaimTTLR501 — TTL заявки узла из LB_CAPACITY_NODE_CLAIM_TTL_SEC.
//
// 0 (или отрицательное) — возраст не проверять: заявка участвует всегда, как до
// R501. Это явный выход для стендов, где heartbeat не настроен.
func CapacityNodeClaimTTLR501() time.Duration {
	capacityNodeClaimTTLOnceR501.Do(func() {
		raw := strings.TrimSpace(os.Getenv(capacityNodeClaimTTLEnvR501))
		if raw == "" {
			capacityNodeClaimTTLR501 = capacityNodeClaimTTLDefaultR501
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			capacityNodeClaimTTLR501 = capacityNodeClaimTTLDefaultR501
			logger.Get().Warnw("LB_CAPACITY_NODE_CLAIM_TTL_SEC: некорректное значение — взят дефолт",
				"value", raw, "default", capacityNodeClaimTTLDefaultR501.String())
			return
		}
		capacityNodeClaimTTLR501 = time.Duration(n) * time.Second
	})
	return capacityNodeClaimTTLR501
}

// ResetCapacityNodeClaimTTLR501 — сброс для тестов (sync.Once иначе залипнет).
func ResetCapacityNodeClaimTTLR501() {
	capacityNodeClaimTTLOnceR501 = sync.Once{}
	capacityNodeClaimTTLR501 = 0
}

// NodeClaimFreshR501 — заявку узла можно считать актуальной.
//
// Отметка времени ОБЯЗАТЕЛЬНА для новой семантики: nil означает «возраст
// неизвестен» (запись из старой сборки). Такую заявку считаем свежей, но
// LoadState сразу проставляет ей время записи state.json — то есть возраст
// отсчитывается от последнего подтверждения, а не от вечности.
func NodeClaimFreshR501(claimedAt *time.Time, now time.Time) bool {
	if claimedAt == nil {
		return true
	}
	ttl := CapacityNodeClaimTTLR501()
	if ttl <= 0 {
		return true
	}
	return now.Sub(*claimedAt) <= ttl
}
