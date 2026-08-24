// Package logger — глобальный sugared zap logger с thread-safe init.
//
// Round 52.2 (2026-08-24): заменён `if log != nil` double-checked locking
// на atomic.Pointer[zap.SugaredLogger] для устранения DATA RACE в -race
// режиме. Старый код (Round 31-32 era) делал:
//
//	var log *zap.SugaredLogger  // глобал
//	func Get() *zap.SugaredLogger {
//	    if log != nil {       // ← read БЕЗ atomic load
//	        return log
//	    }
//	    once.Do(func() {
//	        log = zap.NewNop().Sugar()  // ← write
//	    })
//	    return log
//	}
//
// Race detector ловил concurrent read/write на `log` между goroutine,
// читающей через `Get()` (например, в background poll llama.cpp бэкендов),
// и goroutine, пишущей в `log` из `Init()` или `once.Do`.
//
// atomic.Pointer.Load/Store — lock-free thread-safe reads,
// подходит для hot path в каждом лог-запросе.
package logger

import (
	"sync/atomic"

	"go.uber.org/zap"
)

// logPtr — глобальный atomic.Pointer на текущий SugaredLogger.
// nil = не инициализирован (тесты, lazy Nop init при Get).
var logPtr atomic.Pointer[zap.SugaredLogger]

// Get возвращает глобальный SugaredLogger.
// Если Init() уже вызван — возвращает real logger.
// Если Init() не вызывался — лениво инициализирует Nop-логгер (для тестов).
//
// Потокобезопасен: использует atomic.Pointer.Load для hot path и
// atomic.Pointer.CompareAndSwap для slow path (первая инициализация).
func Get() *zap.SugaredLogger {
	if p := logPtr.Load(); p != nil {
		return p
	}
	// Slow path: ещё не инициализирован. Создаём Nop (для тестов).
	np := zap.NewNop().Sugar()
	// CompareAndSwap: если несколько goroutine одновременно пришли сюда,
	// только первая запишет; остальные прочитают её значение. Гарантирует
	// что мы вернём ОДИН и тот же logger во всех goroutine.
	if logPtr.CompareAndSwap(nil, np) {
		return np
	}
	// Другая goroutine уже инициализировала. Берём её значение.
	return logPtr.Load()
}

// Init инициализирует глобальный логгер zap с заданным уровнем.
// После вызова Init() все последующие Get() возвращают этот logger.
// Можно вызвать повторно (например, в тестах для смены уровня) —
// atomic.Pointer.Store атомарно заменит старый logger на новый.
func Init(level string) {
	var cfg zap.Config
	cfg = zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stdout"}
	cfg.DisableStacktrace = true
	cfg.EncoderConfig.TimeKey = "time"
	cfg.EncoderConfig.LevelKey = "level"
	cfg.EncoderConfig.MessageKey = "msg"
	cfg.EncoderConfig.CallerKey = "caller"

	switch level {
	case "debug":
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		cfg.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	l, err := cfg.Build()
	if err != nil {
		l = zap.NewNop()
	}
	logPtr.Store(l.Sugar())
}

// Sync вызывает sync для сброса буферов.
func Sync() {
	if p := logPtr.Load(); p != nil {
		_ = p.Sync()
	}
}
