//go:build llama_stub

// model_management_progress_r66d_test.go — R66d (2026-09-22).
//
// РЕГРЕСС. После 202 Accepted от cppworker балансер поллит загрузку до maxWait
// (3-15 минут). Провалившаяся загрузка была неотличима от «ещё грузится»:
// cppworker удалял запись о модели и нигде не хранил причину. Итог — операция
// load висела в статусе running, а клиент (реальный Cline с отсутствующей
// моделью gemma-4-E4B-it-Q4_K_M) получал 503 «подожди 90 секунд» бесконечно.
//
// Теперь поллинг дополнительно смотрит /api/models/load/progress и
// останавливается, как только cppworker сообщил state="failed", а если модель
// вообще нигде не появляется несколько поллов подряд — считает загрузку
// провалившейся (страховка).

package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPollLoadCompletion_FailedStateStopsPolling — cppworker сообщает failed →
// ранний выход с настоящей причиной (без ожидания maxWait).
func TestPollLoadCompletion_FailedStateStopsPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/models/load/progress":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"gemma-missing","state":"failed","error":"failed to open GGUF file 'models/gemma-missing.gguf' (No such file or directory)"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)

	start := time.Now()
	res := mm.pollLoadCompletionUntilLoaded(host, port, "test-bk", "gemma-missing", 60*time.Second)
	elapsed := time.Since(start)

	if res == nil {
		t.Fatal("ожидалась ошибка, получен nil (поллинг ушёл ждать maxWait)")
	}
	if res.Success {
		t.Error("Success = true, want false")
	}
	if !strings.Contains(res.Error, "auto-load failed") || !strings.Contains(res.Error, "gemma-missing.gguf") {
		t.Errorf("Error = %q, ожидался текст с причиной от cppworker", res.Error)
	}
	if elapsed > 15*time.Second {
		t.Errorf("поллинг занял %s — должен был остановиться на первом же state=failed", elapsed)
	}
}

// TestPollLoadCompletion_NeverAppearsStopsPolling — модель не появляется ни в
// /api/models, ни в progress (404) → страховка останавливает поллинг через
// несколько поллов, а не через maxWait.
func TestPollLoadCompletion_NeverAppearsStopsPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/models/load/progress":
			http.NotFound(w, r) // «model not found and not loading»
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)

	start := time.Now()
	res := mm.pollLoadCompletionUntilLoaded(host, port, "test-bk", "ghost", 120*time.Second)
	elapsed := time.Since(start)

	if res == nil {
		t.Fatal("ожидалась ошибка «модель не появилась», получен nil")
	}
	if res.Success {
		t.Error("Success = true, want false")
	}
	if !strings.Contains(res.Error, "did not appear") {
		t.Errorf("Error = %q, ожидалось упоминание «did not appear»", res.Error)
	}
	if elapsed > 30*time.Second {
		t.Errorf("страховка сработала через %s — слишком долго (ожидалось ~6с)", elapsed)
	}
}

// TestPollLoadCompletion_LoadedStillSucceeds — регресс: нормальная загрузка
// по-прежнему засчитывается успешной.
func TestPollLoadCompletion_LoadedStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"ok-model","state":"loaded"}]}`))
		case "/api/models/load/progress":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"ok-model","state":"loaded"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)

	res := mm.pollLoadCompletionUntilLoaded(host, port, "test-bk", "ok-model", 30*time.Second)
	if res == nil {
		t.Fatal("нормальная загрузка должна вернуть результат, получен nil")
	}
	if !res.Success {
		t.Errorf("Success = false (%q), want true", res.Error)
	}
}
