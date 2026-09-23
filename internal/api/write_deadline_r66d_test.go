//go:build llama_stub

// write_deadline_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС: длинные admin-операции (загрузка модели, применение профиля,
// reload) обязаны переживать серверный WriteTimeout.
//
// Живой случай: apiHTTPServer (порт 18081) имеет WriteTimeout=60s
// (cmd/balancer/main.go:383), а холодная загрузка gemma-4-E4B-it-Q4_K_M (4.2 GB)
// заняла 88 с. Ответ писался уже после истечения deadline → Go закрыл соединение
// без единого байта ответа (curl: HTTP 000, пустое тело), хотя в логах балансера
// операция завершилась `success:true`. В WebUI это выглядело как «загрузка
// модели не работает» и «настройки модели не применяются».
//
// Тест поднимает сервер с WriteTimeout=1s и handler'ом, который снимает deadline
// (extendWriteDeadline) и отвечает через 1.5s. Без вызова
// extendWriteDeadline тело до клиента не доходит.
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExtendWriteDeadline_SurvivesServerWriteTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extendWriteDeadline(w)
		// Имитируем длинную операцию (загрузка модели) — дольше WriteTimeout.
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"loaded"}`))
	}))
	// Типичный production default из cmd/balancer/main.go.
	srv.Config.WriteTimeout = 1 * time.Second
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("клиент не получил ответ (соединение закрыто по WriteTimeout): %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"success":true,"message":"loaded"}` {
		t.Errorf("body = %q, want success JSON — WriteTimeout обрезал ответ", string(body))
	}
}

// TestExtendWriteDeadline_NoSetWriteDeadlineSupport — если ResponseWriter не
// поддерживает SetWriteDeadline, helper не должен паниковать (просто логирует).
func TestExtendWriteDeadline_NoSetWriteDeadlineSupport(t *testing.T) {
	rec := httptest.NewRecorder()
	extendWriteDeadline(rec) // не должно паниковать
	rec.WriteHeader(http.StatusOK)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}
