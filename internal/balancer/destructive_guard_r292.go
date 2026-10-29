package balancer

// destructive_guard_r292.go — R292 (2026-10-29): «второй агент не имеет права
// вырвать модель у первого».
//
// ЖИВАЯ ЖАЛОБА (второй стенд, один хост, два агента, одна модель). Первый
// клиент работал нормально; как только параллельно запустили второго, первый
// получил `stream ended by upstream before completion`. Разбор показал, что
// guard R271 стоит только на ПУТЯХ ПЕРЕЗАГРУЗКИ (n_ctx-preflight, AutoTune,
// операторский reload), а «рядовой» путь загрузки/выгрузки модели —
// `ModelManager.ExecuteOperation` (load/unload), через который ходят
// автозагрузка по запросу клиента и API — его вообще не проверял. Для
// cppworker `/api/models/load` с другими параметрами = unload + load, то есть
// выгрузка модели ПОД живой генерацией чужого клиента.
//
// ПОЧЕМУ ЭТО СТАЛО ЧАСТЫМ ИМЕННО СЕЙЧАС. R281/R286 научили балансер
// перезагружать модель при ЛЮБОМ отличии параметров (окно, kv-cache, flash-attn,
// mmap, batch) — раньше такие запросы молча обслуживались на загруженных
// параметрах. То есть правка «правильно сравнивать настройки» увеличила частоту
// перезагрузок, и незащищённый путь стал стрелять на каждый второй агент.
//
// РЕШЕНИЕ. Один guard перед ЛЮБОЙ разрушительной операцией над моделью
// (load с другими параметрами = перезагрузка; unload). Он ждёт завершения чужих
// стримов короткий бюджет (`LB_DESTRUCTIVE_OP_WAIT_SEC`, по умолчанию 5 с) и,
// если стримы ещё идут, НЕ начинает операцию: возвращает типизированную
// «отложено»-ошибку. Верхний слой превращает её в 503 + Retry-After, а не в
// «model load failed» — клиент должен видеть «занято, повтори», а не «сломалось».
//
// Бюджет короткий намеренно: ждать 120 с внутри запроса второго клиента
// бессмысленно (он всё равно отвалится по своему таймауту), а отложить — дешево:
// повторный запрос после освобождения модели обслужится нормально.

import (
	"errors"
	"fmt"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// defaultDestructiveOpWaitSecR292 — сколько ждать чужих стримов перед
// загрузкой/выгрузкой модели по умолчанию.
const defaultDestructiveOpWaitSecR292 = 5

// errDestructiveOpDeferredR292 — sentinel: операция НЕ выполнена из-за живых
// стримов. Отдельный тип (а не строка), потому что вызывающие обязаны отличать
// «отложено» от «провалилось»: первое не пишется в circuit breaker, не
// публикует событие о сбое и не должно провоцировать failover на другой узел.
var errDestructiveOpDeferredR292 = errors.New("destructive model operation deferred: active client streams")

// destructiveDeferredErrorR292 — «отложено» с деталями для ответа клиенту.
type destructiveDeferredErrorR292 struct {
	BackendID string
	Model     string
	Streams   int
	RetrySec  int
}

func (e *destructiveDeferredErrorR292) Error() string {
	if e == nil {
		return errDestructiveOpDeferredR292.Error()
	}
	return fmt.Sprintf("%s: %d active client stream(s) on backend %s; model %q was not unloaded, retry in %ds",
		errDestructiveOpDeferredR292.Error(), e.Streams, e.BackendID, e.Model, e.RetrySec)
}

// Unwrap — чтобы errors.Is(err, errDestructiveOpDeferredR292) работал на любом
// уровне обёртки (handlers оборачивают ошибку текстом «auto-load failed»).
func (e *destructiveDeferredErrorR292) Unwrap() error { return errDestructiveOpDeferredR292 }

// destructiveOpWaitBudgetR292 — бюджет ожидания чужих стримов.
//
// R278: значение из реестра настроек (override из WebUI → env → дефолт), поэтому
// оператор может увеличить ожидание без перезапуска контейнера. Отрицательное
// значение трактуем как «дефолт» (а не «ждать вечно»): 0 — это «не ждать», и он
// остаётся явным решением оператора.
func destructiveOpWaitBudgetR292() time.Duration {
	if v, ok := settings.HotInt("LB_DESTRUCTIVE_OP_WAIT_SEC"); ok {
		if v < 0 {
			return defaultDestructiveOpWaitSecR292 * time.Second
		}
		return time.Duration(v) * time.Second
	}
	return defaultDestructiveOpWaitSecR292 * time.Second
}

// destructiveKindForModelOpR292 — как назвать операцию для классификатора
// обрыва: клиент, чей стрим всё-таки оборвался, должен получить ОДНУ из
// известных причин (`balancer_reload`/`balancer_unload`), а не «upstream».
func destructiveKindForModelOpR292(op string) string {
	if op == "unload" {
		return destructiveUnloadR271
	}
	return destructiveReloadR271
}

// guardModelOpR292 — можно ли сейчас выполнять разрушительную операцию.
//
// Возвращает nil, если стримов нет (или они успели завершиться за бюджет), и
// типизированную ошибку «отложено» иначе. Побочных эффектов нет: пометку
// разрушительной операции ставит вызывающий ТОЛЬКО после успешного guard'а —
// иначе (как было в R280) пометка «залипала» и следующий оборванный стрим
// обвинялся в том, чего не было.
func (p *Proxy) guardModelOpR292(backendID, model, op string) *destructiveDeferredErrorR292 {
	if p == nil || backendID == "" {
		return nil
	}
	wait := destructiveOpWaitBudgetR292()
	before := p.activeStreamsOnBackend(backendID)
	if before == 0 {
		return nil
	}
	if p.waitStreamsIdleR271(nil, backendID, model, wait) {
		logger.Get().Infow("R292: model operation allowed after streams finished",
			"backend", backendID, "model", model, "operation", op,
			"streams_before", before, "waited_sec", wait.Seconds())
		return nil
	}
	active := p.activeStreamsOnBackend(backendID)
	retry := int(wait.Seconds())
	if retry < 5 {
		retry = 5
	}
	logger.Get().Warnw("R292: model operation deferred — active client streams on backend",
		"backend", backendID, "model", model, "operation", op,
		"active_streams", active, "wait_sec", wait.Seconds(), "retry_after_sec", retry)
	return &destructiveDeferredErrorR292{
		BackendID: backendID,
		Model:     model,
		Streams:   active,
		RetrySec:  retry,
	}
}

// IsDestructiveDeferredR292 — отложено ли разрушительное действие (для
// вызывающих: не считать провалом, не открывать breaker, не делать failover).
func IsDestructiveDeferredR292(err error) bool {
	return err != nil && errors.Is(err, errDestructiveOpDeferredR292)
}

// RetryAfterOfDestructiveDeferredR292 — сколько секунд советовать клиенту.
func RetryAfterOfDestructiveDeferredR292(err error) (int, bool) {
	if !IsDestructiveDeferredR292(err) {
		return 0, false
	}
	var derr *destructiveDeferredErrorR292
	if errors.As(err, &derr) && derr.RetrySec > 0 {
		return derr.RetrySec, true
	}
	return 5, true
}

// autoLoadFailureTextR292 — текст отказа для клиента, который ЖДАЛ загрузку.
//
// Зачем отдельный текст. Раньше на этом месте был только
// «model %q is not loaded and auto-load failed: %v». Для отложенной операции это
// ложь в двух местах: (а) «failed» — ничего не провалилось, мы сознательно не
// стали выгружать модель под чужим стримом; (б) «not loaded» — модель как раз
// загружена и работает, просто занята. Оператор читал такой текст и шёл искать
// поломку загрузки, которой нет.
func autoLoadFailureTextR292(model string, loadErr error) string {
	if retry, ok := RetryAfterOfDestructiveDeferredR292(loadErr); ok {
		return fmt.Sprintf("model %q is busy serving another request (a live stream is using the backend); "+
			"the balancer did NOT unload it — retry in %ds", model, retry)
	}
	return fmt.Sprintf("model %q is not loaded and auto-load failed: %v", model, loadErr)
}
