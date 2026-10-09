// async_work_tracker.go — R91 (2026-10-09): учёт длительных фоновых работ
// cppworker (загрузка и перезагрузка модели) и ожидание их перед закрытием backend.
//
// ЗАЧЕМ. `runAsyncLoad` (handlers_model.go:306,756) и `runAsyncReload`
// (handlers_model.go:2157) — это ФОНОВЫЕ горутины, которые держат backend
// минутами: дренаж in-flight запросов, unload, load с новыми параметрами, при
// ошибке — rollback. Они не были отслеживаемыми, поэтому `Backend.Close()` мог
// выполниться прямо во время их работы. `go test -race ./cmd/cppworker/` это
// ловил (CI-шаг «Test (cmd packages, -race)» на self-hosted Windows):
//
//	Write at 0x… by goroutine 8:
//	  cppbackend.(*Backend).Close()                  internal/cppbackend/backend.go:2894
//	  cmd/cppworker.setupCppWorkerTestServer.func1() cmd/cppworker/main_ollama_api_test.go:50
//	Previous read at 0x… by goroutine 34:
//	  cppbackend.(*Backend).GetModel()               internal/cppbackend/backend.go:1955
//	  cmd/cppworker.runAsyncReload()                 cmd/cppworker/handlers_model_async.go:471
//
// Теперь каждая такая горутина регистрируется в asyncWorkWG, а перед закрытием
// backend'а (graceful shutdown в main.go и cleanup тестового сервера) вызывающий
// ждёт её завершения. Ожидание ОГРАНИЧЕНО по времени: ждать вечно нельзя —
// оператор останавливает процесс и во время загрузки большой модели. Таймаут
// настраивается LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC (default 30, 0 = не ждать).
package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// asyncWorkWG — счётчик активных фоновых работ (load/reload модели).
var asyncWorkWG sync.WaitGroup

// defaultAsyncWorkWaitSec — сколько ждать завершения фоновой работы при закрытии.
const defaultAsyncWorkWaitSec = 30

// trackAsyncWork — зарегистрировать фоновую работу; возвращает функцию для defer.
//
// Использование:
//
//	done := trackAsyncWork()
//	go func() {
//		defer done()
//		runAsyncReload(...)
//	}()
func trackAsyncWork() func() {
	asyncWorkWG.Add(1)
	return asyncWorkWG.Done
}

// asyncWorkWaitTimeout — сколько секунд ждать фоновые работы при закрытии.
//
// LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC:
//   - пусто        → defaultAsyncWorkWaitSec (30 с);
//   - 0 или меньше → не ждать вовсе (прежнее поведение, гонка возможна);
//   - не число     → default + WARN.
func asyncWorkWaitTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC"))
	if v == "" {
		return defaultAsyncWorkWaitSec * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC is not a number, using default",
			"value", v, "default_sec", defaultAsyncWorkWaitSec)
		return defaultAsyncWorkWaitSec * time.Second
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// WaitAsyncWork — дождаться завершения фоновых load/reload.
//
// Возвращает false, если за timeout они не завершились: тогда вызывающий всё равно
// закрывает backend (остановка процесса не должна зависеть от загрузки модели), но
// факт пишется в лог — иначе «cppworker не выходит по SIGTERM» выглядело бы
// загадкой.
func WaitAsyncWork(timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	done := make(chan struct{})
	go func() {
		asyncWorkWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitAsyncWorkBeforeClose — общий шаг для graceful shutdown и cleanup тестов:
// ждём фоновые работы (с таймаутом из окружения) и пишем WARN, если не дождались.
func waitAsyncWorkBeforeClose() {
	timeout := asyncWorkWaitTimeout()
	if timeout <= 0 {
		return
	}
	if !WaitAsyncWork(timeout) {
		logger.Get().Warnw("async model work did not finish before backend close — "+
			"closing anyway (increase LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC to wait longer)",
			"timeout_sec", int(timeout.Seconds()))
	}
}
