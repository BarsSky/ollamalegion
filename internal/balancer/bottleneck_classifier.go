// bottleneck_classifier.go — R495 (2026-11-18), шаг S2: классификатор «во что уперлась».
//
// ЗАЧЕМ. До S2 балансер судил о занятости карты только по SM busy:
//
//	gpuFree := 100 - m.GPU.UsagePercent      // scoring.go:23-38
//
// На декоде каждый токен читает все веса + KV-cache: скорость упирается в полосу
// памяти, а SM при этом занят на 40–70 % (тензорные ядра ждут данные). Такая
// карта выглядела свободной и получала новые запросы, хотя пропускная способность
// не росла. S1 довёз числа (MemBusyPercent, throttle, PState, PCIe), S2 по ним
// выносит ВЕРДИКТ.
//
// ПОЧЕМУ ТОЛЬКО ДИАГНОЗ. По плану (§7) S2 — shadow: вердикт считается, пишется в
// лог и /api/metrics, но решения НЕ меняет. Влияние на выбор — S3, отдельный
// коммит, за флагом =on. Иначе нельзя отличить «мы сломали селектор» от «мы
// увидели правду».
package balancer

import (
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// BottleneckKindR495 — вердикт о том, во что упирается карта.
type BottleneckKindR495 string

const (
	// BottleneckMemoryR495 — декод уперся в полосу памяти: SM недогружен,
	// занятость памяти высокая, питание не на пределе.
	BottleneckMemoryR495 BottleneckKindR495 = "memory"
	// BottleneckPowerR495 — префилл уперся в TDP: SM занят, питание на пределе,
	// есть троттлинг по питанию/температуре.
	BottleneckPowerR495 BottleneckKindR495 = "power"
	// BottleneckComputeR495 — уперлись в тензорные ядра: SM занят, память нет.
	BottleneckComputeR495 BottleneckKindR495 = "compute"
	// BottleneckStallR495 — карта простаивает при живых запросах: ждёт сеть,
	// PCIe, RPC-узел или CPU. «Свободна» она только по счётчику.
	BottleneckStallR495 BottleneckKindR495 = "stall"
	// BottleneckIdleR495 — действительно свободна: нагрузки нет.
	BottleneckIdleR495 BottleneckKindR495 = "idle"
	// BottleneckUnknownR495 — данных нет (старый агент, N/A у драйвера).
	// НЕ «плохо»: поведение остаётся прежним.
	BottleneckUnknownR495 BottleneckKindR495 = "unknown"
)

// Пороги из §4.1 плана. Стартовые, калибруются замерами (§11 п.2) — поэтому
// вынесены в константы с именами, а не разбросаны по условиям.
const (
	bottleneckMemBusyHighR495 = 85.0 // занятость памяти «высокая», %
	bottleneckSMHighR495      = 90.0 // SM «занят», %
	bottleneckSMMemoryR495    = 70.0 // при memory-bound SM ниже этого
	bottleneckSMLowR495       = 40.0 // «простаивает», %
	bottleneckMemLowR495      = 40.0
	bottleneckPowerRatioR495  = 0.90 // питание «на пределе» — от 0.90
	bottleneckSMIdleR495      = 5.0  // «действительно свободна», %

	// bottleneckHysteresisKR495 — сколько подряд сборов обязаны подтвердить
	// вердикт, прежде чем он сменится (§4.1 п.1: на границе порога иначе дёргается
	// и score, и подсказки).
	bottleneckHysteresisKR495 = 2
)

// BottleneckModeR495 — режим, как у остальных швов проекта.
type BottleneckModeR495 string

const (
	// BottleneckModeOffR495 — прежнее поведение: ничего не считается.
	BottleneckModeOffR495 BottleneckModeR495 = "off"
	// BottleneckModeShadowR495 — считать и публиковать, решения не менять (дефолт S2).
	BottleneckModeShadowR495 BottleneckModeR495 = "shadow"
	// BottleneckModeOnR495 — влиять на выбор (S3; сейчас ведёт себя как shadow).
	BottleneckModeOnR495 BottleneckModeR495 = "on"
)

// bottleneckEnvR495 — имя флага.
const bottleneckEnvR495 = "LB_GPU_BOTTLENECK"

var (
	bottleneckModeOnceR495 sync.Once
	bottleneckModeR495     BottleneckModeR495
)

// bottleneckModeFromEnvR495 — режим из LB_GPU_BOTTLENECK.
//
// Дефолт S2 — shadow (§4.2 плана): диагноз виден, решения прежние.
// Неизвестное значение → shadow + ОДНО предупреждение (не молча: оператор должен
// знать, что его правка не применилась — тот же урок, что selection_order_r493).
func bottleneckModeFromEnvR495() BottleneckModeR495 {
	bottleneckModeOnceR495.Do(func() {
		raw := strings.ToLower(strings.TrimSpace(os.Getenv(bottleneckEnvR495)))
		switch raw {
		case "":
			bottleneckModeR495 = BottleneckModeShadowR495
		case "off", "0", "false":
			bottleneckModeR495 = BottleneckModeOffR495
		case "shadow":
			bottleneckModeR495 = BottleneckModeShadowR495
		case "on", "1", "true":
			bottleneckModeR495 = BottleneckModeOnR495
		default:
			bottleneckModeR495 = BottleneckModeShadowR495
			logger.Get().Warnw("LB_GPU_BOTTLENECK: неизвестное значение — включён shadow",
				"value", raw, "allowed", "off|shadow|on")
		}
	})
	return bottleneckModeR495
}

// resetBottleneckModeR495 — сброс for тестов (sync.Once иначе залипнет).
func resetBottleneckModeR495() {
	bottleneckModeOnceR495 = sync.Once{}
	bottleneckModeR495 = BottleneckModeShadowR495
}

// BottleneckInputR495 — входные данные правила. Только измеренные величины:
// «нет данных» выражается нулями/флагами, а не догадкой.
type BottleneckInputR495 struct {
	// SM — занятость SM (%), MemBusy — занятость интерфейса памяти (%).
	SM      float64
	MemBusy float64
	// MemBusyMeasured — известно ли MemBusy (0 у старого агента/N/A).
	MemBusyMeasured bool
	// PowerUsage/PowerLimit — ватты; PowerLimit == 0 = неизвестно.
	PowerUsage int
	PowerLimit int
	// ThrottleKnown/ThrottleMask — измерялись ли причины троттлинга.
	ThrottleKnown bool
	ThrottleMask  uint64
	// ActiveRequests — сколько запросов сейчас на бэкенде.
	ActiveRequests int
}

// powerRatioR495 — отношение питание/лимит; ok=false, если лимит неизвестен.
func powerRatioR495(in BottleneckInputR495) (float64, bool) {
	if in.PowerLimit <= 0 {
		return 0, false
	}
	r := float64(in.PowerUsage) / float64(in.PowerLimit)
	if r < 0 {
		r = 0
	}
	return r, true
}

// hasThrottleR495 — есть ли в маске троттлинг по питанию/температуре/brake.
// gpu_idle (1) и applications_clocks_setting (2) сюда НЕ входят: это не «медленно
// из-за предела», а простой/настройка.
func hasThrottleR495(mask uint64) bool {
	const powerBits = 4 | 8 | 64 | 128 // sw_power_cap | hw_slowdown | hw_thermal | hw_power_brake
	return mask&powerBits != 0
}

// classifyBottleneckR495 — ЧИСТАЯ функция правила §4.1. Без env, без состояния:
// гистерезис применяется вызывающим (см. bottleneckTrackerR495).
func classifyBottleneckR495(in BottleneckInputR495) BottleneckKindR495 {
	// Нет данных — «неизвестно», и это НЕ наказание: решение остаётся прежним.
	if !in.MemBusyMeasured && in.SM <= 0 {
		return BottleneckUnknownR495
	}

	powerRatio, powerKnown := powerRatioR495(in)
	throttled := in.ThrottleKnown && hasThrottleR495(in.ThrottleMask)

	// Питание: SM занят, питание на пределе, есть троттлинг предела.
	if in.SM >= bottleneckSMHighR495 && (throttled ||
		(powerKnown && powerRatio >= 0.95)) {
		return BottleneckPowerR495
	}

	// Память: карта занята, но SM недогружен — типичная картина декода.
	if in.MemBusyMeasured && in.MemBusy >= bottleneckMemBusyHighR495 &&
		in.SM <= bottleneckSMMemoryR495 &&
		(!powerKnown || powerRatio < bottleneckPowerRatioR495) {
		return BottleneckMemoryR495
	}

	// Вычисления: SM занят, память не на пределе, питание не на пределе.
	if in.SM >= bottleneckSMHighR495 && (!in.MemBusyMeasured || in.MemBusy <= 80) &&
		(!powerKnown || powerRatio < bottleneckPowerRatioR495) {
		return BottleneckComputeR495
	}

	// Простой при живых запросах: оба счётчика низкие, но работа есть. Карта не
	// «свободна» — она ждёт (сеть/PCIe/RPC/CPU), и отдавать ей новые запросы
	// бессмысленно.
	if in.ActiveRequests > 0 &&
		in.SM <= bottleneckSMLowR495 &&
		(!in.MemBusyMeasured || in.MemBusy <= bottleneckMemLowR495) {
		return BottleneckStallR495
	}

	// Действительно свободна: почти нулевая занятость и нет запросов.
	if in.SM <= bottleneckSMIdleR495 && in.ActiveRequests == 0 {
		return BottleneckIdleR495
	}

	// Ни одно правило не сработало (промежуточная зона): не выдумываем вердикт.
	return BottleneckUnknownR495
}

// BottleneckStateR495 — опубликованный вердикт по бэкенду.
type BottleneckStateR495 struct {
	Kind BottleneckKindR495 `json:"kind"`
	// Aggregated — вердикт получен агрегацией по картам хоста, а не по той карте,
	// где лежит модель (EffectiveGPUIndex неизвестен). В панели это должно быть
	// видно, иначе агрегат выглядит как факт по конкретной карте (§4.1 п.3).
	Aggregated bool   `json:"aggregated,omitempty"`
	Since      string `json:"since,omitempty"`
	// Advice — advisory-подсказка оператору по вердикту (S4, §5.3).
	// "increase_parallel" для memory-bound, "reduce_batch" для power-bound;
	// пусто для остальных. Это СОВЕТ: балансер ничего не применяет сам.
	Advice string `json:"advice,omitempty"`
	// Numbers — по каким числам принят вердикт (для лога и разбора).
	SM         float64 `json:"sm,omitempty"`
	MemBusy    float64 `json:"memBusy,omitempty"`
	PowerRatio float64 `json:"powerRatio,omitempty"`
	Active     int     `json:"active,omitempty"`
}

// bottleneckTrackerR495 — гистерезис: вердикт меняется после K подряд сборов.
type bottleneckTrackerR495 struct {
	mu      sync.Mutex
	current map[string]BottleneckStateR495 // backendID → опубликованный вердикт
	pending map[string]BottleneckKindR495  // backendID → кандидат
	streak  map[string]int                 // backendID → сколько подряд
}

func newBottleneckTrackerR495() *bottleneckTrackerR495 {
	return &bottleneckTrackerR495{
		current: make(map[string]BottleneckStateR495),
		pending: make(map[string]BottleneckKindR495),
		streak:  make(map[string]int),
	}
}

// update — применить один замер по бэкенду. Возвращает опубликованный вердикт и
// признак смены (для одной Info-строки на смену, §6).
func (t *bottleneckTrackerR495) update(backendID string, in BottleneckInputR495, aggregated bool, now time.Time) (BottleneckStateR495, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	candidate := classifyBottleneckR495(in)
	prev, known := t.current[backendID]

	// unknown не трогает гистерезис и не сбрасывает подтверждённый вердикт:
	// пропавшие данные не должны выглядеть как «стало хорошо».
	if candidate == BottleneckUnknownR495 {
		if !known {
			t.current[backendID] = BottleneckStateR495{
				Kind: BottleneckUnknownR495, Aggregated: aggregated, Since: now.UTC().Format(time.RFC3339),
			}
			return t.current[backendID], false
		}
		return prev, false
	}

	if pending, ok := t.pending[backendID]; ok && pending == candidate {
		t.streak[backendID]++
	} else {
		t.pending[backendID] = candidate
		t.streak[backendID] = 1
	}

	// Первый вердикт публикуется сразу (нечего подтверждать), смена — после K.
	if !known || prev.Kind == candidate {
		t.streak[backendID] = 0
		state := BottleneckStateR495{
			Kind:       candidate,
			Aggregated: aggregated,
			Since:      now.UTC().Format(time.RFC3339),
			SM:         in.SM,
			MemBusy:    in.MemBusy,
			Active:     in.ActiveRequests,
		}
		if r, ok := powerRatioR495(in); ok {
			state.PowerRatio = r
		}
		if known {
			state.Since = prev.Since
		}
		state.Advice = bottleneckAdviceR495(state.Kind)
		t.current[backendID] = state
		return state, false
	}

	if t.streak[backendID] < bottleneckHysteresisKR495 {
		// Кандидат ещё не подтверждён — оставляем прежний вердикт и его числа.
		return prev, false
	}

	t.streak[backendID] = 0
	state := BottleneckStateR495{
		Kind:       candidate,
		Aggregated: aggregated,
		Since:      now.UTC().Format(time.RFC3339),
		SM:         in.SM,
		MemBusy:    in.MemBusy,
		Active:     in.ActiveRequests,
	}
	if r, ok := powerRatioR495(in); ok {
		state.PowerRatio = r
	}
	state.Advice = bottleneckAdviceR495(state.Kind)
	t.current[backendID] = state
	return state, true
}

// snapshot — опубликованные вердикты для /api/metrics.
func (t *bottleneckTrackerR495) snapshot() map[string]BottleneckStateR495 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]BottleneckStateR495, len(t.current))
	for id, st := range t.current {
		out[id] = st
	}
	return out
}

// bottleneckInputFromMetricsR495 — собрать вход правила из снимка метрик бэкенда.
func bottleneckInputFromMetricsR495(gpu types.GPUMetrics, active int) BottleneckInputR495 {
	return BottleneckInputR495{
		SM:              gpu.UsagePercent,
		MemBusy:         gpu.MemBusyPercent,
		MemBusyMeasured: gpu.MemBusyPercent > 0,
		PowerUsage:      gpu.PowerUsage,
		PowerLimit:      gpu.PowerLimit,
		ThrottleKnown:   gpu.ThrottleKnown,
		ThrottleMask:    gpu.ThrottleReasons,
		ActiveRequests:  active,
	}
}

// String — режим для лога и публикации.
func (m BottleneckModeR495) String() string {
	switch m {
	case BottleneckModeOffR495:
		return "off"
	case BottleneckModeShadowR495:
		return "shadow"
	case BottleneckModeOnR495:
		return "on"
	}
	return string(m)
}

// Коды advisory-подсказок (R495 S4, §5.3 плана).
//
// ПОЧЕМУ КОДЫ, А НЕ ТЕКСТ. Подсказку показывают и лог, и панель; текст в коде
// означал бы третью копию формулировки рядом с i18n. Код переводится на стороне
// панели, а в JSON он читается одинаково на любом языке.
const (
	// BottleneckAdviceIncreaseParallelR495 — memory-bound: главный рычаг —
	// ПОДНЯТЬ parallel/слоты. При декоде веса читаются один раз на N токенов,
	// поэтому батчинг почти линейно поднимает агрегатный tok/s на одной карте.
	// Это противоположно интуиции «разгрузить карту», и именно поэтому подсказка
	// нужна: без неё оператор уменьшает нагрузку и получает хуже.
	BottleneckAdviceIncreaseParallelR495 = "increase_parallel"
	// BottleneckAdviceReduceBatchR495 — power-bound: рост батча упирается в TDP,
	// частота сбрасывается, время до первого токена растёт. Рычаг — уменьшить
	// n_ubatch/n_batch.
	BottleneckAdviceReduceBatchR495 = "reduce_batch"
)

// bottleneckAdviceR495 — подсказка по вердикту. Пустая строка = совета нет.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. Ответ «что делать» обязан быть один на все места
// публикации (лог, /api/metrics, панель). Прецедент R83-fix: «совет оператору,
// ничего не меняем сами» — автоприменение это отдельная задача после замеров.
func bottleneckAdviceR495(kind BottleneckKindR495) string {
	switch kind {
	case BottleneckMemoryR495:
		return BottleneckAdviceIncreaseParallelR495
	case BottleneckPowerR495:
		return BottleneckAdviceReduceBatchR495
	}
	// compute: упираемся в тензорные ядра — подсказки по конфигу нет.
	// stall: причина вне карты (сеть/PCIe/RPC/CPU) — совет зависел бы от того,
	// чего у нас нет в числах, поэтому молчим, а не советуем наугад.
	// idle/unknown: делать нечего или данных нет.
	return ""
}
