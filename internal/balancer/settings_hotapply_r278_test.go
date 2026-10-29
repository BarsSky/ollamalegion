package balancer

// settings_hotapply_r278_test.go — R278 (этап 2 аудита настроек): «горячая»
// настройка обязана влиять на ПОВЕДЕНИЕ, а не только на ответ API.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Самая вероятная форма полуправки: реестр есть, API
// отвечает «updated», а геттер по-прежнему читает os.Getenv — то есть
// переключатель в WebUI «работает» только на бумаге. Поэтому проверяем именно
// геттеры, которые используются в горячем пути.

import (
	"testing"
	"time"

	"ollama-loadbalancer/internal/settings"
)

// withSettingsRegistry — свежий глобальный реестр на время теста.
func withSettingsRegistry(t *testing.T) *settings.Registry {
	t.Helper()
	reg := settings.Init("")
	t.Cleanup(func() {
		// Возвращаем пустой реестр: иначе override-ы протекут в другие тесты
		// пакета, которые проверяют дефолты.
		_ = reg.ResetAll()
		settings.Init("")
	})
	return reg
}

func TestR278_HotApplyStreamLimit(t *testing.T) {
	reg := withSettingsRegistry(t)

	if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
		t.Fatalf("до override: %d, want default %d", got, streamMaxLineBytesDefault)
	}
	if _, err := reg.Set("LB_STREAM_MAX_LINE_BYTES", 2<<20); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := streamMaxLineBytes(); got != 2<<20 {
		t.Fatalf("после override: %d, want %d (hot-apply не работает)", got, 2<<20)
	}
	if err := reg.Reset("LB_STREAM_MAX_LINE_BYTES"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
		t.Fatalf("после reset: %d, want default %d", got, streamMaxLineBytesDefault)
	}
}

func TestR278_HotApplyKeepaliveAndAdmission(t *testing.T) {
	reg := withSettingsRegistry(t)

	if _, err := reg.Set("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC", 3); err != nil {
		t.Fatalf("Set keepalive: %v", err)
	}
	if got := autoStreamKeepaliveInterval(); got != 3*time.Second {
		t.Fatalf("keepalive: %s, want 3s", got)
	}

	if _, err := reg.Set("LB_ADMISSION_WAIT_SEC", 7); err != nil {
		t.Fatalf("Set admission: %v", err)
	}
	if got := AdmissionWaitTimeout(); got != 7*time.Second {
		t.Fatalf("admission: %s, want 7s", got)
	}

	// Смысл отрицательного значения сохраняется и через реестр: «ждать до
	// отмены, но не дольше 30 минут».
	if _, err := reg.Set("LB_ADMISSION_WAIT_SEC", -1); err != nil {
		t.Fatalf("Set admission(-1): %v", err)
	}
	if got := AdmissionWaitTimeout(); got != 30*time.Minute {
		t.Fatalf("admission(-1): %s, want 30m", got)
	}

	// Отключение ожидания: 0 — валидное значение и означает «не ждать».
	if _, err := reg.Set("LB_ADMISSION_WAIT_SEC", 0); err != nil {
		t.Fatalf("Set admission(0): %v", err)
	}
	if got := AdmissionWaitTimeout(); got != 0 {
		t.Fatalf("admission(0): %s, want 0", got)
	}
}

func TestR278_HotApplyToolBufferAndReloadDefer(t *testing.T) {
	reg := withSettingsRegistry(t)

	if _, err := reg.Set("LB_TOOL_CALL_BUFFER_BYTES", 256*1024); err != nil {
		t.Fatalf("Set tool buffer: %v", err)
	}
	if got := toolCallBufferLimit(); got != 256*1024 {
		t.Fatalf("tool buffer: %d, want %d", got, 256*1024)
	}

	if _, err := reg.Set("LB_RELOAD_DEFER_MAX_SEC", 15); err != nil {
		t.Fatalf("Set reload defer: %v", err)
	}
	if got := reloadDeferMaxWaitR271(); got != 15*time.Second {
		t.Fatalf("reload defer: %s, want 15s", got)
	}
}

// TestR278_HotApplyAutoStreamLimits — пределы буферизации non-stream ответа
// тоже должны применяться на лету: именно они решают, «доживёт» ли non-stream
// клиент до конца генерации.
func TestR278_HotApplyAutoStreamLimits(t *testing.T) {
	reg := withSettingsRegistry(t)

	if _, err := reg.Set("LB_OPENAI_AUTO_STREAM_MAX_SEC", 42); err != nil {
		t.Fatalf("Set max sec: %v", err)
	}
	if _, err := reg.Set("LB_OPENAI_AUTO_STREAM_MAX_BYTES", 1<<20); err != nil {
		t.Fatalf("Set max bytes: %v", err)
	}
	maxWait, maxBytes := autoStreamLimits()
	if maxWait != 42*time.Second {
		t.Fatalf("max sec: %s, want 42s", maxWait)
	}
	if maxBytes != 1<<20 {
		t.Fatalf("max bytes: %d, want %d", maxBytes, 1<<20)
	}
}
