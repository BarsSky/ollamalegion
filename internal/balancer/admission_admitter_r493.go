// admission_admitter_r493.go — R493-12p (2026-11-13, этап 4 плана «два мозга»):
// ОДИН ВХОД В ОЧЕРЕДЬ (флаг LB_UNIFIED_ADMISSION).
//
// ЖИВОЙ ФАКТ, ради которого это существует. Ожиданий два, и они не знают друг о
// друге:
//
//  1. единая admission-очередь (`waitForInferenceBackend`) — generic-путь: FIFO с
//     сессиями, backpressure, бюджет max(LB_ADMISSION_WAIT_SEC,
//     LB_AUTO_LOAD_WAIT_SEC), лизинг слота, X-Queue-*, R460-причины;
//  2. очередь ожидания ПОЯВЛЕНИЯ бэкенда (R403, `waitBackendR403`) — путь
//     обработчиков llama.cpp: своя FIFO по модели, свой бюджет
//     (LB_BACKEND_WAIT_SEC), опрос селектора, без лизинга и без сессий.
//
// Следствия, которые видит оператор: запрос на `/v1/chat/completions` ждёт
// 120 c, запрос на `/api/chat` — до 900 c (там бюджет включает ожидание
// автозагрузки); ожидающие на `/v1` не видны в очереди панели; причины отказа
// (перегрузка/таймаут/выключено) у путей разные.
//
// ЧТО ЗДЕСЬ. Интерфейс `Admitter` — единственный вход «дай бэкенд или скажи,
// почему нет» — и ДВЕ реализации, которые уже существуют: unified (эталон) и
// probe (R403). Отличие пути становится ВЫБОРОМ реализации, а не вторым набором
// правил ожидания, заголовков и бюджета.
//
// ФЛАГ `LB_UNIFIED_ADMISSION` (горячая настройка, группа `queue`):
//
//	0 — историческое поведение (по умолчанию): путь обработчиков ждёт появления
//	    бэкенда своей очередью, generic-путь — единой.
//	1 — путь обработчиков входит в ЕДИНУЮ очередь: один бюджет, одна
//	    справедливость, одна видимость в панели.
//
// Режима «тень» здесь НЕТ ОСОЗНАННО, в отличие от решения об окне (R493-12n):
// ожидание нельзя «посчитать в тени» — оно занимает слот и меняет состояние.
// Поэтому переключение проверяется тестами (одинаковый исход у обеих реализаций
// на одинаковой ситуации) и стендом.
package balancer

import (
	"context"
	"errors"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/types"
)

// Ошибки Admitter. Те же, что у admission-очереди, где это возможно: вызывающий
// код уже умеет их различать (выключено / перегрузка / истекло).
var (
	// errAdmitterNoProbe — реализации «ждать появления бэкенда» нужен probe, а его
	// не передали. Это ошибка вызывающего, а не клиента.
	errAdmitterNoProbe = errors.New("admitter: probe is required for the probe-wait mechanism")
)

// admitRequestR493 — что просят и как искать.
type admitRequestR493 struct {
	// Probe — как проверить, появился ли подходящий бэкенд (только для механизма
	// «ждать появления бэкенда»). nil — механизм берёт слот из общей очереди.
	Probe         func() string
	Model         string
	Type          types.BackendType
	Session       string
	RequestedNCtx int
}

// admitOutcomeR493 — чем закончилось. Один набор полей на все механизмы: иначе
// пути снова начнут описывать ожидание по-разному (тело 503, заголовки, лог).
type admitOutcomeR493 struct {
	// Lease — освободить занятый слот (nil — слот не занимали).
	Lease     func()
	BackendID string
	Waited    time.Duration
	Position  int
	// Budget — фактический предел ожидания, который применялся (для заголовка
	// X-Queue-Wait-Max-Sec и тела: «сколько ждали» и «сколько вообще ждать» —
	// разные числа, и раньше второе на одном из путей не сообщалось вовсе).
	Budget time.Duration
	// Queued — запрос действительно стоял в очереди (а не получил слот сразу).
	Queued bool
	Err    error
}

// Admitter — единственный вход в очередь.
type Admitter interface {
	// Admit — дать бэкенд (и слот) либо назвать причину отказа.
	Admit(ctx context.Context, req admitRequestR493) admitOutcomeR493
	// Name — машинное имя механизма: попадает в лог, чтобы живой разбор не
	// требовал догадок о том, какая очередь обслужила запрос.
	Name() string
}

// unifiedAdmitterR493 — единая admission-очередь (эталон, generic-путь).
type unifiedAdmitterR493 struct {
	p *Proxy
}

func (a unifiedAdmitterR493) Name() string { return "unified-admission" }

// Admit — ждёт слот в единой очереди.
//
// Бюджет и все ограничения (image-лок, перегрузка) живут внутри очереди: здесь
// их не дублируем, иначе «сколько ждать» снова разойдётся с тем, что реально
// применилось.
func (a unifiedAdmitterR493) Admit(ctx context.Context, req admitRequestR493) admitOutcomeR493 {
	if a.p == nil {
		return admitOutcomeR493{Err: errAdmissionDisabled}
	}
	id, release, waited, position, err := a.p.waitForInferenceBackend(ctx, req.Model, req.Type, req.Session)
	out := admitOutcomeR493{
		BackendID: id,
		Lease:     release,
		Waited:    waited,
		Position:  position,
		Budget:    a.p.admissionWaitBudget(),
		Queued:    waited > 0,
		Err:       err,
	}
	return out
}

// probeAdmitterR493 — ожидание ПОЯВЛЕНИЯ бэкенда (R403): своя FIFO по модели и
// свой бюджет. Исторический механизм пути обработчиков.
type probeAdmitterR493 struct {
	queue   *backendWaitQueue
	timeout time.Duration
}

func (a probeAdmitterR493) Name() string { return "probe-wait" }

func (a probeAdmitterR493) Admit(ctx context.Context, req admitRequestR493) admitOutcomeR493 {
	out := admitOutcomeR493{Budget: a.timeout}
	if req.Probe == nil {
		out.Err = errAdmitterNoProbe
		return out
	}
	// timeout <= 0 — «не ждать»: один быстрый probe и выход. Прежнее поведение
	// сохраняется, чтобы выключение ожидания не превращалось в вечное 503.
	if a.timeout <= 0 {
		if id := req.Probe(); id != "" {
			out.BackendID = id
			return out
		}
		out.Err = errAdmissionDisabled
		return out
	}
	queue := a.queue
	if queue == nil {
		queue = llmBackendWaitQueue
	}
	id, waited, position, ok := waitBackendR403(ctx, queue, req.Model, a.timeout, req.Probe)
	out.BackendID, out.Waited, out.Position = id, waited, position
	// «Стоял в очереди» — это РЕАЛЬНОЕ ожидание, а не «time.Since(start) > 0».
	//
	// ЖИВОЙ ФАКТ (поймал CI, ubuntu + -race): очередь проверяет probe сразу после
	// постановки, поэтому при свободном бэкенде waited — микросекунды. Правило
	// `waited > 0` объявляло такой запрос ждавшим: клиент получал
	// X-Queue-Wait-Ms: 0 и строку «дождался» в лог на ровном месте. На Windows
	// (грубые часы) тот же тест был зелёным — проверка была ещё и платформенной.
	// Настоящее ожидание всегда не меньше шага опроса очереди.
	out.Queued = admitterQueuedR493(out.Waited)
	if !ok {
		if ctx != nil && ctx.Err() != nil {
			out.Err = ctx.Err()
			return out
		}
		out.Err = errAdmissionTimeout
	}
	return out
}

// admitterForR493 — какой механизм обслуживает ЭТОТ путь.
//
// Выбор один на все пути: либо единая очередь (умеет и слоты, и ожидание
// загрузки), либо исторический механизм «ждать появления бэкенда» — и только
// когда его явно попросили (Probe != nil) и флаг не включён.
func (p *Proxy) admitterForR493(req admitRequestR493) Admitter {
	if admissionUnifiedR493() || req.Probe == nil {
		return unifiedAdmitterR493{p: p}
	}
	return probeAdmitterR493{queue: llmBackendWaitQueue, timeout: backendWaitTimeoutR403()}
}

// admissionUnifiedR493 — действует ли единый вход в очередь.
//
// Один разбор значения (settings: панель → env → умолчание главного реестра),
// как и у флага окна: второго парсера быть не должно.
func admissionUnifiedR493() bool {
	v, ok := settings.HotInt("LB_UNIFIED_ADMISSION")
	return ok && v == 1
}

// admitterQueuedR493 — «запрос действительно стоял в очереди».
//
// ПОЧЕМУ НЕ `waited > 0`. Очередь проверяет probe сразу после постановки, поэтому
// при свободном бэкенде waited — микросекунды (на Windows часто ровно 0). Правило
// `waited > 0` объявляло такой запрос ждавшим: клиент получал X-Queue-Wait-Ms: 0 и
// строку «дождался» в лог на ровном месте. Живой факт: CI (ubuntu + -race) уронил
// именно это, а локально тест был зелёным — проверка была платформенной.
//
// Настоящее ожидание не меньше шага опроса очереди (backendWaitPoll). Функция
// вынесена отдельно, чтобы правило проверялось числами без часов.
func admitterQueuedR493(waited time.Duration) bool {
	return waited >= backendWaitPoll
}

// admitOutcomeLogFieldsR493 — поля ожидания для лога: одна форма на обе
// реализации. Без этого «почему /v1 ждал 120 с, а /api/chat 900» снова
// разбиралось бы по исходникам.
func admitOutcomeLogFieldsR493(a Admitter, out admitOutcomeR493) []interface{} {
	name := "unknown"
	if a != nil {
		name = a.Name()
	}
	errText := ""
	if out.Err != nil {
		errText = out.Err.Error()
	}
	return []interface{}{
		"mechanism", name,
		"backend", out.BackendID,
		"waited_ms", out.Waited.Milliseconds(),
		"position", out.Position,
		"wait_max_sec", int(out.Budget.Seconds()),
		"queued", out.Queued,
		"error", errText,
	}
}
