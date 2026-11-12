// debug_bundle_fields_r493_test.go — R493-12a-dump (2026-11-12, по архиву стенда):
// архив ВОРКЕРА обязан содержать структурированные поля, а не только текст.
//
// ЖИВОЙ ФАКТ. В присланном архиве (стенд A10 ×2, 2026-11-12T14:51) в
// `backends/<id>.bundle/logs.txt` строки выглядели так:
//
//	R482: удержание вывода снято — ход снова потоковый
//	R83 гейт n_ctx: тип KV-cache из плана загрузки
//	/api/chat stream response summary
//
// Ни `held_ms/held_bytes`, ни `kv_cache_type`, ни `generated_tokens` — то есть
// именно те числа, ради которых архив и собирают. Причина: дамп писал только
// `LogEntry.Message`. Тест держит поля в файле и требует стабильный формат.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

func TestR493_WorkerBundleLogsCarryFields(t *testing.T) {
	b := logger.NewLogBroker(100)
	logger.SetBroker(b)
	defer logger.SetBroker(nil)
	logger.SetBrokerSource("cppworker")

	b.Publish(logger.LogEntry{Level: "warn", Source: "cppworker",
		Message: "R482: удержание вывода снято — ход снова потоковый",
		Fields: map[string]interface{}{
			"held_ms":    int64(20061),
			"held_bytes": int64(1137),
			"model":      "Qwen3.8-27B-UD-Q4_K_M",
		}})
	b.Publish(logger.LogEntry{Level: "info", Source: "cppworker",
		Message: "/api/chat stream response summary",
		Fields: map[string]interface{}{
			"generated_tokens": int64(812),
			"thinking_len":     int64(4096),
			"done_reason":      "stop",
			"request_id":       "reqabc123",
		}})

	rec := httptest.NewRecorder()
	handleDebugBundle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	logs := bundleEntriesR476(t, rec.Body.Bytes())["logs.txt"]

	for _, want := range []string{
		"held_ms=20061", "held_bytes=1137", "model=Qwen3.8-27B-UD-Q4_K_M",
		"generated_tokens=812", "thinking_len=4096", "done_reason=stop", "request_id=reqabc123",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("в logs.txt нет %q — числа снова потеряны:\n%s", want, logs)
		}
	}
	// Текст сообщения обязан остаться: поля дополняют, а не заменяют.
	if !strings.Contains(logs, "R482: удержание вывода снято") {
		t.Fatalf("сообщение потерялось при печати полей:\n%s", logs)
	}
	// Стабильный порядок ключей: backend/held_bytes/held_ms/model — по алфавиту,
	// иначе diff между архивами нечитаем.
	idxBytes := strings.Index(logs, "held_bytes=")
	idxMs := strings.Index(logs, "held_ms=")
	if idxBytes < 0 || idxMs < 0 || idxBytes > idxMs {
		t.Fatalf("поля напечатаны не в алфавитном порядке (held_bytes=%d, held_ms=%d):\n%s",
			idxBytes, idxMs, logs)
	}
}
