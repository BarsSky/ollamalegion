// queue_headers_t4_test.go — T4 из плана «два мозга» (refactoring-plan.ru.md §3):
// «одинаковые X-Queue-* и /queue на обоих путях».
//
// ЗАЧЕМ. Заголовки очереди (X-Queue-Wait-Ms, X-Queue-Position,
// X-Queue-Wait-Max-Sec, X-Queue-Wait-Reason, Retry-After) выставлялись
// СТРОКОВЫМИ ЛИТЕРАЛАМИ в трёх местах: handler-путь llama.cpp (admission_queue.go,
// два места) и generic-путь (proxy.go). Паритет держался на дисциплине, а вся
// история R477/R480 показывает, чем это кончается: два пути, «одинаковый»
// контракт и расхождение, которое замечают клиенты, а не мы.
//
// Теперь имена объявлены один раз (queue_headers_r493.go), оба пути зовут общие
// сеттеры, и это проверяется статически:
//  1. ни один продакшен-файл, кроме queue_headers_r493.go, не содержит этих
//     литералов (имена не могут разъехаться по значению);
//  2. handler-путь и generic-путь ВЫЗЫВАЮТ общие сеттеры (иначе кто-то снова
//     начнёт ставить заголовки руками);
//  3. поведение сеттеров: ожидание > 0 → Wait-Ms; позиция > 0 → Position;
//     позиции 0 не бывает в заголовке (0 = «не ждал»); в 503 есть Retry-After,
//     фактический лимит ожидания и причина только когда она есть.
package balancer

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// queueHeaderLiteralsT4 — имена X-Queue-*, которые нельзя писать литералами мимо
// queue_headers_r493.go.
//
// ПОЧЕМУ БЕЗ Retry-After. Первая версия правила требовала и его — и упала на
// шести файлах (llamacpp_transport_nonstream, preflight_helper, proxy,
// proxy_helpers, proxy_request, upstream_error_response). Проверка кода показала,
// что это НЕ дублирование очереди: Retry-After там ставится для других вещей
// (проброс 503 апстрима, лимиты, ошибки preflight). Заголовок общий для HTTP, и
// «один источник» у него один — стандарт; наш контракт — именно X-Queue-*.
var queueHeaderLiteralsT4 = []string{
	"X-Queue-Wait-Ms",
	"X-Queue-Position",
	"X-Queue-Wait-Max-Sec",
	"X-Queue-Wait-Reason",
	// R493-12o: прежние имена очереди ожидания бэкенда (R403). Пока они жили
	// литералами в backend_wait_queue_r403.go, ОДНА И ТА ЖЕ информация называлась
	// по-разному на разных поверхностях, и страж этого не видел (он искал только
	// X-Queue-*). Теперь они — алиасы, объявленные там же, где канон.
	"X-Backend-Queue-Position",
	"X-Backend-Queue-Wait-Ms",
}

// TestT4_HeaderNamesLiveInOneFile — имена заголовков очереди объявлены один раз.
func TestT4_HeaderNamesLiveInOneFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("не прочитать каталог пакета: %v", err)
	}
	const allowed = "queue_headers_r493.go"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == allowed {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", name, err)
		}
		text := string(src)
		for _, h := range queueHeaderLiteralsT4 {
			if strings.Contains(text, `"`+h+`"`) {
				t.Errorf("%s: заголовок %q выставляется литералом мимо %s — "+
					"на двух путях (handler/generic) имена обязаны приходить из одного места, "+
					"иначе паритет снова будет держаться на дисциплине", name, h, allowed)
			}
		}
	}
}

// TestT4_BothPathsCallSharedSetters — оба пути зовут общие сеттеры.
func TestT4_BothPathsCallSharedSetters(t *testing.T) {
	cases := []struct {
		file string
		what string
	}{
		{"admission_queue.go", "handler-путь llama.cpp"},
		{"proxy.go", "generic-путь (unified-очередь)"},
	}
	for _, c := range cases {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", c.file, err)
		}
		text := string(src)
		if !strings.Contains(text, "setQueueWaitHeadersR493(") {
			t.Errorf("%s (%s): нет вызова setQueueWaitHeadersR493 — клиент не увидит, "+
				"что запрос стоял в очереди", c.file, c.what)
		}
	}
	src, err := os.ReadFile("admission_queue.go")
	if err != nil {
		t.Fatalf("не прочитать admission_queue.go: %v", err)
	}
	if !strings.Contains(string(src), "setQueueUnavailableHeadersR493(") {
		t.Error("admission_queue.go: 503 очереди не использует setQueueUnavailableHeadersR493 — " +
			"Retry-After и лимит ожидания снова начнут расходиться")
	}

	// R493-12o: очередь ожидания бэкенда (второй путь входа в handler-путь) тоже
	// обязана говорить на общем языке — и при отказе, и при успешном ожидании.
	// До правки она выставляла только СВОИ имена, поэтому клиент с одним
	// парсером видел очередь лишь на одной поверхности.
	wq, err := os.ReadFile("backend_wait_queue_r403.go")
	if err != nil {
		t.Fatalf("не прочитать backend_wait_queue_r403.go: %v", err)
	}
	for _, call := range []string{"setQueueUnavailableHeadersR493(", "setQueueWaitHeadersR493("} {
		if !strings.Contains(string(wq), call) {
			t.Errorf("backend_wait_queue_r403.go: нет вызова %s — /v1 не сообщает об "+
				"ожидании на языке X-Queue-*, паритет T4 не выполняется", call)
		}
	}
}

// TestT4_SettersBehaviour — что именно попадает в заголовки.
func TestT4_SettersBehaviour(t *testing.T) {
	// Ожидание было: оба заголовка на месте, числа в миллисекундах.
	w := httptest.NewRecorder()
	setQueueWaitHeadersR493(w, 1500*time.Millisecond, 3)
	if got := w.Header().Get(headerQueueWaitMsR493); got != "1500" {
		t.Errorf("%s = %q, ожидалось 1500", headerQueueWaitMsR493, got)
	}
	if got := w.Header().Get(headerQueuePositionR493); got != "3" {
		t.Errorf("%s = %q, ожидалось 3", headerQueuePositionR493, got)
	}

	// Ожидания не было: заголовков нет, иначе «стоял в очереди 0 мс» вводит в заблуждение.
	w = httptest.NewRecorder()
	setQueueWaitHeadersR493(w, 0, 0)
	if got := w.Header().Get(headerQueueWaitMsR493); got != "" {
		t.Errorf("%s = %q при нулевом ожидании, ожидалась пустота", headerQueueWaitMsR493, got)
	}
	if got := w.Header().Get(headerQueuePositionR493); got != "" {
		t.Errorf("%s = %q при нулевой позиции, ожидалась пустота", headerQueuePositionR493, got)
	}

	// 503: Retry-After, фактический лимит и причина.
	w = httptest.NewRecorder()
	setQueueUnavailableHeadersR493(w, 2, 5*time.Second, 7*time.Second, 5, imageLockWaitReason)
	if got := w.Header().Get(headerRetryAfterR493); got != "5" {
		t.Errorf("%s = %q, ожидалось 5", headerRetryAfterR493, got)
	}
	if got := w.Header().Get(headerQueueWaitMsR493); got != "5000" {
		t.Errorf("%s = %q, ожидалось 5000", headerQueueWaitMsR493, got)
	}
	if got := w.Header().Get(headerQueueWaitMaxSecR493); got != "7" {
		t.Errorf("%s = %q, ожидалось 7", headerQueueWaitMaxSecR493, got)
	}
	if got := w.Header().Get(headerQueueWaitReasonR493); got != imageLockWaitReason {
		t.Errorf("%s = %q, ожидалось %q", headerQueueWaitReasonR493, got, imageLockWaitReason)
	}

	// 503 без причины: заголовок причины НЕ ставится (пустая причина — не причина).
	w = httptest.NewRecorder()
	setQueueUnavailableHeadersR493(w, 0, time.Second, time.Second, 5, "")
	if got := w.Header().Get(headerQueueWaitReasonR493); got != "" {
		t.Errorf("%s = %q без причины, ожидалась пустота", headerQueueWaitReasonR493, got)
	}
}
