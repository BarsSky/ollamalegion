// cancel_upstream_payload_r360_test.go — R360 (2026-10-31), аудит abort A5.
//
// ЖИВОЙ ДЕФЕКТ. Балансер при обрыве клиента / таймауте / тишине upstream слал воркеру
// {"model": ...}. Воркер на такой приказ вызывает CancelByModel("", model) — то есть
// гасит ВСЕ генерации этой модели на узле, включая ЧУЖИЕ. Сценарий: два клиента работают
// с одной моделью, у одного отвалился браузер — и второму обрывает ответ; тот повторяет
// запрос, нагрузка растёт, оператор видит «сервер сам закрыл поток».
//
// R360: приказ адресный — {"request_id": ...} из заголовка X-Request-Id ответа воркера.
// Фолбэк на модель остаётся только когда id неизвестен, и это ФИКСИРУЕТСЯ (precise=false
// + предупреждение в логе), а не происходит молча.
//
// Проверяется:
//  1. известный request_id → приказ несёт request_id и НЕ несёт model (иначе воркер
//     снова погасит всех);
//  2. неизвестный request_id → фолбэк на model, признак precise=false;
//  3. ни id, ни модели → пустое тело (воркер отменит всё — админский сброс), precise=false;
//  4. model в фолбэке не теряется (старое поведение сохранено);
//  5. сторож: оба пути отмены (NDJSON-транспорт и hijack) обязаны передавать request_id.
package balancer

import (
	"os"
	"strings"
	"testing"
)

// TestR360_PayloadPrefersRequestID — адресная отмена вместо «все генерации модели».
func TestR360_PayloadPrefersRequestID(t *testing.T) {
	payload, precise := cancelUpstreamPayloadR360("gemma-4-E4B-it-Q4_K_M", "gen-12345")
	if !precise {
		t.Fatal("precise = false при известном request_id")
	}
	if payload["request_id"] != "gen-12345" {
		t.Fatalf("payload = %v, want request_id=gen-12345", payload)
	}
	if _, hasModel := payload["model"]; hasModel {
		t.Fatalf("payload содержит model вместе с request_id: воркер может предпочесть "+
			"модель и погасить чужие генерации (%v)", payload)
	}
}

// TestR360_PayloadFallsBackToModel — id неизвестен: прежнее поведение, но с признаком.
func TestR360_PayloadFallsBackToModel(t *testing.T) {
	payload, precise := cancelUpstreamPayloadR360("gemma-4-E4B-it-Q4_K_M", "")
	if precise {
		t.Fatal("precise = true без request_id — отмена на самом деле по модели")
	}
	if payload["model"] != "gemma-4-E4B-it-Q4_K_M" {
		t.Fatalf("payload = %v, want model (без него отмена не найдёт генерацию)", payload)
	}
	if _, hasID := payload["request_id"]; hasID {
		t.Fatalf("payload содержит пустой request_id: воркер посчитает приказ адресным (%v)", payload)
	}
}

// TestR360_PayloadEmptyIsBroadcast — ни модели, ни id: админский сброс (как раньше).
func TestR360_PayloadEmptyIsBroadcast(t *testing.T) {
	payload, precise := cancelUpstreamPayloadR360("", "")
	if precise {
		t.Error("precise = true при пустом приказе")
	}
	if len(payload) != 0 {
		t.Fatalf("payload = %v, want пустое тело (отменить всё)", payload)
	}
}

// TestR360_CancelPathsPassRequestID_STEWARD — сторож на оба пути отмены.
//
// Смысл: правило «отмена адресная» живёт в одной функции, но ВЫЗОВЫ в двух файлах
// (NDJSON-транспорт и hijack-путь /v1). Новая точка отмены, забывшая request_id,
// снова начнёт гасить чужие генерации — молча.
func TestR360_CancelPathsPassRequestID_STEWARD(t *testing.T) {
	files := []string{"llamacpp_transport.go", "proxy_request_hijack.go"}
	for _, name := range files {
		var src string
		for _, p := range []string{name, "internal/balancer/" + name} {
			if b, err := os.ReadFile(p); err == nil {
				src = string(b)
				break
			}
		}
		if src == "" {
			t.Fatalf("не удалось прочитать %s", name)
		}
		idx := 0
		calls := 0
		for {
			i := strings.Index(src[idx:], "cancelUpstreamGeneration(")
			if i < 0 {
				break
			}
			calls++
			lineEnd := strings.Index(src[idx+i:], "\n")
			if lineEnd < 0 {
				lineEnd = len(src) - idx - i
			}
			line := src[idx+i : idx+i+lineEnd]
			// Аргументы: backendID, model, requestID, reason — четыре, не три.
			if strings.Count(line, ",") < 3 {
				t.Errorf("%s: вызов без request_id: %s — отмена погасит ВСЕ генерации модели",
					name, strings.TrimSpace(line))
			}
			idx += i + lineEnd
		}
		if calls == 0 {
			t.Errorf("%s: не найдено ни одного вызова cancelUpstreamGeneration — "+
				"если путь переименован, обновите сторожа", name)
		}
	}
}
