//go:build llama_stub

// reload_busy_r83_test.go — R83/v60 (2026-10-01): reload во время генерации.
//
// Живой дефект. handleReloadModel ждал дренажа активных запросов БЕЗ ограничения
// времени (inflight.WaitZero(name, 0) — «0 = ждать вечно»), причём дренаж жил
// только в sync-ветке, а async-путь (wait=false, default) уходил в
// runAsyncReload с тем же бесконечным ожиданием. Из WebUI это выглядело как
// «пока модель не закончит ответ, ей нельзя управлять»: кнопка «Применить
// (reload)»/выгрузка висели минутами. Теперь без force отвечаем 409 +
// {busy:true, retryWithForce:true} (как unload), а с force — обрываем активные
// генерации и грузим сразу.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// loadDummyForReload загружает модель "dummy" и ждёт состояния loaded.
func loadDummyForReload(t *testing.T, url string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"name": "dummy"})
	resp, err := http.Post(url+"/api/models/load?wait=true&waitTimeoutMs=5000", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/models/load: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("load: status=%d body=%s", resp.StatusCode, string(body))
	}
	waitForDummyLoaded(t, url, 5*time.Second)
}

// waitForDummyLoaded ждёт, пока "dummy" появится в /api/models со state=loaded.
func waitForDummyLoaded(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/api/models")
		if err == nil {
			var payload struct {
				Models []struct {
					Name  string `json:"name"`
					State string `json:"state"`
				} `json:"models"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			for _, m := range payload.Models {
				if m.Name == "dummy" && m.State == "loaded" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("модель dummy не стала loaded за %s", timeout)
}

// TestR83v60_ReloadBusyWithoutForceIsConflict — занятая модель без force: быстрый
// структурный отказ вместо бесконечного ожидания.
func TestR83v60_ReloadBusyWithoutForceIsConflict(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()
	loadDummyForReload(t, srv.URL)

	// Имитируем активную генерацию по модели.
	if c := backend.InFlight(); c != nil {
		c.Inc("dummy")
		defer c.Dec("dummy")
	}

	body, _ := json.Marshal(map[string]interface{}{"name": "dummy", "contextSize": 65536})
	start := time.Now()
	resp, err := http.Post(srv.URL+"/api/models/reload?wait=false", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/models/reload: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (модель занята); body=%s", resp.StatusCode, string(raw))
	}
	if elapsed > 3*time.Second {
		t.Errorf("отказ занял %s: без force ответ обязан быть мгновенным, а не ждать генерацию", elapsed)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("ответ не JSON: %s", string(raw))
	}
	if parsed["busy"] != true || parsed["retryWithForce"] != true {
		t.Errorf("нет признаков busy/retryWithForce: %s", string(raw))
	}
	if _, ok := parsed["hint"]; !ok {
		t.Errorf("нет подсказки, что делать дальше: %s", string(raw))
	}
}

// TestR83v60_ReloadBusyWithForceProceeds — с force=true отказа нет: генерации
// обрываются, reload идёт (в stub-режиме быстро).
func TestR83v60_ReloadBusyWithForceProceeds(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()
	loadDummyForReload(t, srv.URL)

	c := backend.InFlight()
	if c != nil {
		c.Inc("dummy") // генерация «висит»; force обязан её не ждать
		go func() {
			time.Sleep(100 * time.Millisecond)
			c.Dec("dummy")
		}()
	}

	body, _ := json.Marshal(map[string]interface{}{"name": "dummy", "contextSize": 65536})
	resp, err := http.Post(srv.URL+"/api/models/reload?wait=false&force=true", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/models/reload: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		t.Errorf("force=true не должен давать 409: %s", string(raw))
	}
	// Дожидаемся фоновой перезагрузки: иначе тест завершится раньше, чем
	// runAsyncReload отпустит файлы, и TempDir cleanup упадёт.
	waitForDummyLoaded(t, srv.URL, 5*time.Second)
}

// TestR83v60_ReloadForceCancelsGenerations — force обрывает активные генерации
// (CancelByModel), а не просто «пережидает» их.
func TestR83v60_ReloadForceCancelsGenerations(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()
	loadDummyForReload(t, srv.URL)

	gen := backend.ActiveGenerations()
	if gen == nil {
		t.Skip("ActiveGenerations недоступен")
	}
	// Регистрируем «генерацию» так, как это делает handleChat.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ok := gen.Add("req-r83v60", "test-user", "dummy", "test-backend", cancel); !ok {
		t.Skip("не удалось зарегистрировать тестовую генерацию")
	}
	before := len(gen.Snapshot())

	body, _ := json.Marshal(map[string]interface{}{"name": "dummy", "contextSize": 65536})
	resp, err := http.Post(srv.URL+"/api/models/reload?wait=false&force=true", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/models/reload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("force=true не должен давать 409")
	}
	// Дожидаемся фоновой перезагрузки, чтобы горутина не осталась после теста.
	waitForDummyLoaded(t, srv.URL, 5*time.Second)

	select {
	case <-ctx.Done():
	default:
		t.Errorf("контекст тестовой генерации не отменён: CancelByModel не сработал (before=%d)", before)
	}
	// Запись из реестра убирает сам запрос (Remove) при своём завершении,
	// поэтому проверяем именно отмену контекста; здесь лишь фиксируем, что
	// реестр не вырос.
	if after := len(gen.Snapshot()); after > before {
		t.Errorf("активных генераций после force-reload %d, было %d", after, before)
	}
	gen.Remove("req-r83v60")
}
