// diag_log_fields_r493_test.go — R493 (2026-11-12, по архиву стенда):
// в balancer-logs.txt терялись структурированные поля.
//
// ЖИВОЙ ФАКТ. В присланном архиве (Qwen3.8-27B, A10 ×2) каждая строка лога была
// вида «2026-11-12T11:08:56.469Z INFO balancer R493: свободный узел пропущен —
// модель не влезает в свободную VRAM» — БЕЗ backend/model/need_mb/free_mb, а
// «request trace» — без request_id/path/status/duration_ms. Разбор по такому
// архиву невозможен: числа, ради которых строки и пишутся, отсутствуют.
//
// Причина: diagBundleBalancerLogsSinceR476 печатал только e.Message, хотя поля
// лежат в e.Fields. Тест фиксирует, что поля попадают в файл и что значение со
// пробелом не ломает разбор строки.
package api

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// TestR493_BalancerLogsCarryFields — поля записи попадают в файл архива.
func TestR493_BalancerLogsCarryFields(t *testing.T) {
	broker := logger.NewLogBroker(50)
	logger.SetBroker(broker)
	defer logger.SetBroker(nil)
	broker.Publish(logger.LogEntry{
		Time:    time.Now(),
		Level:   "info",
		Message: "R493: свободный узел пропущен — модель не влезает",
		Fields: map[string]interface{}{
			"backend": "A10-126",
			"need_mb": 21504,
			"free_mb": 2745,
			"note":    "модель не влезает в свободную VRAM",
		},
	})

	text := diagBundleBalancerLogsSinceR476(time.Now().Add(-time.Minute))
	for _, want := range []string{"backend=A10-126", "need_mb=21504", "free_mb=2745"} {
		if !strings.Contains(text, want) {
			t.Fatalf("в архиве нет поля %q — оператор снова не увидит чисел.\nлог:\n%s", want, text)
		}
	}
	// Значение с пробелами обязано быть в кавычках, иначе строка «поедет».
	if !strings.Contains(text, `note="модель не влезает в свободную VRAM"`) {
		t.Fatalf("значение с пробелами не закавычено — разбор строки сломается.\nлог:\n%s", text)
	}
}

// TestR493_LogFieldsOrderIsStable — порядок ключей стабильный (сортировка):
// иначе diff между двумя архивами нечитаем.
func TestR493_LogFieldsOrderIsStable(t *testing.T) {
	broker := logger.NewLogBroker(50)
	logger.SetBroker(broker)
	defer logger.SetBroker(nil)
	broker.Publish(logger.LogEntry{
		Time:    time.Now(),
		Level:   "warn",
		Message: "order-probe",
		Fields:  map[string]interface{}{"z": 1, "a": 2, "m": 3},
	})
	text := diagBundleBalancerLogsSinceR476(time.Now().Add(-time.Minute))
	var line string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "order-probe") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("строка не найдена в архиве:\n%s", text)
	}
	if !(strings.Index(line, "a=2") < strings.Index(line, "m=3") &&
		strings.Index(line, "m=3") < strings.Index(line, "z=1")) {
		t.Fatalf("поля не отсортированы — diff архивов нечитаем: %q", line)
	}
}
