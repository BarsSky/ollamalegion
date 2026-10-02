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
// TestR83_LoadContext_NoDeadlineByDefault — R83/v67 (2026-10-02).
//
// БЫЛО (до v67): тест требовал НАЛИЧИЯ дедлайна у контекста загрузки — дефолт
// 30 минут. Но этот дедлайн и есть duration-кап на работу: на медленном диске
// 15-гигабайтная модель грузится дольше, контекст отменялся, и bridge
// возвращал BRIDGE_ERR_ABORTED («load cancelled») при полностью здоровой
// загрузке.
//
// СТАЛО: по умолчанию дедлайна НЕТ — загрузка живёт до терминального
// состояния. Кап возможен только явным opt-in
// CPPWORKER_LAZY_LOAD_TIMEOUT_SEC (проверяется следующим тестом).
func TestR83_LoadContext_NoDeadlineByDefault(t *testing.T) {
	t.Setenv("CPPWORKER_LAZY_LOAD_TIMEOUT_SEC", "")

	loadCtx, cancelLoad := loadContextForSharedLoad()
	defer cancelLoad()

	if deadline, ok := loadCtx.Deadline(); ok {
		t.Fatalf("у контекста загрузки есть дедлайн %v — это duration-кап на работу: "+
			"загрузка большой модели будет прервана таймером, а не терминальным состоянием",
			time.Until(deadline))
	}
	if err := loadCtx.Err(); err != nil {
		t.Fatalf("контекст загрузки уже отменён: %v", err)
	}
}

// TestR83_LoadContext_OptInDeadline — оператор может осознанно взвести кап.
func TestR83_LoadContext_OptInDeadline(t *testing.T) {
	t.Setenv("CPPWORKER_LAZY_LOAD_TIMEOUT_SEC", "120")

	if got := lazyLoadDetachedTimeout(); got != 120*time.Second {
		t.Fatalf("lazyLoadDetachedTimeout() = %v, want 120s (явный opt-in)", got)
	}
	loadCtx, cancelLoad := loadContextForSharedLoad()
	defer cancelLoad()

	deadline, ok := loadCtx.Deadline()
	if !ok {
		t.Fatal("при взведённом CPPWORKER_LAZY_LOAD_TIMEOUT_SEC дедлайн обязан быть")
	}
	if d := time.Until(deadline); d <= 0 || d > 3*time.Minute {
		t.Fatalf("неожиданный дедлайн: %v", d)
	}
}
