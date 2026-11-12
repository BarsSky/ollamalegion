// load_duration_r493.go — R493-12a (2026-11-12): СКОЛЬКО ЗАПРОС ОТДАЛ НА
// ЗАГРУЗКУ МОДЕЛИ.
//
// ЖИВОЙ ЗАМЕР на стенде (Qwen3-4B, модель выгружена, затем /api/chat через
// балансер):
//
//	клиент: первый кадр 40.12 с, всего 41.49 с
//	load_duration        = 0.00 с   ← эти 40 секунд потеряны
//	prompt_eval_duration = 0.14 с
//	eval_duration        = 1.23 с
//	total_duration       = 1.37 с
//
// ПРИЧИНА. Ленивая загрузка (`ensureModelLoadedWithNCtx`) выполняется ВНУТРИ
// запроса и синхронно — 40 с ожидания и есть загрузка модели. Но статистика
// считается от `prefillStartTime`, который берётся ПОСЛЕ загрузки, поэтому в
// кадре этих секунд нет вовсе. Балансер со своей стороны их тоже не видит:
// запрос уже отправлен воркеру (client.Do) и ждёт ответа.
//
// РЕШЕНИЕ. Воркер — единственный, кто знает эту длительность, поэтому он её
// измеряет и кладёт в done-кадр (`load_duration`, наносекунды — как в Ollama).
// Балансер при правке нативного кадра складывает её со своим ожиданием в
// очереди/preflight (см. applyStatsWindowsNativeR493).
//
// ПОЧЕМУ ЧЕРЕЗ КОНТЕКСТ, А НЕ ЧЕРЕЗ ВОЗВРАЩАЕМОЕ ЗНАЧЕНИЕ. Загрузка происходит
// в разных функциях (handleChat, runGenerateCore, …), а кадр собирается в
// третьих; менять их сигнатуры значило бы тронуть контракт вызовов, который
// сторожат свои тесты. Контекст запроса — единственное место, доступное всем.
// Значение пишется через указатель, поэтому передаётся и в те подфункции, что
// получают уже производный контекст.
package main

import (
	"context"
	"sync/atomic"
	"time"
)

// loadScopeKeyR493 — ключ контекста (свой тип: чужих значений не задеваем).
type loadScopeKeyR493 struct{}

// loadScopeR493 — общая на запрос ячейка «сколько ушло на загрузку модели».
// atomic: загрузку может завершить другая горутина (waitForModelReady), а кадр
// собирает обработчик.
type loadScopeR493 struct {
	ns atomic.Int64
}

// markLoadR493 — записать длительность загрузки (0 и отрицательные игнорируются).
func (s *loadScopeR493) markLoadR493(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.ns.Store(d.Nanoseconds())
}

// withLoadScopeR493 — положить ячейку в контекст запроса (идемпотентно: если
// ячейка уже есть, возвращается тот же контекст).
func withLoadScopeR493(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	if _, ok := ctx.Value(loadScopeKeyR493{}).(*loadScopeR493); ok {
		return ctx
	}
	return context.WithValue(ctx, loadScopeKeyR493{}, &loadScopeR493{})
}

// loadScopeFromCtxR493 — ячейка из контекста (nil, если обработчик её не создал).
func loadScopeFromCtxR493(ctx context.Context) *loadScopeR493 {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(loadScopeKeyR493{}).(*loadScopeR493)
	return s
}

// loadDurationNsR493 — измеренная длительность загрузки в наносекундах
// (0 — модель уже была загружена или обработчик не ставил ячейку).
func loadDurationNsR493(ctx context.Context) int64 {
	if s := loadScopeFromCtxR493(ctx); s != nil {
		return s.ns.Load()
	}
	return 0
}

// loadModelMeasuredR493 — обёртка над ensureModelLoadedWithNCtx: измеряет загрузку
// и пишет её в ячейку контекста. Возвращает тот же контракт, что и исходная
// функция (имя модели, ошибка) — сигнатуры вызывающих не меняются.
func loadModelMeasuredR493(ctx context.Context, modelName string, requestedNCtx int) (string, error) {
	start := time.Now()
	actual, err := ensureModelLoadedWithNCtx(ctx, modelName, requestedNCtx)
	if err != nil {
		return actual, err
	}
	loadScopeFromCtxR493(ctx).markLoadR493(time.Since(start))
	return actual, nil
}
