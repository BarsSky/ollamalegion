package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// img2img / inpaint: POST /v1/images/edits и POST /sdapi/v1/img2img
// ============================================================
//
// Мок движка (mockEngine) ЗАПОМИНАЕТ нативный img_gen, поэтому здесь
// проверяется не «200 пришёл», а что именно уехало на провод: init_image,
// mask_image, strength, sample_params, batch_count и seed.

// tinyPNGBytes — байты 1x1 PNG (const tinyPNG из handlers_test.go).
func tinyPNGBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatalf("tinyPNG не декодируется: %v", err)
	}
	return raw
}

// pngBytes — валидный PNG w×h (для проверки геометрии init-картинки).
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

type multipartPart struct {
	field string
	raw   []byte
}

// editsRequest — multipart-запрос ровно в том виде, в каком его шлют клиенты
// (файловые части + обычные поля).
func editsRequest(t *testing.T, fields map[string]string, files []multipartPart) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	for _, f := range files {
		fw, err := mw.CreateFormFile(f.field, f.field+".png")
		if err != nil {
			t.Fatalf("create form file %s: %v", f.field, err)
		}
		if _, err := fw.Write(f.raw); err != nil {
			t.Fatalf("write file %s: %v", f.field, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func serveEdits(t *testing.T, app *App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	app.setupRouter().ServeHTTP(rec, req)
	return rec
}

// seedFromPrompt — seed, который уехал в движок внутри <sd_cpp_extra_args>
// (OpenAI-поверхность: поле seed движок не читает).
func seedFromPrompt(t *testing.T, prompt string) int64 {
	t.Helper()
	const open, closeTag = "<sd_cpp_extra_args>", "</sd_cpp_extra_args>"
	i := strings.Index(prompt, open)
	if i < 0 {
		t.Fatalf("seed не инжектирован в prompt: %q", prompt)
	}
	rest := prompt[i+len(open):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		t.Fatalf("блок extra_args не закрыт: %q", prompt)
	}
	var payload struct {
		Seed int64 `json:"seed"`
	}
	if err := json.Unmarshal([]byte(rest[:j]), &payload); err != nil {
		t.Fatalf("внутри extra_args не JSON: %v (%q)", err, rest[:j])
	}
	return payload.Seed
}

// --- OpenAI /v1/images/edits --------------------------------------------------

// --- OpenAI /v1/images/variations --------------------------------------------

// TestOpenAI_ImagesVariations_EmptyPromptAndDefaultStrength — R-Image (2026-10-02):
// вариации = img2img с ПУСТЫМ промптом и strength по умолчанию 0.5.
// Отличие от edits: prompt не обязателен, а strength подставляется, если не задан.
func TestOpenAI_ImagesVariations_EmptyPromptAndDefaultStrength(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 128, 128)

	// Промпта нет вовсе — это и есть смысл variations. Собираем запрос прямо
	// на variations: editsRequest готовит multipart, путь подменяем.
	req := editsRequest(t, map[string]string{"n": "1"}, []multipartPart{{"image[]", src}})
	req.URL.Path = "/v1/images/variations"
	rec := serveEdits(t, app, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("variations без prompt: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got := engine.lastRequest()
	if got.InitImage == "" {
		t.Fatal("init_image не доехал до движка")
	}
	if got.Strength == nil {
		t.Fatal("strength обязан подставляться для variations (0.5)")
	}
	if *got.Strength != defaultVariationsStrength {
		t.Fatalf("strength = %v, want %v", *got.Strength, defaultVariationsStrength)
	}

	// Явный strength уважается.
	req2 := editsRequest(t, map[string]string{"strength": "0.9"}, []multipartPart{{"image[]", src}})
	req2.URL.Path = "/v1/images/variations"
	if rec2 := serveEdits(t, app, req2); rec2.Code != http.StatusOK {
		t.Fatalf("variations с явным strength: %d %s", rec2.Code, rec2.Body.String())
	}
	if got2 := engine.lastRequest(); got2.Strength == nil || *got2.Strength != 0.9 {
		t.Fatalf("явный strength не доехал: %+v", got2.Strength)
	}

	// А edits по-прежнему ТРЕБУЕТ prompt (поведение не сломано).
	rec3 := serveEdits(t, app, editsRequest(t, map[string]string{}, []multipartPart{{"image[]", src}}))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("edits без prompt: status = %d, want 400", rec3.Code)
	}
}

func TestOpenAI_ImagesEdits_MultipartImageArray(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	first := pngBytes(t, 128, 192)
	second := pngBytes(t, 64, 64)
	mask := pngBytes(t, 128, 192)

	rec := serveEdits(t, app, editsRequest(t,
		map[string]string{"prompt": "make it snow", "n": "2", "model": "dall-e-2"},
		[]multipartPart{
			{"image[]", first},
			{"image[]", second},
			{"mask", mask},
		}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Created      int64  `json:"created"`
		OutputFormat string `json:"output_format"`
		Data         []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Seed int64 `json:"seed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if resp.Created == 0 || resp.OutputFormat != "png" {
		t.Errorf("created/output_format = %+v", resp)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data = %d элементов, want 2 (n=2)", len(resp.Data))
	}
	for i, d := range resp.Data {
		if _, err := base64.StdEncoding.DecodeString(d.B64JSON); err != nil {
			t.Fatalf("data[%d].b64_json не декодируется: %v", i, err)
		}
	}
	if resp.Seed <= 0 {
		t.Fatalf("seed = %d, want positive", resp.Seed)
	}

	req := engine.lastRequest()
	if req.InitImage != base64.StdEncoding.EncodeToString(first) {
		t.Fatalf("init_image — не ПЕРВОЕ изображение из image[] (len=%d, want=%d)",
			len(req.InitImage), len(base64.StdEncoding.EncodeToString(first)))
	}
	// Маска доехала как mask_image и приведена к 1 каналу (контракт движка).
	if req.MaskImage == "" {
		t.Fatal("mask не доехал как mask_image")
	}
	mraw, err := base64.StdEncoding.DecodeString(req.MaskImage)
	if err != nil {
		t.Fatalf("mask_image не base64: %v", err)
	}
	mimg, _, err := image.Decode(bytes.NewReader(mraw))
	if err != nil {
		t.Fatalf("mask_image не декодируется: %v", err)
	}
	if _, ok := mimg.(*image.Gray); !ok {
		t.Fatalf("mask_image = %T, want *image.Gray (1 канал)", mimg)
	}
	if b := mimg.Bounds(); b.Dx() != 128 || b.Dy() != 192 {
		t.Fatalf("геометрия маски = %dx%d, want 128x192", b.Dx(), b.Dy())
	}
	if req.BatchCount != 2 {
		t.Fatalf("batch_count = %d, want 2", req.BatchCount)
	}
	// Размер: size не передан → геометрия первого изображения (128x192).
	if req.Width != 128 || req.Height != 192 {
		t.Fatalf("size = %dx%d, want 128x192 (геометрия init-картинки)", req.Width, req.Height)
	}
	// OpenAI-путь: seed обязан быть и в поле, и в prompt (движок читает prompt).
	if req.Seed != resp.Seed {
		t.Errorf("seed на проводе = %d, в ответе = %d", req.Seed, resp.Seed)
	}
	if got := seedFromPrompt(t, req.Prompt); got != resp.Seed {
		t.Errorf("seed в <sd_cpp_extra_args> = %d, want %d", got, resp.Seed)
	}
}

func TestOpenAI_ImagesEdits_LegacyImageField(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 128, 128)

	rec := serveEdits(t, app, editsRequest(t,
		map[string]string{"prompt": "x", "size": "512x512"},
		[]multipartPart{{"image", src}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := engine.lastRequest()
	if req.InitImage != base64.StdEncoding.EncodeToString(src) {
		t.Fatalf("legacy-поле image не ушло как init_image")
	}
	if req.Width != 512 || req.Height != 512 {
		t.Fatalf("явный size обязан побеждать геометрию картинки: %dx%d", req.Width, req.Height)
	}
}

// Data-URL прямо в form-value (часть SDK-обёрток так делает).
func TestOpenAI_ImagesEdits_DataURLFormValue(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 128, 128)

	rec := serveEdits(t, app, editsRequest(t,
		map[string]string{
			"prompt": "x",
			"image":  "data:image/png;base64," + base64.StdEncoding.EncodeToString(src),
		}, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := engine.lastRequest().InitImage; got != base64.StdEncoding.EncodeToString(src) {
		t.Fatalf("data-URL из form-value не нормализован: len=%d", len(got))
	}
}

func TestOpenAI_ImagesEdits_SeedDiffersBetweenRequests(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 128, 128)
	send := func(prompt string) int64 {
		rec := serveEdits(t, app, editsRequest(t,
			map[string]string{"prompt": prompt}, []multipartPart{{"image[]", src}}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return seedFromPrompt(t, engine.lastRequest().Prompt)
	}
	a, b := send("cat"), send("dog")
	if a == b {
		t.Fatalf("seed на проводе одинаковый (%d): ловушка №1 (дефолт 42) не закрыта", a)
	}
	if a <= 0 || b <= 0 {
		t.Fatalf("seed обязан быть положительным: %d, %d", a, b)
	}
}

func TestOpenAI_ImagesEdits_Validation(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 64, 64)

	// Нет prompt.
	rec := serveEdits(t, app, editsRequest(t, map[string]string{"prompt": "  "}, []multipartPart{{"image[]", src}}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "prompt_required") {
		t.Fatalf("пустой prompt: %d %s", rec.Code, rec.Body.String())
	}
	// Нет изображения.
	rec = serveEdits(t, app, editsRequest(t, map[string]string{"prompt": "x"}, nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "image_required") {
		t.Fatalf("без изображения: %d %s", rec.Code, rec.Body.String())
	}
	// Неподдерживаемый output_format.
	rec = serveEdits(t, app, editsRequest(t,
		map[string]string{"prompt": "x", "output_format": "tiff"}, []multipartPart{{"image[]", src}}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_generation_params") {
		t.Fatalf("output_format=tiff: %d %s", rec.Code, rec.Body.String())
	}
	// Не multipart.
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = serveEdits(t, app, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_content_type") {
		t.Fatalf("json вместо multipart: %d %s", rec.Code, rec.Body.String())
	}
	// GET.
	req = httptest.NewRequest(http.MethodGet, "/v1/images/edits", nil)
	rec = serveEdits(t, app, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
	// Strength вне диапазона — честная ошибка, а не молчаливый clamp.
	rec = serveEdits(t, app, editsRequest(t,
		map[string]string{"prompt": "x", "strength": "1.4"}, []multipartPart{{"image[]", src}}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_strength") {
		t.Fatalf("strength=1.4: %d %s", rec.Code, rec.Body.String())
	}
}

// Ограничение размера тела: multipart больше 32 MB не должен съедать память.
func TestOpenAI_ImagesEdits_PayloadTooLarge(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	huge := make([]byte, maxEditsRequestBytes+1024)
	rec := serveEdits(t, app, editsRequest(t, map[string]string{"prompt": "x"}, []multipartPart{{"image[]", huge}}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body = %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "payload_too_large") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// В движок ничего не ушло.
	if got := engine.lastRequest(); got.Prompt != "" {
		t.Fatalf("запрос ушёл в движок, несмотря на отказ: %+v", got)
	}
}

// OOM на encode (img2img) → 503 + hint с флагами sd.cpp.
func TestOpenAI_ImagesEdits_OOMHint(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	engine.failWith("ggml_backend_cuda: failed to allocate 2048 MB")

	rec := serveEdits(t, app, editsRequest(t,
		map[string]string{"prompt": "x"}, []multipartPart{{"image[]", pngBytes(t, 64, 64)}}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != "engine_out_of_memory" {
		t.Errorf("code = %q, want engine_out_of_memory", env.Error.Code)
	}
	for _, want := range []string{"--vae-tiling", "--vae-conv-direct"} {
		if !strings.Contains(env.Error.Hint, want) {
			t.Errorf("hint обязан содержать %q: %q", want, env.Error.Hint)
		}
	}
}

// OPTIONS на новых маршрутах (кнопка Connect у клиентов) → 204.
func TestEditsRoutes_OptionsNoContent(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()
	for _, path := range []string{"/v1/images/edits", "/sdapi/v1/img2img"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "http://localhost:8000")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d, want 204", path, rec.Code)
		}
	}
}

// --- A1111 /sdapi/v1/img2img --------------------------------------------------

func TestSDAPI_Img2Img(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := pngBytes(t, 128, 128)

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":             "cyberpunk city",
		"negative_prompt":    "blurry",
		"init_images":        []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(src)},
		"denoising_strength": 0.45,
		"steps":              12,
		"cfg_scale":          6.5,
		"seed":               -1,
		"batch_size":         2,
		"sampler_name":       "euler",
		"scheduler":          "karras",
		"clip_skip":          2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Images     []string       `json:"images"`
		Parameters map[string]any `json:"parameters"`
		Info       string         `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Images) != 2 {
		t.Fatalf("images = %d, want 2 (batch_size)", len(resp.Images))
	}
	if _, err := base64.StdEncoding.DecodeString(resp.Images[0]); err != nil {
		t.Fatalf("images[0] не base64: %v", err)
	}
	// info — JSON-СТРОКА (LibreChat/SillyTavern её парсят).
	var info struct {
		Width             int      `json:"width"`
		Height            int      `json:"height"`
		Seed              int64    `json:"seed"`
		Steps             int      `json:"steps"`
		SamplerName       string   `json:"sampler_name"`
		AllSeeds          []int64  `json:"all_seeds"`
		Infotexts         []string `json:"infotexts"`
		DenoisingStrength float64  `json:"denoising_strength"`
	}
	if err := json.Unmarshal([]byte(resp.Info), &info); err != nil {
		t.Fatalf("info не JSON-строка: %v (%q)", err, resp.Info)
	}
	if info.Width != 128 || info.Height != 128 {
		t.Errorf("info размер = %dx%d, want 128x128 (геометрия init-картинки)", info.Width, info.Height)
	}
	if info.Seed <= 0 || len(info.AllSeeds) == 0 || info.Infotexts == nil {
		t.Errorf("info неполон: %+v", info)
	}
	if info.Steps != 12 || info.SamplerName != "euler" {
		t.Errorf("info steps/sampler = %d/%q", info.Steps, info.SamplerName)
	}
	if info.DenoisingStrength != 0.45 {
		t.Errorf("info.denoising_strength = %v, want 0.45", info.DenoisingStrength)
	}
	if got := resp.Parameters["denoising_strength"]; got != 0.45 {
		t.Errorf("parameters.denoising_strength = %v", got)
	}

	req := engine.lastRequest()
	if req.InitImage != base64.StdEncoding.EncodeToString(src) {
		t.Fatalf("init_images[0] не ушёл как init_image (data-URL не срезан?)")
	}
	if req.Strength == nil || *req.Strength != 0.45 {
		t.Fatalf("denoising_strength → strength: %v", req.Strength)
	}
	if req.BatchCount != 2 {
		t.Fatalf("batch_count = %d, want 2", req.BatchCount)
	}
	// A1111-путь: seed ОБЫЧНЫМ полем и положительный, без блока в prompt.
	if strings.Contains(req.Prompt, "<sd_cpp_extra_args>") {
		t.Fatalf("A1111-путь не должен инжектить seed в prompt: %q", req.Prompt)
	}
	if req.Seed <= 0 {
		t.Fatalf("seed = %d, want positive (иначе overflow движка)", req.Seed)
	}
	if req.SampleParams == nil || req.SampleParams.SampleSteps != 12 ||
		req.SampleParams.SampleMethod != "euler" || req.SampleParams.Scheduler != "karras" {
		t.Fatalf("sample_params потеряны: %+v", req.SampleParams)
	}
	if req.SampleParams.Guidance == nil || req.SampleParams.Guidance.TxtCfg == nil || *req.SampleParams.Guidance.TxtCfg != 6.5 {
		t.Fatalf("guidance.txt_cfg потерян: %+v", req.SampleParams.Guidance)
	}
}

func TestSDAPI_Img2Img_ClampsDenoisingStrength(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":             "x",
		"init_images":        []string{src},
		"denoising_strength": 1.4,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := engine.lastRequest()
	if req.Strength == nil || *req.Strength != 1.0 {
		t.Fatalf("strength = %v, want 1.0 (clamp, а не 400: так делает и A1111)", req.Strength)
	}
	var resp struct {
		Info string `json:"info"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Info, "denoising_strength зажат") {
		t.Errorf("в info обязана быть отметка о clamp: %s", resp.Info)
	}
}

// inpainting_mask_invert: маска инвертируется ПОПИКСЕЛЬНО (не игнорируется).
func TestSDAPI_Img2Img_MaskInvert(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))

	// Маска: белый левый пиксель, чёрный правый — классическая ч/б конвенция.
	maskImg := image.NewRGBA(image.Rect(0, 0, 2, 1))
	maskImg.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	maskImg.Set(1, 0, color.RGBA{R: 0, G: 0, B: 0, A: 255})
	var mbuf bytes.Buffer
	_ = png.Encode(&mbuf, maskImg)
	maskB64 := base64.StdEncoding.EncodeToString(mbuf.Bytes())

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":                 "x",
		"init_images":            []string{src},
		"mask":                   maskB64,
		"inpainting_mask_invert": 1, // A1111 объявляет это поле как integer
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := engine.lastRequest()
	if req.MaskImage == "" {
		t.Fatal("mask_image не доехал")
	}
	if req.MaskImage == maskB64 {
		t.Fatal("inpainting_mask_invert проигнорирован: ушла исходная маска")
	}
	raw, err := base64.StdEncoding.DecodeString(req.MaskImage)
	if err != nil {
		t.Fatalf("mask_image не base64: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mask_image не декодируется: %v", err)
	}
	if _, ok := img.(*image.Gray); !ok {
		t.Fatalf("ожидался grayscale PNG, got %T", img)
	}
	if v := img.At(0, 0).(color.Gray).Y; v != 0 {
		t.Errorf("белый пиксель маски → %d, want 0 (инверсия)", v)
	}
	if v := img.At(1, 0).(color.Gray).Y; v != 255 {
		t.Errorf("чёрный пиксель маски → %d, want 255 (инверсия)", v)
	}
}

// bool-вариант inpainting_mask_invert тоже обязан приниматься.
func TestSDAPI_Img2Img_MaskInvertBool(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))
	mask := base64.StdEncoding.EncodeToString(pngBytes(t, 8, 8))

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":                 "x",
		"init_images":            []string{src},
		"mask":                   mask,
		"inpainting_mask_invert": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bool-вариант: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if engine.lastRequest().MaskImage == mask {
		t.Fatal("bool-true не инвертировал маску")
	}
}

func TestSDAPI_Img2Img_Validation(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt": "x",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "init_images_required") {
		t.Fatalf("без init_images: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, router, http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"init_images": []string{base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "prompt_required") {
		t.Fatalf("без prompt: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, router, http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt": "x", "init_images": []string{"!!!not-base64!!!"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("битый init_image: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, router, http.MethodGet, "/sdapi/v1/img2img", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

// Маска БЕЗ инверсии тоже доезжает — уже приведённая к 1 каналу (контракт
// движка: mask_image — 1 канал, а A1111-клиенты шлют RGBA).
func TestSDAPI_Img2Img_MaskWithoutInvert(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	src := base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))
	mask := base64.StdEncoding.EncodeToString(pngBytes(t, 16, 16))

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":             "x",
		"init_images":        []string{src},
		"mask":               mask,
		"denoising_strength": 0.5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := engine.lastRequest()
	if req.MaskImage == "" {
		t.Fatal("mask не доехал как mask_image")
	}
	raw, err := base64.StdEncoding.DecodeString(req.MaskImage)
	if err != nil {
		t.Fatalf("mask_image не base64: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mask_image не декодируется: %v", err)
	}
	if _, ok := img.(*image.Gray); !ok {
		t.Fatalf("mask_image = %T, want *image.Gray (1 канал)", img)
	}
}

// OOM на encode из A1111-пути → 503 + hint (плоский конверт с `hint`).
func TestSDAPI_Img2Img_OOMHint(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	engine.failWith("CUDA error: out of memory")

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":      "x",
		"init_images": []string{base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))},
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	hint, _ := body["hint"].(string)
	for _, want := range []string{"--vae-tiling", "--vae-conv-direct", "уменьшить размер"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint обязан содержать %q: %q", want, hint)
		}
	}
}

// Обычная (не OOM) ошибка движка не должна притворяться OOM.
func TestSDAPI_Img2Img_NonOOMErrorHasNoHint(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	engine.failWith("invalid sample method")

	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/img2img", map[string]any{
		"prompt":      "x",
		"init_images": []string{base64.StdEncoding.EncodeToString(pngBytes(t, 64, 64))},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("ошибка движка обязана давать не-200: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "vae-tiling") {
		t.Fatalf("hint про OOM не должен появляться на не-OOM ошибке: %s", rec.Body.String())
	}
}
