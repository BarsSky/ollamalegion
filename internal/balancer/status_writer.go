// status_writer.go — R629 (2026-11-19): обёртка ResponseWriter и «отдано ли уже
// клиенту что-то необратимое».
//
// ЗАЧЕМ ВЫНЕСЕНО. `statusRecorder` и `clientAlreadyCommitted` — механизм, который
// решает, можно ли повторять запрос на другом бэкенде. Это отдельная тема с
// отдельными граблями (повтор после частичной отдачи = второй ответ в том же
// стриме, жалоба «OpenWebUI показывает ответ дважды»), и она не должна лежать
// вперемешку с маршрутизацией.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// statusRecorder — обёртка http.ResponseWriter для захвата HTTP статус-кода.
type statusRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	// errBody — накопленный body response (для 4xx/5xx). Round 16 (2026-07-10).
	// Только первые 1 KB чтобы не раздувать лог.
	errBody string
	// R83 (2026-09-25): сколько байт тела реально ушло клиенту и был ли connection
	// hijack'нут (hijack-путь пишет в сокет напрямую, минуя Write).
	//
	// Зачем: перед повторной попыткой на другом бэкенде нужно знать, не отдали ли
	// мы клиенту часть ответа. WriteHeader повторно не сыграть, но Write ДОПИШЕТ
	// тело — и клиент получит второй ответ в том же стриме (жалоба «OpenWebUI
	// показывает ответ дважды», см. clientAlreadyCommitted).
	bytesWritten int64
	hijacked     bool
}

// statusClientClosedR696 — «клиент закрыл запрос до ответа» (соглашение nginx).
//
// В HTTP этот код НЕ отправляется: писателя уже нет. Он нужен записи в
// «Прокси-логах», чтобы отменённая заявка не выглядела успешной: живой архив
// второго стенда показывал `status=200 bytes=0 first_byte_ms=-1`, то есть
// ожидание в очереди, оборванное клиентом, было не отличить от пустого
// УСПЕШНОГО ответа (клиент реально видел пустоту).
const statusClientClosedR696 = 499

// clientAlreadyCommitted — отправлено ли клиенту что-то необратимое.
//
// true означает: повторять запрос НЕЛЬЗЯ. Раньше повторяли всегда (proxy.go,
// циклы alternate-backend), и при обрыве уже начатого стрима второй ответ
// дописывался в тот же response.
func clientAlreadyCommitted(w http.ResponseWriter) bool {
	if sr, ok := w.(*statusRecorder); ok {
		return sr.bytesWritten > 0 || sr.hijacked
	}
	return false
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		// Заголовки уже отправлены — повторный вызов WriteHeader
		// вызывает "superfluous response.WriteHeader call" в логах.
		// Просто игнорируем.
		return
	}
	r.statusCode = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
		r.statusCode = http.StatusOK
		r.ResponseWriter.WriteHeader(http.StatusOK)
	}
	// Round 16: для error responses захватываем body чтобы логи могли показать
	// что конкретно вернул бэкенд. Обрезаем до 1 KB.
	if r.statusCode >= 400 && len(r.errBody) < 1024 {
		remaining := 1024 - len(r.errBody)
		if remaining > 0 {
			take := remaining
			if take > len(b) {
				take = len(b)
			}
			r.errBody += string(b[:take])
		}
	}
	n, err := r.ResponseWriter.Write(b)
	// R83: считаем РЕАЛЬНО записанные байты (n), а не длину b — при ошибке
	// клиент мог получить только часть, и это тоже означает «уже отдали».
	r.bytesWritten += int64(n)
	return n, err
}

// Round 31 #6 real fix (2026-08-09): hijack support на statusRecorder.
// Без этого Round 31 #6 hijack path не активируется (type assertion w.(http.Hijacker)
// возвращает false для *statusRecorder даже при embed http.ResponseWriter).
// Делегируем к underlying ResponseWriter.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("statusRecorder: underlying ResponseWriter %T does not support hijack", r.ResponseWriter)
	}
	conn, bufrw, err := hj.Hijack()
	if err == nil {
		// R83: соединение ушло из-под управления Go — повторять запрос нельзя,
		// даже если байты шли не через Write.
		r.hijacked = true
	}
	return conn, bufrw, err
}

// Round 31 #6 real fix: Flush support (для случая если statusRecorder используется
// вместо w, который раньше кастился в http.Flusher). Hijack path требует Flusher
// для manual chunked writes. Делегируем.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// isClientDisconnectErr — ошибка означает, что запрос отменил КЛИЕНТ (или наш же
// стрим закрылся по отмене контекста), а не сбой бэкенда.
//
// R194 (2026-10-20). Живой дефект (errors-matrix-r180): после обрыва клиента
// следующий запрос получал 503, потому что здесь безусловно выполнялось
// `UpdateBackendStatus(StatusUnhealthy)`. Отмена клиента — не отказ бэкенда:
// модель и слот в порядке, помечать узел нездоровым нельзя.
func isClientDisconnectErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, marker := range []string{"context canceled", "context_canceled", "client disconnected", "broken pipe", "connection reset by peer"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
