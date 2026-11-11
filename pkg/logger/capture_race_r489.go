// capture_race_r489.go — R489 (2026-11-11): перехват логов в тестах должен быть
// потокобезопасным.
//
// ЖИВОЙ ДЕФЕКТ (красный CI, self-hosted Windows, -race): `logger.CaptureToBuffer`
// подменял глобальный логгер на zapcore с `zapcore.AddSync(*bytes.Buffer)`.
// `bytes.Buffer` не потокобезопасен, а в пакете balancer десятки тестов идут с
// `t.Parallel()` и пишут в лог одновременно. Детектор гонок печатал
// «WARNING: DATA RACE: Write at … by goroutine … Previous write … by goroutine …»
// и валил весь прогон (internal/... (-race)) — при том что проверяемый код был
// ни при чём: гонка жила в тестовой обвязке.
//
// ЧТО ЗДЕСЬ. Обёртка-writer с мьютексом: записи сериализуются, `Sync` — no-op
// (буферу синхронизировать нечего). Поведение тестов не меняется: они попрежнему
// читают накопленный текст из своего буфера.
package logger

import (
	"io"
	"sync"
)

// lockedWriterR489 — io.Writer с мьютексом и Sync под zapcore.WriteSyncer.
type lockedWriterR489 struct {
	w  io.Writer
	mu sync.Mutex
}

func (l *lockedWriterR489) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Sync — требование zapcore.WriteSyncer; у буфера/трубы синхронизировать нечего.
func (l *lockedWriterR489) Sync() error { return nil }

// newLockedWriterR489 — обернуть writer так, чтобы параллельные тесты не гонялись
// за один и тот же буфер.
func newLockedWriterR489(w io.Writer) io.Writer {
	if w == nil {
		return w
	}
	return &lockedWriterR489{w: w}
}
