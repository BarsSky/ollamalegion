// capture_race_r489_test.go — R489 (2026-11-11): перехват логов безопасен для
// параллельных тестов.
//
// ЖИВОЙ ДЕФЕКТ: `logger.CaptureToBuffer(&bytes.Buffer)` в тесте trace-строки
// ставил общий sink, а десятки тестов пакета идут с t.Parallel() и пишут в лог
// одновременно — `bytes.Buffer` не потокобезопасен, и `-race` валил прогон
// (internal/... (-race), self-hosted Windows). Проверяемый код был ни при чём.
package logger

import (
	"strings"
	"sync"
	"testing"
)

// TestR489_CaptureIsRaceFree — параллельные записи в перехваченный буфер.
//
// Тест имеет смысл именно под `-race` (так его и гоняет CI): без блокировки
// writer детектор гонок печатает WARNING: DATA RACE и прогон краснеет.
func TestR489_CaptureIsRaceFree(t *testing.T) {
	buf := NewCaptureBuffer()
	restore := CaptureToBuffer(buf)
	defer restore()

	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				Get().Infow("r489 capture race probe", "writer", n, "iter", j)
			}
		}(i)
	}
	wg.Wait()

	if buf.Len() == 0 {
		t.Fatal("буфер перехвата пуст: записи не дошли")
	}
	if !strings.Contains(buf.String(), "r489 capture race probe") {
		t.Fatal("в перехвате нет сообщения — sink не работает")
	}
	// Ни одна строка не должна быть разорвана: мьютекс сериализует записи целиком.
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Fatalf("запись разорвана параллельной записью: %q", line)
		}
	}
}

// TestR489_NilWriterDoesNotPanic — capture в nil не должен падать.
func TestR489_NilWriterDoesNotPanic(t *testing.T) {
	buf := NewCaptureBuffer()
	restore := CaptureTo(buf) // sanity: потокобезопасный буфер
	Get().Infow("r489 nil probe")
	restore()
	if buf.Len() == 0 {
		t.Fatal("обычный writer не получил записи")
	}
}
