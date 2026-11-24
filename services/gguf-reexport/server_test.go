// server_test.go — HTTP-коды отказов ре-экспорта.
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ МЕНЕДЖЕРА. Оператор в браузере видит только HTTP-ответ:
// если сервис вернул 422 вместо 400 или спрятал код ошибки в текст, WebUI не
// сможет подсказать действие. Проверяем ровно то, что обещает контракт:
// путь вне тома моделей — 400 `path_not_allowed`, нормальный запрос — 202.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestServer(t *testing.T, m *Manager, cfg config) *server {
	t.Helper()
	return newServer(cfg, m, newInfoProbe(cfg), newModelsCache(0), zap.NewNop().Sugar())
}

func postJob(t *testing.T, srv *server, body string) (*httptest.ResponseRecorder, *struct {
	Error *jobError `json:"error"`
}) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/reexport/jobs", strings.NewReader(body))
	srv.routes().ServeHTTP(rec, req)
	var resp struct {
		Error *jobError `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, &resp
}

func TestCreateOutputEscapeReturns400PathNotAllowed(t *testing.T) {
	m, cfg, cancel := newTestManager(t, &fakeRunner{})
	defer cancel()
	srv := newTestServer(t, m, cfg)

	cases := map[string]string{
		"выход из каталога модели": `{"model_dir":"inkling.gguf","target":"inkling2_loop","options":{"out_path":"../escape.gguf"}}`,
		"абсолютный путь вне тома": `{"model_dir":"inkling.gguf","target":"inkling2_loop","options":{"out_path":"` +
			strings.ReplaceAll(t.TempDir(), `\`, `\\`) + `/evil.gguf"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec, resp := postJob(t, srv, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, ожидался 400 (тело: %s)", rec.Code, rec.Body.String())
			}
			if resp.Error == nil || resp.Error.Code != "path_not_allowed" {
				t.Fatalf("код ошибки %+v, ожидался path_not_allowed", resp.Error)
			}
			if resp.Error.Message == "" {
				t.Fatal("отказ без объяснения: оператор не поймёт, что править в форме")
			}
		})
	}
}

// TestCreateValidJobReturns202 — контроль к отказу выше: тот же обработчик на
// нормальном запросе отвечает 202, то есть 400 приходит от проверки пути, а не
// от того, что эндпоинт сломан.
func TestCreateValidJobReturns202(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()
	srv := newTestServer(t, m, cfg)

	rec, resp := postJob(t, srv, `{"model_dir":"inkling.gguf","target":"inkling2_loop"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("HTTP %d, ожидался 202 (тело: %s)", rec.Code, rec.Body.String())
	}
	if resp.Error != nil {
		t.Fatalf("в успешном ответе ошибка: %+v", resp.Error)
	}
	// Дожидаемся финала: задача держит каталог состояния, и удалять временный
	// каталог под работающим исполнителем значит получить флейк на ровном месте.
	var created jobView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("ответ не разобран: %v", err)
	}
	waitStatus(t, m, created.ID, statusSucceeded, 5*time.Second)
}

// TestCreateShortOnDiskReturnsJobFailure — отказ по месту приходит не HTTP-кодом
// на POST (задача создана и стоит в очереди), а статусом задачи: код
// insufficient_space и числа «нужно/свободно» в сообщении.
func TestCreateShortOnDiskReturnsJobFailure(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()
	m.freeSpace = func(string) (int64, error) { return 1 << 20, nil }
	srv := newTestServer(t, m, cfg)

	rec, _ := postJob(t, srv, `{"model_dir":"inkling.gguf","target":"inkling2_loop"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("HTTP %d, ожидался 202: отказ по месту виден в статусе задачи", rec.Code)
	}
	var created jobView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("ответ не разобран: %v", err)
	}
	final := waitStatus(t, m, created.ID, statusFailed, 5*time.Second)
	if final.Error == nil || final.Error.Code != "insufficient_space" {
		t.Fatalf("код задачи %+v, ожидался insufficient_space", final.Error)
	}
	if !strings.Contains(final.Error.Message, "нужно") || !strings.Contains(final.Error.Message, "свободно") {
		t.Fatalf("в сообщении нет чисел «нужно/свободно»: %s", final.Error.Message)
	}
}
