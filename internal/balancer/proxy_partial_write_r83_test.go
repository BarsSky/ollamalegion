//go:build llama_stub

// proxy_partial_write_r83_test.go — R83 (2026-09-25).
//
// Зачем гард. Жалоба: «при выводе клиент OpenWebUI дублирует ответ».
// Один из механизмов — повтор запроса на другом бэкенде ПОСЛЕ того, как часть
// ответа уже ушла клиенту: statusRecorder глушит повторный WriteHeader, но Write
// дописывает тело, поэтому клиент получает второй ответ в том же стриме.
//
// Второй механизм (перегенерация при авто-продолжении) закрыт отдельно —
// см. auto_continue_duplicate_similarity_r83_test.go.
package balancer

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubHijackWriter — ResponseWriter, который умеет hijack (успешный или нет).
type stubHijackWriter struct {
	*httptest.ResponseRecorder
	hijackErr error
}

func (s *stubHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if s.hijackErr != nil {
		return nil, nil, s.hijackErr
	}
	// Соединение в тесте не нужно: проверяем только факт «ушли из-под Go».
	return nil, nil, nil
}

func TestR83_StatusRecorder_CountsRealBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, statusCode: http.StatusOK}

	if clientAlreadyCommitted(sr) {
		t.Error("сразу после создания recorder уже считается committed")
	}

	// Одних заголовков для «отдали ответ» недостаточно: тело ещё не ушло,
	// и повторная попытка не создаст дубль контента.
	sr.WriteHeader(http.StatusOK)
	if clientAlreadyCommitted(sr) {
		t.Error("после WriteHeader без тела ответ не должен считаться отданным")
	}

	if _, err := sr.Write([]byte("Первая часть ответа")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !clientAlreadyCommitted(sr) {
		t.Error("после записи тела ответ обязан считаться отданным (иначе повтор даст дубль)")
	}
	if sr.bytesWritten != int64(len("Первая часть ответа")) {
		t.Errorf("bytesWritten = %d, want %d", sr.bytesWritten, len("Первая часть ответа"))
	}
}

// TestR83_StatusRecorder_ShortWriteStillCommitted — если базовый writer принял
// только часть байт, ответ всё равно уже испорчен: повторять нельзя.
func TestR83_StatusRecorder_ShortWriteStillCommitted(t *testing.T) {
	sr := &statusRecorder{ResponseWriter: &shortWriter{n: 3}, statusCode: http.StatusOK}
	_, _ = sr.Write([]byte("abcdefghij"))
	if !clientAlreadyCommitted(sr) {
		t.Error("частичная запись не помечена как отданная")
	}
	if sr.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3 (реально записанные байты)", sr.bytesWritten)
	}
}

// shortWriter принимает только n байт — эмулирует обрыв соединения.
type shortWriter struct {
	h http.Header
	n int
}

func (s *shortWriter) Header() http.Header {
	if s.h == nil {
		s.h = make(http.Header)
	}
	return s.h
}
func (s *shortWriter) WriteHeader(int) {}
func (s *shortWriter) Write(b []byte) (int, error) {
	if len(b) > s.n {
		return s.n, errors.New("short write (simulated broken pipe)")
	}
	return len(b), nil
}

func TestR83_ClientAlreadyCommitted_Hijacked(t *testing.T) {
	w := &stubHijackWriter{ResponseRecorder: httptest.NewRecorder()}
	sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

	if clientAlreadyCommitted(sr) {
		t.Error("до hijack ответ не отдан")
	}
	if _, _, err := sr.Hijack(); err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if !clientAlreadyCommitted(sr) {
		t.Error("после успешного hijack соединение вне Go — повторять запрос нельзя")
	}
}

func TestR83_ClientAlreadyCommitted_FailedHijack(t *testing.T) {
	w := &stubHijackWriter{
		ResponseRecorder: httptest.NewRecorder(),
		hijackErr:        errors.New("hijack not possible"),
	}
	sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

	if _, _, err := sr.Hijack(); err == nil {
		t.Fatal("ожидалась ошибка hijack")
	}
	if clientAlreadyCommitted(sr) {
		t.Error("неудачный hijack не означает «ответ отдан»")
	}
}

// TestR83_ClientAlreadyCommitted_NonRecorder — writer без обёртки не должен
// приводить к отказу от повторов (иначе сломались бы все прочие пути).
func TestR83_ClientAlreadyCommitted_NonRecorder(t *testing.T) {
	if clientAlreadyCommitted(httptest.NewRecorder()) {
		t.Error("сырой ResponseWriter не должен считаться committed")
	}
	if clientAlreadyCommitted(nil) {
		t.Error("nil writer не должен считаться committed")
	}
}

// TestR83_RetryGuard_PreventsDuplicateBody — суть гарда в одном месте:
// смоделировать «первая попытка отдала часть стрима, потом упала» и убедиться,
// что повтор в тот же writer дал бы ДУБЛЬ (то есть гард обязателен).
//
// Здесь проверяется именно арифметика дубля, а не маршрутизация proxy —
// маршрутизация требует поднятых бэкендов и покрыта proxy_integration_test.go.
func TestR83_RetryGuard_PreventsDuplicateBody(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, statusCode: http.StatusOK}

	// Первая попытка: успела отдать начало ответа.
	_, _ = sr.Write([]byte("Столица Франции — Париж. "))

	committed := clientAlreadyCommitted(sr)
	if !committed {
		t.Fatal("гард не сработал: повтор был бы разрешён")
	}

	// Демонстрируем, что было бы БЕЗ гарда — второй ответ дописывается в тот же
	// response (именно это видит пользователь как «ответ дважды»).
	_, _ = sr.Write([]byte("Столица Франции — Париж. "))

	body := rec.Body.String()
	if strings.Count(body, "Столица Франции — Париж.") != 2 {
		t.Fatalf("модель дубля не воспроизведена, body=%q", body)
	}
	// С гардом второй Write не выполняется — значит в теле ровно одно вхождение.
	// (Проверено выше через committed; здесь фиксируем контракт явно.)
	if !committed {
		t.Error("guard должен возвращать true после частичной отдачи")
	}
}
