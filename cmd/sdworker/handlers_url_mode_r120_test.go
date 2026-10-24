package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// R120 (2026-10-15): url-режим нативного API не тащит base64
// ============================================================
//
// ЖИВОЙ ДЕФЕКТ. `POST /api/image/generate {"sync":true,"responseFormat":"url"}`
// возвращал ссылку И полный base64 картинки. Балансер (инструмент generate_image)
// читает этот ответ с ограничением и на крупной картинке обрезал тело на середине
// JSON: пользователь терял УЖЕ СГЕНЕРИРОВАННУЮ картинку с диагнозом
// «image backend returned invalid JSON».
//
// Контракт воркера (`/api/v1/image/contract`) обещает в url-режиме ССЫЛКУ:
// «"url" — воркер сохраняет PNG и отдаёт ссылку». Тесты фиксируют, что лишних
// пикселей в ответе нет.

// TestR120_GenerateSync_URLModeHasNoBase64 — sync-путь нативного API.
func TestR120_GenerateSync_URLModeHasNoBase64(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	// Так выглядит настоящий PNG 1024x1024 после base64.
	engine.setImagePayload(strings.Repeat("A", 4<<20))

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/api/image/generate", map[string]any{
		"prompt":         "a red cat",
		"sync":           true,
		"responseFormat": "url",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL == "" {
		t.Fatalf("url-режим не отдал ссылку: %+v", resp.Data)
	}
	if resp.Data[0].B64JSON != "" {
		t.Errorf("url-режим вернул base64 (%d байт): балансер обрежет такой ответ "+
			"и пользователь потеряет готовую картинку", len(resp.Data[0].B64JSON))
	}
	// Главная проверка живого дефекта: тело ответа не масштабируется с картинкой.
	if n := rec.Body.Len(); n > 4096 {
		t.Errorf("ответ в url-режиме = %d байт, want < 4096: картинка едет в JSON", n)
	}
}

// TestR120_GenerateSync_B64ModeUnchanged — регресс-защита: без responseFormat
// (или с b64_json) ответ прежний — клиенты Open WebUI читают data[].b64_json.
func TestR120_GenerateSync_B64ModeUnchanged(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/api/image/generate", map[string]any{
		"prompt": "a red cat",
		"sync":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON == "" {
		t.Fatalf("b64-режим потерял пиксели: %+v", resp.Data)
	}
}

// TestR120_JobPoll_URLModeHasNoBase64 — async-путь: джоба в url-режиме тоже
// хранит только ссылку (иначе /api/image/jobs/{id} отдаёт мегабайты).
func TestR120_JobPoll_URLModeHasNoBase64(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	engine.setImagePayload(strings.Repeat("A", 4<<20))
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodPost, "/api/image/generate", map[string]any{
		"prompt":         "a red cat",
		"responseFormat": "url",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var submitted struct {
		ID      string `json:"id"`
		PollURL string `json:"poll_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if submitted.ID == "" {
		t.Fatalf("нет id джобы: %s", rec.Body.String())
	}

	// Поллим до терминального состояния. Конверт — {"job": {...}}.
	var envelope struct {
		Job struct {
			State  string `json:"state"`
			Images []struct {
				B64JSON string `json:"b64_json"`
				URL     string `json:"url"`
			} `json:"images"`
		} `json:"job"`
	}
	deadline := 200
	for i := 0; i < deadline; i++ {
		poll := doJSON(t, router, http.MethodGet, "/api/image/jobs/"+submitted.ID, nil)
		if poll.Code != http.StatusOK {
			t.Fatalf("poll status = %d, body=%s", poll.Code, poll.Body.String())
		}
		if err := json.Unmarshal(poll.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("poll invalid JSON: %v", err)
		}
		if envelope.Job.State == "completed" || envelope.Job.State == "failed" {
			break
		}
		// Джоба исполняется в горутине: без паузы цикл отработает быстрее,
		// чем движок успеет ответить.
		time.Sleep(25 * time.Millisecond)
	}
	job := envelope.Job
	if job.State != "completed" {
		t.Fatalf("джоба не завершилась: state=%q", job.State)
	}
	if len(job.Images) != 1 || job.Images[0].URL == "" {
		t.Fatalf("images = %+v, want 1 со ссылкой", job.Images)
	}
	if job.Images[0].B64JSON != "" {
		t.Errorf("джоба в url-режиме хранит base64 (%d байт)", len(job.Images[0].B64JSON))
	}
}
