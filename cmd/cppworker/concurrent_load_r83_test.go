//go:build llama_stub

// concurrent_load_r83_test.go — R83 §9.6 D-A (2026-09-27).
//
// «Асинхронность только на бумаге»: обе ветки «другой запрос уже грузит эту
// модель» сначала вызывали backend.WaitForLoad(name) и лишь ПОТОМ смотрели на
// waitSync. WaitForLoad ждёт канал загрузки без таймаута, поэтому при
// `?wait=false` (default) HTTP-ответ приходил не сразу, а после чужой загрузки:
// в логе это `status:202 duration:9m6.6s`. Клиент с коротким таймаутом
// отваливался, хотя progressUrl у него уже был.
//
// Тесты фиксируют контракт: async отвечает 202 СРАЗУ (не дожидаясь канала),
// sync (?wait=true) — по-прежнему ждёт, потому что в этом и смысл запроса.
package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// holdLoad — занять лок загрузки так, как это делает другой запрос: канал
// остаётся открытым (никто не завершил загрузку). Возвращает функцию
// завершения «загрузки» другой стороны.
func holdLoad(t *testing.T, name string) func() {
	t.Helper()
	taken, err := backend.TryLockLoad(name)
	if err != nil || !taken {
		t.Fatalf("не удалось занять лок загрузки: taken=%v err=%v", taken, err)
	}
	return func() { backend.UnlockLoad(name) }
}

// TestR83_AsyncLoadDoesNotWaitForConcurrentLoad — главный тест D-A: при
// wait=false ответ приходит немедленно, пока чужая загрузка ещё идёт.
//
// Раньше здесь был бы таймаут теста: helper ждал канал, который освобождают
// только через 30 секунд, а лимит ожидания в тестах — доли секунды.
func TestR83_AsyncLoadDoesNotWaitForConcurrentLoad(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	setupLoadLockTestBackend(t)

	const model = "r83-concurrent-async-model"
	release := holdLoad(t, model)
	defer release()

	// Чужую «загрузку» отпускаем через 30 с — если бы ответ её ждал, тест не
	// успел бы проверить ничего (и это ровно тот симптом, что был в логе).
	time.AfterFunc(30*time.Second, release)

	w := bodyReq("POST", "/api/models/load-with-params",
		`{"name":"`+model+`","contextSize":4096}`)

	start := time.Now()
	respondWhileAnotherLoadInProgress(w, httptest.NewRequest("POST", "/api/models/load-with-params", nil),
		model, "/tmp/"+model+".gguf", 1000, start, false /* waitSync */)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("async-ответ ждал чужую загрузку %v — «202 сразу» нарушен (D-A вернулся)",
			elapsed)
	}
	if w.Code != 202 {
		t.Fatalf("status = %d, want 202 (клиент дополлит progressUrl)", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := resp["status"].(string); got != "loading" {
		t.Errorf("status = %q, want loading", got)
	}
	if got, _ := resp["progressUrl"].(string); got == "" {
		t.Error("progressUrl пуст — клиенту нечем поллить состояние")
	}
	// Важно: ответ честно сообщает state=loading, а не выдумывает loaded.
	lm, _ := resp["model"].(map[string]interface{})
	if lm == nil {
		t.Fatalf("в ответе нет model: %v", resp)
	}
	if got, _ := lm["state"].(string); got != string(cppbackend.StateLoading) {
		t.Errorf("model.state = %q, want loading (мы не знаем, чем закончится чужая загрузка)", got)
	}
}

// TestR83_SyncLoadWaitsForConcurrentLoad — при wait=true ожидание сохраняется:
// клиент сознательно просил дождаться, и раньше именно этот путь давал
// `loaded_by_other`.
func TestR83_SyncLoadWaitsForConcurrentLoad(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	setupLoadLockTestBackend(t)

	const model = "r83-concurrent-sync-model"
	release := holdLoad(t, model)

	// Завершаем чужую загрузку через 150 мс — helper обязан дождаться.
	time.AfterFunc(150*time.Millisecond, release)

	w := bodyReq("POST", "/api/models/load-with-params", "")
	start := time.Now()
	respondWhileAnotherLoadInProgress(w, httptest.NewRequest("POST", "/api/models/load-with-params", nil),
		model, "/tmp/"+model+".gguf", 1000, start, true /* waitSync */)
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond {
		t.Errorf("sync-ответ вернулся за %v — не дождался чужой загрузки", elapsed)
	}
	// Модель в реестре так и не появилась (лок отпустили, но загрузки не было):
	// helper обязан ответить ошибкой, а не «202, который не станет loaded».
	if w.Code != 503 && w.Code != 500 {
		t.Fatalf("status = %d, want 5xx (загрузка не завершилась успехом), body=%s",
			w.Code, w.Body.String())
	}
}

// TestR83_SyncLoadReportsLoadedByOther — если чужая загрузка УСПЕШНО завершилась
// (модель появилась в реестре), sync-клиент получает loaded_by_other с моделью,
// а не 503: это тот случай, ради которого ожидание и существует.
func TestR83_SyncLoadReportsLoadedByOther(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	setupLoadLockTestBackend(t)

	const model = "r83-concurrent-loaded-model"
	release := holdLoad(t, model)

	// «Чужая загрузка» завершается успешно: регистрируем модель под тем же
	// именем и отпускаем лок — ровно то, что делает UnlockLoad в runAsyncLoad.
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	time.AfterFunc(100*time.Millisecond, release)

	w := bodyReq("POST", "/api/models/load-with-params", "")
	start := time.Now()
	respondWhileAnotherLoadInProgress(w, httptest.NewRequest("POST", "/api/models/load-with-params", nil),
		model, "/tmp/"+model+".gguf", 1000, start, true /* waitSync */)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (loaded_by_other), body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := resp["status"].(string); got != "loaded_by_other" {
		t.Errorf("status = %q, want loaded_by_other", got)
	}
	if _, ok := resp["model"]; !ok {
		t.Error("в ответе нет model — клиент не увидит, что загрузилось")
	}
}
