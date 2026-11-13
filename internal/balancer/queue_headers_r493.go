// queue_headers_r493.go — R493/долг P2-13: заголовки очереди в ОДНОМ месте.
//
// ЗАЧЕМ. Клиентские заголовки очереди (`X-Queue-Wait-Ms`, `X-Queue-Position`,
// `X-Queue-Wait-Max-Sec`, `X-Queue-Wait-Reason`, `Retry-After`) выставлялись
// строковыми литералами в ТРЁХ местах: handler-путь llama.cpp
// (admission_queue.go — и при выданном слоте, и в 503), generic-путь
// (proxy.go, после unified-очереди). Это ровно тот класс дефектов, из которого
// выросла вся история R477/R480: два пути, одинаковый «контракт» и расхождение,
// которое замечают клиенты, а не мы. Пока имена живут литералами, паритет
// держится на дисциплине; теперь он держится конструкцией — оба пути зовут одни
// и те же функции, а имена объявлены один раз.
//
// ВАЖНО про порядок: заголовки можно ставить только ДО первой записи тела
// (ADMISSION-HONEST, 2026-10-15). Функции ниже — чистые сеттеры: они ничего не
// коммитят и не пишут тело, поэтому вызывающий сам отвечает за момент вызова.
package balancer

import (
	"net/http"
	"strconv"
	"time"
)

// Имена заголовков очереди (стабильные: их читают клиенты и панель).
const (
	headerQueueWaitMsR493     = "X-Queue-Wait-Ms"
	headerQueuePositionR493   = "X-Queue-Position"
	headerQueueWaitMaxSecR493 = "X-Queue-Wait-Max-Sec"
	headerQueueWaitReasonR493 = "X-Queue-Wait-Reason"
	headerRetryAfterR493      = "Retry-After"
)

// setQueueWaitHeadersR493 — «запрос стоял в очереди и слот выдан».
//
// Ставится на ОБОИХ путях после успешного ожидания: клиент должен узнать, что
// запрос ждал, независимо от поверхности (/api/chat, /v1/chat/completions).
func setQueueWaitHeadersR493(w http.ResponseWriter, waited time.Duration, position int) {
	if waited > 0 {
		w.Header().Set(headerQueueWaitMsR493, strconv.FormatInt(waited.Milliseconds(), 10))
	}
	if position > 0 {
		w.Header().Set(headerQueuePositionR493, strconv.Itoa(position))
	}
}

// setQueueUnavailableHeadersR493 — 503 «слот не освободился».
//
// retryAfterSec — подсказка клиенту, maxWaitSec — ФАКТИЧЕСКИ применённый лимит
// ожидания (он бывает сужен per-request, см. imageLockWaitCap), reason — машинный
// код причины (пусто = обычная перегрузка).
func setQueueUnavailableHeadersR493(
	w http.ResponseWriter, position int, waited, maxWait time.Duration,
	retryAfterSec int, reason string,
) {
	w.Header().Set(headerRetryAfterR493, strconv.Itoa(retryAfterSec))
	if position > 0 {
		w.Header().Set(headerQueuePositionR493, strconv.Itoa(position))
	}
	w.Header().Set(headerQueueWaitMsR493, strconv.FormatInt(waited.Milliseconds(), 10))
	w.Header().Set(headerQueueWaitMaxSecR493, strconv.Itoa(int(maxWait.Seconds())))
	if reason != "" {
		w.Header().Set(headerQueueWaitReasonR493, reason)
	}
}

// Прежние имена очереди ожидания бэкенда (R403). Оставлены АЛИАСАМИ: их читают
// уже написанные клиенты, и убрать их молча — значит отобрать данные у тех, кто
// на них опирается. Канонические имена — X-Queue-* (см. выше); литералов мимо
// этого файла нет (страж T4).
const (
	headerLegacyBackendQueuePositionR493 = "X-Backend-Queue-Position"
	headerLegacyBackendQueueWaitMsR493   = "X-Backend-Queue-Wait-Ms"
)

// setLegacyBackendQueueHeadersR493 — алиасы для клиентов, читающих прежние имена.
//
// R493-12o (долг P2-13/T4): до этой правки ОДНА И ТА ЖЕ информация называлась
// по-разному в зависимости от поверхности — `/api/chat` отдавал X-Queue-Position,
// `/v1/chat/completions` (путь с очередью ожидания бэкенда) отдавал
// X-Backend-Queue-Position. Клиент с одним парсером видел очередь только на одной
// из поверхностей, а страж T4 этого не ловил: он искал литералы только из списка
// X-Queue-*.
func setLegacyBackendQueueHeadersR493(w http.ResponseWriter, waited time.Duration, position int) {
	if w == nil {
		return
	}
	if position > 0 {
		w.Header().Set(headerLegacyBackendQueuePositionR493, strconv.Itoa(position))
	}
	w.Header().Set(headerLegacyBackendQueueWaitMsR493, strconv.FormatInt(waited.Milliseconds(), 10))
}
