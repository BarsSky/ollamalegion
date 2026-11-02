package logger

// broker_core_test.go — R390 (2026-11-02): логгер обязан кормить брокер.
//
// Живой дефект: брокер создавался, /ws/logs подключался, а в буфер никто не
// писал — панель логов и экспорт отдавали пустоту. Здесь фиксируем контракт:
// каждое событие zapCore попадает в брокер с уровнем, сообщением и полями.

import (
	"io"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestR390_LoggerFeedsBroker — событие zap доходит до брокера.
func TestR390_LoggerFeedsBroker(t *testing.T) {
	prev := GetBroker()
	broker := NewLogBroker(16)
	SetBroker(broker)
	t.Cleanup(func() { SetBroker(prev) })

	// Ядро обязано быть ВКЛЮЧЕНО (NopCore выключен, и тогда Check не пропускает
	// запись — именно это первая версия теста и поймала).
	inner := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(io.Discard),
		zap.DebugLevel,
	)
	logger := zap.New(brokerCore{Core: inner}).Sugar()
	logger.Warnw("узел раскладки молчит", "backend", "donor-1", "stalled_sec", 12)

	snapshot := broker.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("в брокере %d записей, ожидалась 1 (логгер не кормит брокер?)", len(snapshot))
	}
	e := snapshot[0]
	if e.Level != "warn" {
		t.Errorf("level = %q, ожидался warn", e.Level)
	}
	if !strings.Contains(e.Message, "узел раскладки молчит") {
		t.Errorf("сообщение потерялось: %q", e.Message)
	}
	// Поля записи обязаны доехать контекстом — иначе в выгрузке нет backend/stalled_sec.
	if !strings.Contains(e.Message, `"backend":"donor-1"`) || !strings.Contains(e.Message, `"stalled_sec":12`) {
		t.Errorf("поля не попали в сообщение: %q", e.Message)
	}
	if e.Source != "balancer" {
		t.Errorf("source = %q, ожидался balancer", e.Source)
	}
}

// TestR390_LevelsMappedToContract — уровни приводятся к ожидаемым строкам.
func TestR390_LevelsMappedToContract(t *testing.T) {
	cases := map[zapcore.Level]string{
		zapcore.DebugLevel:  "debug",
		zapcore.InfoLevel:   "info",
		zapcore.WarnLevel:   "warn",
		zapcore.ErrorLevel:  "error",
		zapcore.DPanicLevel: "error",
	}
	for lvl, want := range cases {
		if got := brokerLevelName(lvl); got != want {
			t.Errorf("brokerLevelName(%v) = %q, ожидалось %q", lvl, got, want)
		}
	}
}

// TestR390_NilBrokerNoPanic — брокера нет (тесты/лёгкая сборка): запись не падает.
func TestR390_NilBrokerNoPanic(t *testing.T) {
	prev := GetBroker()
	SetBroker(nil)
	t.Cleanup(func() { SetBroker(prev) })

	core := brokerCore{Core: zapcore.NewNopCore()}
	logger := zap.New(core).Sugar()
	logger.Info("без брокера")
}
