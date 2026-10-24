//go:build llama_stub

// image_tool_payload_r120_test.go — R120 (2026-10-15), найден ЖИВЫМ прогоном
// пункта 2 плана.
//
// ЖИВОЙ ДЕФЕКТ. Инструмент `generate_image` просит воркер в url-режиме, но
// воркер присылал и `url`, и `b64_json` (см. internal/sdbackend R120). Для
// 768x512 это ~766 KB, а для картинки «пошумнее» — уже больше мегабайта, то есть
// больше `imageToolResponseLimitBytes`. Тело обрезалось на середине JSON, и
// пользователь терял УЖЕ СГЕНЕРИРОВАННУЮ картинку с диагнозом
// «image backend returned invalid JSON: unexpected end of JSON input» — виноватым
// выглядел воркер, хотя обрезал ответ сам балансер.
//
// Тесты фиксируют два свойства:
//  1. крупный (но валидный) ответ воркера не должен стоить пользователю картинки;
//  2. если ответ всё-таки обрезан лимитом — ошибка обязана называть причину
//     (размер/лимит), а не притворяться «невалидным JSON».
package balancer

import (
	"context"
	"strings"
	"testing"
)

// bigURLModeBody — ответ воркера, каким его присылал url-режим до R120:
// ссылка ЕСТЬ, но рядом лежит полный base64 картинки.
func bigURLModeBody(blobBytes int) string {
	return `{"created":1,"model":"sd15-q4-0","output_format":"png","seed":7,` +
		`"width":1024,"height":1024,"steps":25,"duration_ms":24000,"data":[{"index":0,` +
		`"b64_json":"` + strings.Repeat("A", blobBytes) + `",` +
		`"url":"http://worker:18093/images/img_live_1.png"}]}`
}

// withImageToolResponseLimit — временно уменьшить предел чтения ответа воркера,
// чтобы проверить ОБНАРУЖЕНИЕ обрезки, не выделяя под это десятки мегабайт.
// Тесты пакета не параллельные, поэтому подмена безопасна.
func withImageToolResponseLimit(n int64) func() {
	old := imageToolResponseLimitBytes
	imageToolResponseLimitBytes = n
	return func() { imageToolResponseLimitBytes = old }
}

// TestR120_ImageTool_LargeInlineBase64DoesNotLoseImage — картинка есть, ссылка
// есть, значит результат обязан быть успешным: base64 в теле — не повод терять
// генерацию.
func TestR120_ImageTool_LargeInlineBase64DoesNotLoseImage(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	// 2 MiB base64 — реалистичный размер 1024x1024 PNG (было больше лимита).
	stub.setGenBody(bigURLModeBody(2 << 20))

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a red cat"})
	if err != nil {
		t.Fatalf("картинка потеряна из-за размера ответа воркера: %v", err)
	}
	if got, _ := res["url"].(string); got != "http://worker:18093/images/img_live_1.png" {
		t.Errorf("url = %q, want ссылку воркера", got)
	}
	if ok, _ := res["generated"].(bool); !ok {
		t.Errorf("результат без generated=true: %v", res)
	}
}

// TestR120_ImageTool_TruncatedResponseIsNotCalledInvalidJSON — если тело всё же
// превысило предел чтения, ошибка обязана объяснять ЭТО, а не «invalid JSON».
func TestR120_ImageTool_TruncatedResponseIsNotCalledInvalidJSON(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	// Заведомо больше предела чтения (лимит в тесте уменьшен, чтобы не
	// выделять десятки мегабайт: см. withImageToolResponseLimit).
	restore := withImageToolResponseLimit(4096)
	defer restore()
	stub.setGenBody(bigURLModeBody(8192))

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}
	_, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a red cat"})
	if err == nil {
		t.Fatal("обрезанный ответ не распознан: инструмент отчитался об успехе")
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid JSON") {
		t.Errorf("обрезанный ответ по-прежнему называется «invalid JSON»: %q — "+
			"оператор пойдёт искать ошибку в воркере", msg)
	}
	for _, want := range []string{"4096", "url"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ошибка %q не содержит %q: причина и что делать не названы", msg, want)
		}
	}
	// Ошибка обязана быть машиночитаемой: модель должна уметь передать код.
	var payload map[string]interface{}
	payload = imageToolErrorPayload(err)
	if payload["status"] != "error" {
		t.Errorf("payload.status = %v, want error", payload["status"])
	}
	if code, _ := payload["code"].(string); code == "" {
		t.Errorf("payload без кода: %v — модель не сможет объяснить пользователю", payload)
	}
}

// TestR120_ImageTool_NormalURLResponseStillWorks — регресс-защита: обычный
// маленький url-ответ (каким его присылает воркер после R120) проходит как раньше.
func TestR120_ImageTool_NormalURLResponseStillWorks(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenBody(`{"created":1,"output_format":"png","seed":3,"width":512,"height":512,` +
		`"steps":25,"duration_ms":15000,"data":[{"index":0,"url":"/images/img_2_cd.png"}]}`)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a blue dog"})
	if err != nil {
		t.Fatalf("обычный url-ответ не прошёл: %v", err)
	}
	if got, _ := res["url"].(string); got != "/images/img_2_cd.png" {
		t.Errorf("url = %q", got)
	}
}
