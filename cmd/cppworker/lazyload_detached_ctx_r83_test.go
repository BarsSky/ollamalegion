// lazyload_detached_ctx_r83_test.go — R83-fix (2026-09-29).
//
// ДЕФЕКТ (воспроизведён на живом стенде). Загрузка модели — разделяемый ресурс:
// при параллельных запросах к незагруженной модели грузит одна горутина
// (TryLockLoad), остальные ждут её. Но контекст загрузки брался от клиента,
// который её инициировал, поэтому завершение ЕГО запроса убивало общую загрузку:
//
//	load cancelled: model load aborted by user (R60.57 checkpoint A:
//	after llama_model_load_from_file): BRIDGE_ERR_ABORTED
//
// Наблюдалось как «первый кто отправил получил ответ, второй получил ошибку 503
// и то что модель не загрузить».
//
// Предыдущая «починка» проверяла `ctx.Done() != nil` — но это истина ровно для
// CANCELLABLE контекста, то есть для клиентского r.Context(); ветка отвязки
// срабатывала только для context.Background() (у него Done() == nil) и НЕ
// срабатывала в том самом случае, от которого защищала. Тесты ниже фиксируют
// правильное свойство: контекст загрузки не зависит от клиентского.
package main

import (
	"context"
	"testing"
	"time"
)

// TestR83_LoadContext_DetachedFromCaller — отменённый клиентский запрос не
// отменяет контекст разделяемой загрузки.
func TestR83_LoadContext_DetachedFromCaller(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	cancelCaller() // клиент уже ушёл

	loadCtx, cancelLoad := loadContextForSharedLoad()
	defer cancelLoad()

	if err := loadCtx.Err(); err != nil {
		t.Fatalf("контекст загрузки отменён вместе с клиентским запросом: %v — "+
			"именно это даёт 503 «model load aborted by user» остальным клиентам", err)
	}
	if caller.Err() == nil {
		t.Fatal("тест сломан: клиентский контекст должен быть отменён")
	}
	// Контексты обязаны быть разными объектами, а не одним и тем же.
	if loadCtx == caller {
		t.Error("контекст загрузки — это тот же объект, что клиентский контекст")
	}
}

// TestR83_LoadContext_NotKilledByCallerAfterStart — отмена клиента ПОСЛЕ начала
// загрузки тоже не должна её прерывать.
func TestR83_LoadContext_NotKilledByCallerAfterStart(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	loadCtx, cancelLoad := loadContextForSharedLoad()
	defer cancelLoad()

	cancelCaller()
	if caller.Err() == nil {
		t.Fatal("тест сломан: клиентский контекст должен быть отменён")
	}
	select {
	case <-loadCtx.Done():
		t.Fatalf("загрузка прервана уходом клиента: %v", loadCtx.Err())
	case <-time.After(50 * time.Millisecond):
		// ожидаемо: контекст загрузки жив
	}
}

// TestR83_LoadContext_HasDeadline — «повисшая» загрузка не держит ресурс вечно:
// контекст обязан быть ограничен по времени.
func TestR83_LoadContext_HasDeadline(t *testing.T) {
	loadCtx, cancelLoad := loadContextForSharedLoad()
	defer cancelLoad()

	deadline, ok := loadCtx.Deadline()
	if !ok {
		t.Fatal("у контекста загрузки нет дедлайна — зависшая загрузка останется навсегда")
	}
	d := time.Until(deadline)
	if d <= 0 {
		t.Fatalf("дедлайн уже прошёл: %v", d)
	}
	if d > 24*time.Hour {
		t.Errorf("дедлайн неразумно далеко: %v", d)
	}
}
