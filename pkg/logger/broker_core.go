// broker_core.go — R390 (2026-11-02): каждое лог-событие попадает в брокер.
//
// ЖИВОЙ ДЕФЕКТ. В балансере брокер логов создавался (cmd/balancer/main.go) и
// WebSocket /ws/logs к нему подключался, но в буфер НИКТО НЕ ПИСАЛ: единственным
// источником были ручные вызовы `logger.Publish(...)`, которых в коде нет ни
// одного. Следствие: панель «Системные логи» в WebUI показывала только то, что
// сама же вкладка добавила через addLogEntry (эхо собственных сообщений), а
// новый экспорт /api/v1/logs/export отдавал пустой файл (X-Logs-Exported: 0).
//
// ЧТО ЗДЕСЬ. Обёртка над zapcore.Core: сначала публикует запись в брокер
// (level/message/source), затем делегирует обычному ядру (stdout). Так экспорт
// и live-tail видят РЕАЛЬНЫЕ логи балансера, а не пустоту.
package logger

import (
	"encoding/json"

	"go.uber.org/zap/zapcore"
)

// brokerCore — zapcore.Core с побочной публикацией в брокер.
type brokerCore struct {
	zapcore.Core
}

// With — сохранить обёртку при добавлении полей (иначе после первого .With()
// записи снова уходили бы мимо брокера).
func (c brokerCore) With(fields []zapcore.Field) zapcore.Core {
	return brokerCore{Core: c.Core.With(fields)}
}

// Check — решение «писать ли» принимает вложенное ядро (уровни, sampler).
func (c brokerCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !c.Enabled(e.Level) {
		return ce
	}
	return ce.AddCore(e, c)
}

// Write — публикация в брокер + обычная запись в stdout.
func (c brokerCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	if b := GetBroker(); b != nil {
		msg := e.Message
		if suffix := brokerFieldsSuffix(fields); suffix != "" {
			msg += " " + suffix
		}
		b.Publish(LogEntry{
			Time:    e.Time,
			Level:   brokerLevelName(e.Level),
			Message: msg,
			Source:  "balancer",
		})
	}
	return c.Core.Write(e, fields)
}

// brokerLevelName — уровень zap → строки контракта брокера
// ("debug" | "info" | "warn" | "error").
func brokerLevelName(l zapcore.Level) string {
	switch {
	case l <= zapcore.DebugLevel:
		return "debug"
	case l == zapcore.InfoLevel:
		return "info"
	case l == zapcore.WarnLevel:
		return "warn"
	default:
		return "error"
	}
}

// brokerFieldsSuffix — поля записи компактным JSON-суффиксом к сообщению.
//
// Зачем суффикс, а не отдельное поле: LogEntry контрактно содержит только
// level/message/source (его читает WebUI), а контекст (backend, model, error…)
// оператору нужен. Склеиваем в сообщение — формат строки экспорта остаётся
// «время, уровень, источник, сообщение».
func brokerFieldsSuffix(fields []zapcore.Field) string {
	if len(fields) == 0 {
		return ""
	}
	m := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		if f.Type == zapcore.SkipType || f.Key == "" {
			continue
		}
		switch {
		case f.Interface != nil:
			m[f.Key] = f.Interface
		case f.String != "":
			m[f.Key] = f.String
		case f.Type == zapcore.BoolType:
			m[f.Key] = f.Integer == 1
		default:
			m[f.Key] = f.Integer
		}
	}
	if len(m) == 0 {
		return ""
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(raw)
}
