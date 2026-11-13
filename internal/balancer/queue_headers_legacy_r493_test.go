package balancer

// queue_headers_legacy_r493_test.go — R493-12o (долг P2-13/T4): обе поверхности
// очереди говорят на одном языке.
//
// ЖИВОЙ ДЕФЕКТ. Заголовки очереди объявлены один раз (`queue_headers_r493.go`),
// и страж T4 требует, чтобы `X-Queue-*` не писались литералами мимо него. Но у
// очереди ОЖИДАНИЯ БЭКЕНДА (путь обработчиков llama.cpp, R403) были СВОИ имена —
// `X-Backend-Queue-Position` / `X-Backend-Queue-Wait-Ms`, — и они не попадали в
// список стража. Следствие: один и тот же факт («запрос стоял в очереди N мс,
// позиция P») на `/api/chat` назывался `X-Queue-*`, а на `/v1/chat/completions`
// — `X-Backend-Queue-*`. Клиент с одним парсером видел очередь только на одной
// поверхности. Вторая половина: на УСПЕШНОМ ожидании `/v1` не выставлял ничего —
// ожидание было видно в логе, но не клиенту.
//
// Исправлено: канон ставится общей функцией (`setQueueUnavailableHeadersR493` /
// `setQueueWaitHeadersR493`), прежние имена — алиасы из того же файла (клиентов,
// которые их уже читают, нельзя лишать данных молча).

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestR493_LegacyQueueHeadersAreAliases — прежние имена несут ТЕ ЖЕ числа, что
// канонические: два имени одного факта не могут разойтись.
func TestR493_LegacyQueueHeadersAreAliases(t *testing.T) {
	w := httptest.NewRecorder()
	setQueueWaitHeadersR493(w, 2500*time.Millisecond, 4)
	setLegacyBackendQueueHeadersR493(w, 2500*time.Millisecond, 4)

	pairs := [][2]string{
		{headerQueueWaitMsR493, headerLegacyBackendQueueWaitMsR493},
		{headerQueuePositionR493, headerLegacyBackendQueuePositionR493},
	}
	for _, p := range pairs {
		canon, alias := w.Header().Get(p[0]), w.Header().Get(p[1])
		if canon == "" {
			t.Fatalf("канонический %s не выставлен", p[0])
		}
		if canon != alias {
			t.Fatalf("%s=%q и прежний %s=%q разошлись — это два имени одного факта",
				p[0], canon, p[1], alias)
		}
	}
}

// TestR493_BackendUnavailableSpeaksQueueVocabulary — 503 очереди бэкенда несёт
// канонические X-Queue-*, Retry-After, лимит ожидания и прежние алиасы.
func TestR493_BackendUnavailableSpeaksQueueVocabulary(t *testing.T) {
	w := httptest.NewRecorder()
	writeBackendUnavailableR403(w, "qwen", 3*time.Second, 2, true)

	mustHave := []string{
		headerQueuePositionR493,
		headerQueueWaitMsR493,
		headerQueueWaitMaxSecR493,
		headerRetryAfterR493,
		headerLegacyBackendQueuePositionR493,
		headerLegacyBackendQueueWaitMsR493,
	}
	for _, h := range mustHave {
		if got := w.Header().Get(h); got == "" {
			t.Errorf("%s пуст: /v1 обязан сообщать об ожидании тем же языком, что /api/chat", h)
		}
	}
	if got := w.Code; got != 503 {
		t.Fatalf("код %d, ожидался 503", got)
	}
	// Прежний контракт тела не тронут: клиенты читают error/code/reason.
	body := w.Body.String()
	for _, want := range []string{`"error":"no llama.cpp backend available"`, `"code":"no_backend"`, `"position":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("в теле нет %s (прежний контракт сломан): %s", want, body)
		}
	}
}

// TestR493_WaitingRequestReportsCanonicalHeaders — успешное ожидание тоже обязано
// отдавать канонические заголовки (до правки /v1 молчал).
func TestR493_WaitingRequestReportsCanonicalHeaders(t *testing.T) {
	// Страж подключения: вызов стоит в самом ожидании бэкенда, а не только в 503.
	// Поведенческий тест потребовал бы собрать роутер с живым выбором бэкенда —
	// он проверял бы фикстуру, а не контракт; подключение проверяется отдельно.
	src, err := os.ReadFile("backend_wait_queue_r403.go")
	if err != nil {
		t.Fatalf("не прочитать backend_wait_queue_r403.go: %v", err)
	}
	if !strings.Contains(string(src), "setQueueWaitHeadersR493(w, out.Waited, out.Position)") {
		t.Fatal("успешное ожидание в waitForLlamaCppBackendR403 не выставляет X-Queue-* — " +
			"клиент на /v1 не узнает, что запрос стоял в очереди")
	}
}
