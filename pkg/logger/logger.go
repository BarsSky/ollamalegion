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
	"io"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	// R390 (2026-11-02): каждое событие дополнительно публикуем в брокер логов
	// (живой tail /ws/logs и выгрузка /api/v1/logs/export). Без этой обёртки
	// брокер оставался пустым: в него писали только ручные logger.Publish(...),
	// которых в коде не было.
	l = l.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return brokerCore{Core: c}
	}))
	logPtr.Store(l.Sugar())
}

// Sync вызывает sync для сброса буферов.
func Sync() {
	if p := logPtr.Load(); p != nil {
		_ = p.Sync()
	}
}

// CaptureTo перенаправляет пакетный логгер в w и возвращает функцию
// восстановления предыдущего логгера.
//
// Назначение: тесты, которым нужно утверждать, что код действительно
// залогировал событие (раньше такие тесты опирались на несуществующий
// LOG_BUFFER-хук и молча ничего не проверяли).
//
// Реализация: строим zap-логгер с тем же encoder-ом, что и Init (json,
// time/level/msg/caller), но с AddSync(w) вместо stdout, и подменяем
// глобальный указатель. Уровень — DebugLevel, чтобы тест видел все записи.
//
// Не безопасно вызывать параллельно с другими тестами, которые логируют:
// подмена глобальная. Вызывающий тест обязан быть последовательным
// (в cmd/cppworker тесты не помечены t.Parallel()).
func CaptureTo(w io.Writer) (restore func()) {
	prev := logPtr.Load()

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "time"
	encCfg.LevelKey = "level"
	encCfg.MessageKey = "msg"
	encCfg.CallerKey = "caller"

	// R489 (2026-11-11): writer оборачивается в мьютекс. Без этого перехват логов
	// в тестах ставил *bytes.Buffer, а параллельные тесты писали в него
	// одновременно — детектор гонок валил прогон (internal/... (-race)).
	// R489.1 (2026-11-11): если передан уже потокобезопасный CaptureBuffer, второй
	// раз не оборачиваем — его мьютекс покрывает и запись, и ЧТЕНИЕ (String), а
	// иначе гонка переезжает с «писатель-писатель» на «писатель-читатель».
	sink := w
	if _, ok := w.(*CaptureBuffer); !ok {
		sink = newLockedWriterR489(w)
	}
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encCfg),
		zapcore.AddSync(sink),
		zap.DebugLevel,
	)
	logPtr.Store(zap.New(core, zap.AddStacktrace(zapcore.FatalLevel)).Sugar())

	return func() {
		logPtr.Store(prev)
	}
}

// CaptureToBuffer — удобная обёртка над CaptureTo.
//
// R489.1: принимает любой io.Writer, а не только *bytes.Buffer: тестам нужен
// CaptureBuffer (запись И чтение под одним мьютексом). *bytes.Buffer тоже
// подходит — но тогда вызывающий обязан сам не читать буфер, пока другие
// горутины пишут в перехват.
func CaptureToBuffer(w io.Writer) (restore func()) {
	return CaptureTo(w)
}
