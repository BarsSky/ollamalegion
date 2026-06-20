package logger

import (
	"sync"

	"go.uber.org/zap"
)

var (
	log  *zap.SugaredLogger
	once sync.Once
)

// Get возвращает глобальный SugaredLogger.
// Если Init() уже вызван — возвращает real logger.
// Если Init() не вызывался — лениво инициализирует Nop-логгер (тесты/sync.Once).
func Get() *zap.SugaredLogger {
	if log != nil {
		// Init() уже был вызван — real logger, не перезаписываем через once.Do
		return log
	}
	once.Do(func() {
		// fallback: Nop-логгер если Init не вызывался (тесты)
		log = zap.NewNop().Sugar()
	})
	return log
}

// Init инициализирует глобальный логгер zap с заданным уровнем
func Init(level string) {
	var cfg zap.Config
	cfg = zap.NewProductionConfig()
	// Явно направляем логи в stdout (Docker ожидает логи в stdout, а не stderr)
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
	log = l.Sugar()
}

// Sync вызывает sync для сброса буферов
func Sync() {
	if log != nil {
		_ = log.Sync()
	}
}