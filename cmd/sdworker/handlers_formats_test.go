// handlers_formats_test.go — R755 (2026-11-25): HTTP-контракт матрицы форматов.
//
// ЧТО ПРОВЕРЯЕТСЯ (имена полей — контракт UI):
//   - GET /api/image/formats?family=qwen_image → 200, engine/family/familyKnown/
//     dit/allInOne/rules/nameWarnings; safetensors-диффузия = needs_gguf,
//     GGUF = reads, MLX-предупреждение на месте;
//   - без family — консервативная матрица (familyKnown=false, safetensors
//     diffusion = warn);
//   - POST — 405.
//
// Декодируем в ТИП СЕРВЕРА (imagemodel.FormatMatrix), а не в анонимную копию:
// так тест ловит переименование/исчезновение поля в контракте, а не только
// «JSON парсится».
package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"ollama-loadbalancer/internal/imagemodel"
)

func TestImageFormatsHTTP_R755(t *testing.T) {
	app, _ := newTestApp(t, nil)
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodGet, "/api/image/formats?family=qwen_image", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("formats = %d (%s)", rec.Code, rec.Body.String())
	}
	var payload imagemodel.FormatMatrix
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if payload.Engine != "stable-diffusion.cpp" {
		t.Fatalf("engine = %q", payload.Engine)
	}
	if payload.Family != "qwen_image" || !payload.FamilyKnown || !payload.DIT || payload.AllInOne {
		t.Fatalf("family flags: %+v", payload)
	}
	byKey := map[string]string{}
	for _, r := range payload.Rules {
		byKey[r.Format+"/"+r.Role] = r.Verdict
	}
	if byKey["gguf/*"] != imagemodel.FormatReads {
		t.Fatalf("gguf/* = %q, want reads (%v)", byKey["gguf/*"], byKey)
	}
	if byKey["safetensors/diffusion"] != imagemodel.FormatNeedsGGUF {
		t.Fatalf("qwen_image safetensors/diffusion = %q, want needs_gguf", byKey["safetensors/diffusion"])
	}
	if byKey["safetensors/vae"] != imagemodel.FormatReads || byKey["safetensors/llm"] != imagemodel.FormatReads {
		t.Fatalf("компоненты safetensors обязаны читаться: vae=%q llm=%q",
			byKey["safetensors/vae"], byKey["safetensors/llm"])
	}
	if len(payload.NameWarnings) == 0 || payload.NameWarnings[0].Contains != "mlx" {
		t.Fatalf("нет MLX-предупреждения: %+v", payload.NameWarnings)
	}
	if payload.NameWarnings[0].Verdict != imagemodel.FormatUnsupported ||
		payload.NameWarnings[0].Code != "mlx_quantized" {
		t.Fatalf("MLX warning = %+v", payload.NameWarnings[0])
	}

	// Без семейства: не обещаем чтение safetensors-диффузии.
	rec = doJSON(t, router, http.MethodGet, "/api/image/formats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("formats (no family) = %d", rec.Code)
	}
	var generic imagemodel.FormatMatrix
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if generic.FamilyKnown {
		t.Fatal("без family familyKnown обязан быть false")
	}
	got := ""
	for _, r := range generic.Rules {
		if r.Format == "safetensors" && r.Role == "diffusion" {
			got = r.Verdict
		}
	}
	if got != imagemodel.FormatWarn {
		t.Fatalf("generic safetensors/diffusion = %q, want warn", got)
	}

	rec = doJSON(t, router, http.MethodPost, "/api/image/formats", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST formats = %d, want 405", rec.Code)
	}
}
