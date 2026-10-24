package sdbackend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// R120 (2026-10-15): url-режим не должен тащить base64 картинки
// ============================================================
//
// ЖИВОЙ ДЕФЕКТ (найден при живой проверке пункта 2 плана — «нехватка VRAM при
// генерации картинок должна доезжать до пользователя через инструмент балансера»).
//
// Инструмент generate_image просит у воркера `responseFormat:"url"` — ему нужна
// ССЫЛКА, а не пиксели. Но `runGeneration` клал в результат И `b64_json`, И
// `url`: воркер складывал PNG на диск (url) и тут же возвращал ту же картинку
// вторым полем в base64. Для 768x512 это ~766 KB, для 1024x1024 — уже больше
// самого лимита чтения балансера, и `image backend returned invalid JSON:
// unexpected end of JSON input` убивал УЖЕ СГЕНЕРИРОВАННУЮ картинку.
//
// Контракт (`/api/v1/image/contract`, поле response_format) прямо говорит:
// `"url" — воркер сохраняет PNG и отдаёт ссылку`. Значит в url-режиме base64 в
// ответе быть не должно.

// TestR120_URLMode_ResultHasNoBase64 — url-режим: ссылка есть, base64 нет.
func TestR120_URLMode_ResultHasNoBase64(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	// Реалистичный payload: base64 длиннее, чем был бы у 1x1, — чтобы тест
	// падал не из-за размера, а из-за САМОГО наличия поля.
	ts.fake.setImagePayload(strings.Repeat("A", 1<<20))

	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", true, false, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(res.Images))
	}
	if res.Images[0].URL == "" {
		t.Fatalf("url-режим не отдал ссылку: %+v", res.Images[0])
	}
	if res.Images[0].B64JSON != "" {
		t.Errorf("url-режим вернул base64 (%d байт) вместе со ссылкой: "+
			"это лишний мегабайт в каждом ответе и обрезка у балансера",
			len(res.Images[0].B64JSON))
	}
}

// TestR120_URLMode_JobRecordHasNoBase64 — async-путь (202 + poll) обязан вести
// себя так же: в url-режиме в записи джобы остаётся только ссылка.
func TestR120_URLMode_JobRecordHasNoBase64(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	ts.fake.setImagePayload(strings.Repeat("A", 1<<20))

	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)
	rec, err := ts.svc.Runner.Submit(context.Background(), "sd15", true, false, norm)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, err := ts.svc.Runner.Wait(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(job.Images) != 1 || job.Images[0].URL == "" {
		t.Fatalf("job.Images = %+v, want 1 со ссылкой", job.Images)
	}
	if job.Images[0].B64JSON != "" {
		t.Errorf("джоба в url-режиме хранит base64 (%d байт)", len(job.Images[0].B64JSON))
	}
}

// TestR120_URLMode_SyncResponseStaysSmall — ответ воркера в url-режиме обязан
// остаться маленьким (метаданные + ссылка), даже если движок вернул крупный PNG.
//
// ЭТО И ЕСТЬ ЖИВОЙ ДЕФЕКТ: прежний ответ был ~размер картинки, и балансер
// обрезал его по своему лимиту чтения.
func TestR120_URLMode_SyncResponseStaysSmall(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	ts.fake.setImagePayload(strings.Repeat("A", 4<<20)) // ~4 MB base64: 1024x1024 PNG

	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 1024, Height: 1024}, nil, nil)
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", true, false, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	raw, err := json.Marshal(res.Images)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > 4096 {
		t.Errorf("ответ в url-режиме = %d байт: пиксели не должны ехать в JSON "+
			"(балансер читает ответ с ограничением и крупное тело обрежет)", len(raw))
	}
}

// TestR120_B64Mode_StillCarriesBase64 — регресс-защита: режим по умолчанию
// (b64_json) обязан остаться прежним — Open WebUI и AnythingLLM читают именно
// data[].b64_json.
func TestR120_B64Mode_StillCarriesBase64(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if len(res.Images) != 1 || res.Images[0].B64JSON == "" {
		t.Fatalf("b64-режим потерял картинку: %+v", res.Images)
	}
	if _, err := decodePNG(res.Images[0].B64JSON); err != nil {
		t.Fatalf("b64 не декодируется: %v", err)
	}
}
