//go:build llama_stub

package balancer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	
	"ollama-loadbalancer/internal/settings"
)

// R548 (T-4 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// ЖИВОЙ ДЕФЕКТ. На пути «клиент просит non-stream, воркер отдаёт SSE, балансер
// копит ответ» стоял предел длительности 300 с ПО УМОЛЧАНИЮ, и при его
// превышении ответ обрывался ошибкой `auto_stream_timeout`. Живая арифметика
// того же стенда: 27B на A10 — единицы токенов в секунду, ответ на 8k токенов
// идёт минутами, а клиенты (Cline и подобные) рвут соединение на ~480 с. То есть
// балансер ошибался РАНЬШЕ клиента, да ещё и «своей» ошибкой вместо ответа.
//
// Плюс доктрина: таймаут — осознанное решение оператора, а не дефолт
// (docs/architecture/request-lifecycle.ru.md). Поэтому:
//   - предел длительности по умолчанию ВЫКЛЮЧЕН (0);
//   - от зависшего воркера защищает сторож ТИШИНЫ (R297), который теперь стоит и
//     на этом пути: тишина ≠ работа, и она объясняется отдельным кодом ошибки.
//
// Второй кап из T-4 — `LB_STREAM_STALL_ABORT_SEC` (300 с тишины посреди стрима) —
// оставлен дефолтом ОСОЗНАННО и проверен ниже тестом: он не рубит работу, он
// рубит молчание (0 = выключить).

// TestR548_DurationLimitIsOffByDefault — предела длительности по умолчанию нет,
// а явная настройка работает.
func TestR548_DurationLimitIsOffByDefault(t *testing.T) {
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "")
	maxWait, maxBytes := autoStreamLimits()
	if maxWait != 0 {
		t.Fatalf("предел длительности по умолчанию = %v, ожидалось 0 (таймаут — только осознанно): "+
			"300 с обрывали законную генерацию 27B раньше, чем клиент рвёт соединение", maxWait)
	}
	if maxBytes <= 0 {
		t.Fatalf("предел объёма по умолчанию = %d, ожидался положительный (защита памяти)", maxBytes)
	}

	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "120")
	if got, _ := autoStreamLimits(); got != 120*time.Second {
		t.Fatalf("явный LB_OPENAI_AUTO_STREAM_MAX_SEC=120 → %v, ожидалось 2m", got)
	}
}

// TestR548_SingleSourceForDefault — дефолт живёт в ОДНОМ месте (реестр настроек),
// а константа балансера обязана с ним совпадать.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ПРОВЕРКА. Ровно здесь аудит и нашёл свою болезнь: дефолт 300 с
// был записан ДВАЖДЫ — константой в балансере и `Default: 300` в реестре. Читается
// реестр (HotInt отдаёт значение всегда), поэтому правка одной константы ничего не
// меняла, а оператор видел прежние 300 с. Тест не даёт разойтись снова.
func TestR548_SingleSourceForDefault(t *testing.T) {
	// «Действующее значение» без Init() и без override/env — ровно дефолт
	// встроенного описания (settings.HotInt): так его читает и сам балансер.
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "")
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_BYTES", "")

	gotSec, ok := settings.HotInt("LB_OPENAI_AUTO_STREAM_MAX_SEC")
	if !ok {
		t.Fatal("нет записи LB_OPENAI_AUTO_STREAM_MAX_SEC в реестре настроек")
	}
	if gotSec != int64(defaultAutoStreamMaxSec) {
		t.Fatalf("дефолт реестра = %d, константа балансера = %d — два источника одной величины "+
			"(читается реестр, константа мертва)", gotSec, defaultAutoStreamMaxSec)
	}
	gotBytes, ok := settings.HotInt("LB_OPENAI_AUTO_STREAM_MAX_BYTES")
	if !ok {
		t.Fatal("нет записи LB_OPENAI_AUTO_STREAM_MAX_BYTES в реестре настроек")
	}
	if gotBytes != int64(defaultAutoStreamMaxBytes) {
		t.Fatalf("дефолт объёма в реестре = %d, константа балансера = %d", gotBytes, defaultAutoStreamMaxBytes)
	}
}

// TestR548_SilentUpstreamEndsWithStallError — воркер замолчал посреди ответа:
// запрос завершается понятной ошибкой (auto_stream_stalled), а НЕ висит вечно и
// НЕ ждёт 300-секундного предела.
func TestR548_SilentUpstreamEndsWithStallError(t *testing.T) {
	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "1")   // тишина посреди стрима
	t.Setenv("LB_STREAM_FIRST_FRAME_ABORT_SEC", "") // prefill не трогаем
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "")

	var wg sync.WaitGroup
	wg.Add(1)
	stalled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Один законный кадр — дальше тишина (воркер подвис).
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"привет\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		close(stalled)
		// Молчим, пока тест не закончится (канал / таймаут контекста).
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer upstream.Close()

	proxy := newProxyWithCleanup(t, createTestConfig())
	body := []byte(`{"model":"qwen3-8b","messages":[{"role":"user","content":"ок"}],"stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan error, 1)
	go func() {
		done <- proxy.proxyRequestOpenAIStreamAsNonStream(rec, req, body, upstream.URL, "backend-1")
	}()

	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("тестовый upstream не успел отдать первый кадр")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ошибка вызова: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("запрос не завершился после тишины upstream: сторожа тишины на пути буферизации нет — " +
			"это и есть дефект, который закрывает R548 (раньше спасал только предел 300 с, рубивший и живые ответы)")
	}

	raw := strings.TrimSpace(rec.Body.String())
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("тело не JSON (%q): %v", truncateForTest(raw), err)
	}
	code, _ := payload["error"].(map[string]interface{})
	gotCode := ""
	if code != nil {
		gotCode, _ = code["code"].(string)
	}
	if gotCode != "auto_stream_stalled" {
		t.Fatalf("код ошибки = %q, ожидался auto_stream_stalled (тело: %q) — оператор должен видеть «молчание», "+
			"а не «превышен предел» или пустой ответ", gotCode, truncateForTest(raw))
	}
}

// TestR548_StallCapRemainsOptOut — второй кап из T-4 (тишина посреди стрима)
// остаётся дефолтом и выключается нулём: он про молчание, а не про длительность.
func TestR548_StallCapRemainsOptOut(t *testing.T) {
	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "")
	if got := streamStallAbortTimeout(); got != streamStallAbortTimeoutDefault {
		t.Fatalf("порог тишины по умолчанию = %v, ожидался %v", got, streamStallAbortTimeoutDefault)
	}
	if streamStallAbortTimeoutDefault <= 30*time.Second {
		t.Fatalf("порог тишины %v слишком мал: паузы префилла/планировщика бывают дольше",
			streamStallAbortTimeoutDefault)
	}

	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "0")
	if got := streamStallAbortTimeout(); got != 0 {
		t.Fatalf("LB_STREAM_STALL_ABORT_SEC=0 должен выключать сторож, получено %v", got)
	}
}

// truncateForTest — короткий фрагмент тела для сообщения об ошибке.
func truncateForTest(s string) string {
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
