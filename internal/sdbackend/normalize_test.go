package sdbackend

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Тесты нормализации (§12.4)
// ============================================================

// --- size ---------------------------------------------------------------------

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		w, h    int
		wantErr bool
	}{
		{"", 0, 0, false},     // дефолт LibreChat/AnythingLLM
		{"auto", 0, 0, false}, // дефолт LobeChat/Open WebUI
		{"AUTO", 0, 0, false},
		{"512x512", 512, 512, false},
		{"1024X768", 1024, 768, false},
		{"512*512", 512, 512, false},
		{"garbage", 0, 0, true},
		{"512", 0, 0, true},
		{"0x512", 0, 0, true},
		{"-64x64", 0, 0, true},
	}
	for _, c := range cases {
		w, h, err := ParseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): unexpected error %v", c.in, err)
			continue
		}
		if w != c.w || h != c.h {
			t.Errorf("ParseSize(%q) = %d,%d want %d,%d", c.in, w, h, c.w, c.h)
		}
	}
}

func TestNormalizeGeneration_SizeClampAndRound64(t *testing.T) {
	cases := []struct {
		inW, inH     int
		wantW, wantH int
	}{
		{512, 512, 512, 512},
		{4096, 4096, 4096, 4096},
		{5000, 5000, 4096, 4096}, // верхний clamp
		{10, 10, 64, 64},         // нижний clamp
		{700, 300, 640, 256},     // округление ВНИЗ до 64
		{650, 650, 640, 640},     // 650 → 640 (не 704)
		{63, 63, 64, 64},         // ниже минимума → ровно минимум
	}
	for _, c := range cases {
		got, err := NormalizeGeneration(GenerationRequest{Width: c.inW, Height: c.inH}, nil, nil)
		if err != nil {
			t.Fatalf("Normalize(%d,%d): %v", c.inW, c.inH, err)
		}
		if got.Width != c.wantW || got.Height != c.wantH {
			t.Errorf("size %dx%d → %dx%d, want %dx%d", c.inW, c.inH, got.Width, got.Height, c.wantW, c.wantH)
		}
		if got.Width%SizeMultiple != 0 || got.Height%SizeMultiple != 0 {
			t.Errorf("size %dx%d is not a multiple of %d", got.Width, got.Height, SizeMultiple)
		}
	}
}

func TestNormalizeGeneration_SizeFromProfileDefaults(t *testing.T) {
	p := &types.ImageModelProfile{
		Name: "sdxl", Family: "sdxl",
		Defaults: types.DefaultImageGenDefaults("sdxl"), // 1024x1024
	}
	got, err := NormalizeGeneration(GenerationRequest{}, p, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.Width != 1024 || got.Height != 1024 {
		t.Fatalf("size = %dx%d, want 1024x1024 (дефолт профиля)", got.Width, got.Height)
	}
	if got.Steps != 25 {
		t.Fatalf("steps = %d, want 25 (дефолт sdxl)", got.Steps)
	}
	if got.CFGScale != 7.0 {
		t.Fatalf("cfg = %v, want 7.0 (дефолт профиля)", got.CFGScale)
	}
}

func TestNormalizeGeneration_UsesEngineLimits(t *testing.T) {
	// Движок сообщает более узкие границы — обязаны уважать их, а не свои.
	limits := &CapLimits{MinWidth: 128, MaxWidth: 2048, MaxBatchCount: 4}
	got, err := NormalizeGeneration(GenerationRequest{Width: 4096, Height: 64, BatchCount: 8}, nil, limits)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.Width != 2048 {
		t.Fatalf("width = %d, want 2048 (max движка)", got.Width)
	}
	if got.Height != 128 {
		t.Fatalf("height = %d, want 128 (min движка)", got.Height)
	}
	if got.BatchCount != 4 {
		t.Fatalf("batch = %d, want 4 (max_batch_count движка)", got.BatchCount)
	}
}

// --- steps / batch ------------------------------------------------------------

func TestNormalizeGeneration_StepsClamp(t *testing.T) {
	got, _ := NormalizeGeneration(GenerationRequest{Steps: 500}, nil, nil)
	if got.Steps != MaxSteps {
		t.Fatalf("steps = %d, want %d", got.Steps, MaxSteps)
	}
	got, _ = NormalizeGeneration(GenerationRequest{Steps: -3}, nil, nil)
	if got.Steps != 20 {
		t.Fatalf("steps = %d, want дефолт 20", got.Steps)
	}
}

func TestNormalizeGeneration_BatchClamp(t *testing.T) {
	got, _ := NormalizeGeneration(GenerationRequest{BatchCount: 99}, nil, nil)
	if got.BatchCount != MaxBatchCount {
		t.Fatalf("batch = %d, want %d", got.BatchCount, MaxBatchCount)
	}
	got, _ = NormalizeGeneration(GenerationRequest{BatchCount: 0}, nil, nil)
	if got.BatchCount != 1 {
		t.Fatalf("batch = %d, want 1", got.BatchCount)
	}
}

// --- seed --------------------------------------------------------------------

// Ловушка №1 (§12.3): без seed движок берёт 42 → все картинки одинаковые.
func TestNormalizeGeneration_SeedAlwaysPositive(t *testing.T) {
	p := &types.ImageModelProfile{Name: "m", Runtime: types.ImageRuntime{SeedMode: "random"}}
	for i := 0; i < 50; i++ {
		got, err := NormalizeGeneration(GenerationRequest{}, p, nil)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got.Seed <= 0 {
			t.Fatalf("seed = %d: обязан быть положительным (иначе overflow/дефолт 42)", got.Seed)
		}
		if !got.SeedRandomized {
			t.Fatalf("seed не помечен как сгенерированный")
		}
	}
}

func TestNormalizeGeneration_SeedRandomIsRandom(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 30; i++ {
		got, _ := NormalizeGeneration(GenerationRequest{}, nil, nil)
		seen[got.Seed] = true
	}
	if len(seen) < 25 {
		t.Fatalf("seed'ы повторяются: %d уникальных из 30 (клиенты получат одинаковые картинки)", len(seen))
	}
}

func TestNormalizeGeneration_SeedFromClientIsKept(t *testing.T) {
	got, _ := NormalizeGeneration(GenerationRequest{Seed: 777, SeedProvided: true}, nil, nil)
	if got.Seed != 777 {
		t.Fatalf("seed = %d, want 777 (явный seed клиента обязан сохраняться)", got.Seed)
	}
	if got.SeedRandomized {
		t.Fatal("явный seed не должен помечаться как сгенерированный")
	}
}

func TestNormalizeGeneration_NegativeSeedResolvedToPositive(t *testing.T) {
	// A1111-клиенты шлют seed = -1 («случайный»), а движок на нём падает
	// (integer overflow → «generate_image returned no results»).
	got, _ := NormalizeGeneration(GenerationRequest{Seed: -1, SeedProvided: true}, nil, nil)
	if got.Seed <= 0 {
		t.Fatalf("seed = %d, want positive", got.Seed)
	}
}

func TestNormalizeGeneration_FixedSeedMode(t *testing.T) {
	p := &types.ImageModelProfile{
		Name:     "m",
		Runtime:  types.ImageRuntime{SeedMode: "fixed"},
		Defaults: types.ImageGenDefaults{Seed: 1337, Steps: 4, CFGScale: 1, Width: 512, Height: 512, BatchCount: 1},
	}
	got, _ := NormalizeGeneration(GenerationRequest{}, p, nil)
	if got.Seed != 1337 || got.SeedRandomized {
		t.Fatalf("seed = %d randomized=%v, want fixed 1337", got.Seed, got.SeedRandomized)
	}
}

// --- output_format -----------------------------------------------------------

func TestNormalizeOutputFormat(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "png"}, {"png", "png"}, {"PNG", "png"},
		{"jpeg", "jpeg"}, {"jpg", "jpeg"},
		{"webp", "webp"}, {"WEBP", "webp"},
	} {
		got, err := NormalizeOutputFormat(c.in)
		if err != nil {
			t.Errorf("NormalizeOutputFormat(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeOutputFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := NormalizeOutputFormat("webm"); err == nil {
		t.Error("webm (видео) должен отклоняться в Phase 3")
	}
	if _, err := NormalizeOutputFormat("tiff"); err == nil {
		t.Error("tiff должен отклоняться")
	}
}

func TestNormalizeGeneration_OutputCompressionClamp(t *testing.T) {
	comp := 500
	got, err := NormalizeGeneration(GenerationRequest{OutputCompression: &comp}, nil, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.OutputCompression == nil || *got.OutputCompression != 100 {
		t.Fatalf("compression = %v, want 100", got.OutputCompression)
	}
	comp = -5
	got, _ = NormalizeGeneration(GenerationRequest{OutputCompression: &comp}, nil, nil)
	if *got.OutputCompression != 0 {
		t.Fatalf("compression = %d, want 0", *got.OutputCompression)
	}
}

// --- инъекция seed в prompt --------------------------------------------------

func TestInjectSeedIntoPrompt(t *testing.T) {
	got := InjectSeedIntoPrompt("a cat", 4242)
	if !strings.HasPrefix(got, "a cat ") {
		t.Fatalf("prompt повреждён: %q", got)
	}
	if !strings.Contains(got, "<sd_cpp_extra_args>") || !strings.Contains(got, "</sd_cpp_extra_args>") {
		t.Fatalf("блок extra_args отсутствует: %q", got)
	}
	inner := got[strings.Index(got, extraArgsOpen)+len(extraArgsOpen) : strings.Index(got, extraArgsClose)]
	var parsed map[string]any
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil {
		t.Fatalf("внутренний JSON невалиден: %v (%q)", err, inner)
	}
	if seed, ok := parsed["seed"].(float64); !ok || int64(seed) != 4242 {
		t.Fatalf("seed в блоке = %v, want 4242", parsed["seed"])
	}
	// Пустой prompt — блок всё равно обязан быть валидным.
	if out := InjectSeedIntoPrompt("", 7); !strings.HasPrefix(out, extraArgsOpen) {
		t.Fatalf("пустой prompt: %q", out)
	}
}

func TestBuildImgGenRequest_SeedInjectionOnlyForOpenAIPath(t *testing.T) {
	n, _ := NormalizeGeneration(GenerationRequest{Prompt: "cat", Seed: 999, SeedProvided: true, Steps: 9, CFGScale: 3.5, Sampler: "euler", Scheduler: "karras"}, nil, nil)

	withInject := BuildImgGenRequest(n, true)
	if !strings.Contains(withInject.Prompt, "<sd_cpp_extra_args>") {
		t.Fatal("OpenAI-путь обязан инжектить seed в prompt (движок не читает поле seed)")
	}
	if withInject.Seed != 999 {
		t.Fatalf("seed = %d", withInject.Seed)
	}
	withoutInject := BuildImgGenRequest(n, false)
	if strings.Contains(withoutInject.Prompt, "<sd_cpp_extra_args>") {
		t.Fatal("A1111/нативный путь не должен инжектить seed в prompt")
	}
	if withoutInject.Prompt != "cat" {
		t.Fatalf("prompt = %q, want cat", withoutInject.Prompt)
	}
	if withoutInject.SampleParams == nil || withoutInject.SampleParams.SampleSteps != 9 {
		t.Fatalf("sample_params потеряны: %+v", withoutInject.SampleParams)
	}
	if withoutInject.SampleParams.Guidance == nil || withoutInject.SampleParams.Guidance.TxtCfg == nil || *withoutInject.SampleParams.Guidance.TxtCfg != 3.5 {
		t.Fatalf("guidance.txt_cfg потерян: %+v", withoutInject.SampleParams.Guidance)
	}
	if withoutInject.SampleParams.SampleMethod != "euler" || withoutInject.SampleParams.Scheduler != "karras" {
		t.Fatalf("sampler/scheduler потеряны: %+v", withoutInject.SampleParams)
	}
}

func TestBuildImgGenRequest_OmitsUnsetFields(t *testing.T) {
	// Незаданные поля не должны уезжать в движок нулями: eta/flow_shift/img_cfg
	// в JSON отсутствуют, и движок применит свои дефолты.
	n, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	req := BuildImgGenRequest(n, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "clip_skip") {
		t.Errorf("clip_skip не задан, но попал в тело: %s", s)
	}
	if strings.Contains(s, `"eta"`) || strings.Contains(s, `"flow_shift"`) || strings.Contains(s, `"img_cfg"`) {
		t.Errorf("незаданные опциональные поля не должны сериализоваться: %s", s)
	}
}

// ============================================================
// img2img / inpaint: init_image, mask_image, strength
// ============================================================

// pngB64 — валидный PNG w×h с заданным цветом (для тестов картинок).
func pngB64(t *testing.T, w, h int, c color.Color) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestNormalizeImageBase64_DataURLAndBare(t *testing.T) {
	bare := tinyPNG
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare base64", bare, bare},
		{"png data URL", "data:image/png;base64," + bare, bare},
		{"jpeg data URL", "data:image/jpeg;base64," + bare, bare},
		{"uppercase scheme", "DATA:IMAGE/PNG;BASE64," + bare, bare},
		{"whitespace/newlines", "data:image/png;base64,\n  " + bare[:8] + "\n" + bare[8:], bare},
		{"empty", "", ""},
	}
	for _, c := range cases {
		got, err := NormalizeImageBase64(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: ожидалась ошибка на пустой payload", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want чистый base64 (%q)", c.name, got, c.want)
		}
	}

	// Base64 без padding (часть JS-клиентов режет '=') тоже обязан приниматься.
	raw, _ := base64.StdEncoding.DecodeString(bare)
	unpadded := base64.RawStdEncoding.EncodeToString(raw)
	got, err := NormalizeImageBase64(unpadded)
	if err != nil {
		t.Fatalf("unpadded base64: %v", err)
	}
	if got != bare {
		t.Fatalf("unpadded → %q, want %q", got, bare)
	}

	// Не-base64 и не-base64 data URL — ошибка, а не «пропустить как есть».
	if _, err := NormalizeImageBase64("data:image/png,notbase64"); err == nil {
		t.Error("data URL без ;base64 обязан отклоняться")
	}
	if _, err := NormalizeImageBase64("!!!not base64!!!"); err == nil {
		t.Error("мусор обязан отклоняться")
	}
}

func TestImageSizeFromBase64(t *testing.T) {
	b64 := pngB64(t, 96, 64, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	w, h, err := ImageSizeFromBase64("data:image/png;base64," + b64)
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if w != 96 || h != 64 {
		t.Fatalf("size = %dx%d, want 96x64", w, h)
	}
	if _, _, err := ImageSizeFromBase64("AAAA"); err == nil {
		t.Error("невалидная картинка обязана давать ошибку (фолбэк — на дефолты профиля)")
	}
}

func TestNormalizeDenoisingStrength_MapsAndClamps(t *testing.T) {
	cases := []struct {
		in      float64
		want    float64
		clamped bool
	}{
		{0, 0, false},
		{0.35, 0.35, false},
		{1, 1, false},
		{1.7, 1, true}, // A1111 молча зажимает — и мы зажимаем (не 400)
		{-0.4, 0, true},
	}
	for _, c := range cases {
		got, clamped := NormalizeDenoisingStrength(c.in)
		if got != c.want || clamped != c.clamped {
			t.Errorf("NormalizeDenoisingStrength(%v) = %v,%v want %v,%v", c.in, got, clamped, c.want, c.clamped)
		}
	}
	if err := ValidateStrength(1.0001); err == nil {
		t.Error("ValidateStrength обязан отклонять > 1 (движок: validate())")
	}
	if err := ValidateStrength(0.5); err != nil {
		t.Errorf("ValidateStrength(0.5): %v", err)
	}
}

func TestNormalizeGeneration_StrengthOutOfRangeRejected(t *testing.T) {
	bad := 1.5
	if _, err := NormalizeGeneration(GenerationRequest{Strength: &bad}, nil, nil); err == nil {
		t.Fatal("strength=1.5 обязан отклоняться: движок отвергает значение вне [0,1]")
	}
	ok := 0.0
	got, err := NormalizeGeneration(GenerationRequest{Strength: &ok}, nil, nil)
	if err != nil {
		t.Fatalf("strength=0 валиден: %v", err)
	}
	if got.Strength == nil || *got.Strength != 0 {
		t.Fatalf("strength потерян: %v", got.Strength)
	}
}

func TestBuildImgGenRequest_InitMaskStrength(t *testing.T) {
	init := tinyPNG
	mask := pngB64(t, 2, 2, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	strength := 0.6
	norm, err := NormalizeGeneration(GenerationRequest{
		Prompt:    "cat",
		InitImage: "data:image/png;base64," + init,
		MaskImage: "data:image/png;base64," + mask,
		Strength:  &strength,
	}, nil, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	req := BuildImgGenRequest(norm, false)
	if req.InitImage != init {
		t.Fatalf("init_image = %q (data-URL-префикс обязан быть срезан)", req.InitImage)
	}
	if req.MaskImage == "" || req.MaskImage == mask {
		t.Fatalf("маска обязана быть приведена к 1 каналу (движок принимает mask_image 1 каналом)")
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
	if req.Strength == nil || *req.Strength != 0.6 {
		t.Fatalf("strength = %v, want 0.6", req.Strength)
	}
	// Без img2img-полей в JSON их быть не должно: движок применит свои дефолты.
	txt, _ := NormalizeGeneration(GenerationRequest{Prompt: "cat"}, nil, nil)
	body, _ := json.Marshal(BuildImgGenRequest(txt, false))
	for _, key := range []string{"init_image", "mask_image", "strength"} {
		if strings.Contains(string(body), key) {
			t.Errorf("%q не задан, но попал в тело: %s", key, body)
		}
	}
}

// Grayscale-маску не перекодируем, а маску A1111 (информация в АЛЬФЕ) —
// конвертируем по альфе, а не по яркости (иначе получился бы «белый лист»).
func TestNormalizeMaskBase64_AlphaConvention(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255}) // перерисовать
	src.Set(1, 0, color.RGBA{R: 255, G: 255, B: 255, A: 0})   // оставить
	var buf bytes.Buffer
	_ = png.Encode(&buf, src)

	got, err := NormalizeMaskBase64(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("normalize mask: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(got)
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("got %T, want *image.Gray", img)
	}
	if v := gray.GrayAt(0, 0).Y; v != 255 {
		t.Errorf("непрозрачный пиксель → %d, want 255 (маска в альфе)", v)
	}
	if v := gray.GrayAt(1, 0).Y; v != 0 {
		t.Errorf("прозрачный пиксель → %d, want 0 (маска в альфе)", v)
	}

	// Полностью прозрачная маска = «перерисовывать нечего» (0), а не белый лист.
	empty := image.NewRGBA(image.Rect(0, 0, 2, 1)) // все пиксели нулевые (A=0)
	var ebuf bytes.Buffer
	_ = png.Encode(&ebuf, empty)
	got2, err := NormalizeMaskBase64(base64.StdEncoding.EncodeToString(ebuf.Bytes()))
	if err != nil {
		t.Fatalf("normalize empty mask: %v", err)
	}
	raw2, _ := base64.StdEncoding.DecodeString(got2)
	img2, _, _ := image.Decode(bytes.NewReader(raw2))
	if v := img2.(*image.Gray).GrayAt(0, 0).Y; v != 0 {
		t.Errorf("полностью прозрачная маска → %d, want 0", v)
	}

	// Уже grayscale — проходит без перекодирования.
	graySrc := image.NewGray(image.Rect(0, 0, 2, 1))
	graySrc.SetGray(0, 0, color.Gray{Y: 200})
	var gbuf bytes.Buffer
	_ = png.Encode(&gbuf, graySrc)
	original := base64.StdEncoding.EncodeToString(gbuf.Bytes())
	same, err := NormalizeMaskBase64(original)
	if err != nil {
		t.Fatalf("gray mask: %v", err)
	}
	if same != original {
		t.Errorf("grayscale-маска перекодирована без необходимости")
	}
}

func TestNormalizeGeneration_InvalidInitImageRejected(t *testing.T) {
	_, err := NormalizeGeneration(GenerationRequest{InitImage: "not-base64!!!"}, nil, nil)
	if err == nil {
		t.Fatal("битый init_image обязан давать ошибку до похода в движок")
	}
	if !strings.Contains(err.Error(), "init_image") {
		t.Fatalf("ошибка обязана называть поле: %v", err)
	}
}

func TestInvertMaskBase64_GrayscaleInvertsLuminance(t *testing.T) {
	// Чёрно-белая маска (белое = «перерисовать»): инверсия обязана поменять
	// местами области.
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	src.Set(1, 0, color.RGBA{R: 0, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}

	inv, err := InvertMaskBase64(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("invert: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(inv)
	if err != nil {
		t.Fatalf("результат не base64: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("результат не декодируется: %v", err)
	}
	if format != "png" {
		t.Fatalf("format = %q, want png", format)
	}
	if _, ok := img.(*image.Gray); !ok {
		t.Fatalf("ожидался grayscale PNG (у движка mask_image 1 канал), got %T", img)
	}
	if r, _, _, _ := img.At(0, 0).RGBA(); r>>8 != 0 {
		t.Errorf("белый пиксель не инвертирован: %v", r>>8)
	}
	if r, _, _, _ := img.At(1, 0).RGBA(); r>>8 != 255 {
		t.Errorf("чёрный пиксель не инвертирован: %v", r>>8)
	}
}

func TestInvertMaskBase64_AlphaConvention(t *testing.T) {
	// A1111 кладёт маску в альфу: прозрачное = не перерисовывать.
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 0})   // fully transparent
	src.Set(1, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255}) // opaque
	var buf bytes.Buffer
	_ = png.Encode(&buf, src)

	inv, err := InvertMaskBase64(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("invert: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(inv)
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v := img.At(0, 0).(color.Gray).Y; v != 255 {
		t.Errorf("прозрачный пиксель → %d, want 255 (инверсия альфы)", v)
	}
	if v := img.At(1, 0).(color.Gray).Y; v != 0 {
		t.Errorf("непрозрачный пиксель → %d, want 0 (инверсия альфы)", v)
	}
}

func TestInvertMaskBase64_GarbageRejected(t *testing.T) {
	if _, err := InvertMaskBase64("data:image/webp;base64,AAAA"); err == nil {
		t.Error("недекодируемая маска обязана давать ошибку, а не тихо вернуть исходник")
	}
}

// ============================================================
// webp (R-Image follow-up, 2026-10-02): init_image и mask
// ============================================================
//
// ПОЧЕМУ ФИКСТУРЫ — КОНСТАНТЫ, А НЕ testdata-ФАЙЛЫ И НЕ ГЕНЕРАЦИЯ В ТЕСТЕ:
// кодировщика webp в Go нет (golang.org/x/image/webp умеет только
// декодировать), поэтому «сгенерировать валидный webp кодом теста» нечем.
// Байты зафиксированы как base64 — это ровно те файлы, что собирает ImageMagick:
//
//	magick mask.pgm -define webp:lossless=true mask_lossless.webp
//	magick init.ppm -define webp:lossless=true init_lossless.webp
//	magick init.ppm -quality 90 init_lossy.webp
//
// Источники: mask.pgm = P2 2x1 «255 0» (белое = перерисовать),
// init.ppm = P3 4x2 с контрольными цветами (красный/зелёный/синий/жёлтый,
// чёрный/серый 128/белый/10-20-30).
//
// ВАЖНО: покрыты ОБА кодека — VP8L (lossless, байты 12..16 == "VP8L") и
// VP8 (lossy). Ровно потому, что декодер умеет не всё: тест на lossless не
// доказывает, что мы не отдадим движку мусор на lossy-входе.
const (
	webpMaskLosslessB64 = "UklGRh4AAABXRUJQVlA4TBIAAAAvAQAAAA8w//M///MfeMiI/gc="
	webpInitLosslessB64 = "UklGRkQAAABXRUJQVlA4TDcAAAAvA0AAAD9AmG20QZzl+X+nQSZtk79+Z38E2TYbyd3u77D5DxCJOIRkzXcQCqAxmQSC6hfR/8ADAA=="
	webpInitLossyB64    = "UklGRloAAABXRUJQVlA4IE4AAABQAgCdASoEAAIAAMASJYwCdAW4B+IBoU4KqAAA/vG/4gWRf++N//Slp7T/5oErUJyfqL14bEZcv6avv/8myo5/DT7//krkE2upaoXAAAA="
)

// webpFixtureKind — какой кодек внутри фикстуры (по FourCC контейнера).
func webpFixtureKind(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("фикстура не base64: %v", err)
	}
	if !looksLikeWebP(raw) {
		t.Fatalf("фикстура не похожа на webp (magic RIFF/WEBP): % x", raw[:12])
	}
	return string(raw[12:16])
}

// Обе фикстуры обязаны декодироваться форматом "webp" — иначе тесты ниже
// проверяли бы не то (например, что мы «поддерживаем webp», не декодируя его).
func TestWebPFixtures_DecodeConfig(t *testing.T) {
	cases := []struct {
		name string
		b64  string
		w, h int
		kind string
	}{
		{"mask lossless", webpMaskLosslessB64, 2, 1, "VP8L"},
		{"init lossless", webpInitLosslessB64, 4, 2, "VP8L"},
		{"init lossy", webpInitLossyB64, 4, 2, "VP8 "},
	}
	for _, c := range cases {
		if got := webpFixtureKind(t, c.b64); got != c.kind {
			t.Fatalf("%s: FourCC = %q, want %q", c.name, got, c.kind)
		}
		w, h, err := ImageSizeFromBase64(c.b64)
		if err != nil {
			t.Errorf("%s: ImageSizeFromBase64: %v", c.name, err)
			continue
		}
		if w != c.w || h != c.h {
			t.Errorf("%s: size = %dx%d, want %dx%d", c.name, w, h, c.w, c.h)
		}
	}
}

// webp-init обязан доехать до движка как PNG (контракт img_gen — PNG/JPEG),
// а PNG/JPEG — остаться байт-в-байт (иначе мы бы гоняли CPU на каждый запрос).
func TestNormalizeImageBase64_WebPTranscodedToPNG(t *testing.T) {
	got, err := NormalizeImageBase64("data:image/webp;base64," + webpInitLosslessB64)
	if err != nil {
		t.Fatalf("webp init: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("результат не base64: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("результат не декодируется: %v", err)
	}
	if format != "png" {
		t.Fatalf("webp-init отдан движку как %q, want png (webp в контракте init_image не заявлен)", format)
	}
	if b := img.Bounds(); b.Dx() != 4 || b.Dy() != 2 {
		t.Fatalf("bounds = %v, want 4x2", b)
	}
	// Lossless-фикстура обязана сохранить цвета пиксель-в-пиксель.
	want := []color.RGBA{
		{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255},
		{A: 255}, {R: 128, G: 128, B: 128, A: 255}, {R: 255, G: 255, B: 255, A: 255}, {R: 10, G: 20, B: 30, A: 255},
	}
	for i, w := range want {
		x, y := i%4, i/4
		r, g, b, a := img.At(x, y).RGBA()
		got8 := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
		if got8 != w {
			t.Errorf("пиксель (%d,%d) = %+v, want %+v", x, y, got8, w)
		}
	}

	// Lossy-webp тоже перекодируется (цвета проверять нельзя — сжатие с потерями).
	lossy, err := NormalizeImageBase64(webpInitLossyB64)
	if err != nil {
		t.Fatalf("lossy webp init: %v", err)
	}
	lraw, _ := base64.StdEncoding.DecodeString(lossy)
	if _, format, err := image.Decode(bytes.NewReader(lraw)); err != nil || format != "png" {
		t.Fatalf("lossy webp → format=%q err=%v, want png", format, err)
	}

	// PNG проходит без перекодирования (та же строка, что и на входе).
	if got, err := NormalizeImageBase64(tinyPNG); err != nil || got != tinyPNG {
		t.Fatalf("PNG обязан проходить байт-в-байт: err=%v equal=%v", err, got == tinyPNG)
	}
}

// webp-маска обязана превратиться в 1-канальный PNG: значение = яркость
// (маска не в альфе), инверсия = 255 − значение.
func TestMaskWebP_LosslessToGrayPNGAndInvert(t *testing.T) {
	norm, err := NormalizeMaskBase64("data:image/webp;base64," + webpMaskLosslessB64)
	if err != nil {
		t.Fatalf("normalize mask: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(norm)
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("маска не декодируется: %v", err)
	}
	if format != "png" {
		t.Fatalf("webp-маска отдана движку как %q, want png (mask_image у движка — 1 канал PNG/JPEG)", format)
	}
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("маска = %T, want *image.Gray (1 канал)", img)
	}
	if v := gray.GrayAt(0, 0).Y; v != 255 {
		t.Errorf("белый пиксель → %d, want 255", v)
	}
	if v := gray.GrayAt(1, 0).Y; v != 0 {
		t.Errorf("чёрный пиксель → %d, want 0", v)
	}

	inv, err := InvertMaskBase64(webpMaskLosslessB64)
	if err != nil {
		t.Fatalf("invert mask: %v", err)
	}
	iraw, _ := base64.StdEncoding.DecodeString(inv)
	iimg, _, err := image.Decode(bytes.NewReader(iraw))
	if err != nil {
		t.Fatalf("инвертированная маска не декодируется: %v", err)
	}
	ig, ok := iimg.(*image.Gray)
	if !ok {
		t.Fatalf("инвертированная маска = %T, want *image.Gray", iimg)
	}
	if v := ig.GrayAt(0, 0).Y; v != 0 {
		t.Errorf("инверсия белого → %d, want 0", v)
	}
	if v := ig.GrayAt(1, 0).Y; v != 255 {
		t.Errorf("инверсия чёрного → %d, want 255", v)
	}
}

// Битый webp: ошибка обязана называть формат и просить PNG — «invalid format»
// клиенту ничего не объясняет (и именно эту ветку мы обязаны проверять, если
// декодер формат не понял).
func TestWebPBrokenPayload_ClearError(t *testing.T) {
	// Валидный контейнер (RIFF/WEBP) с мусором внутри — так выглядит обрезанный
	// или повреждённый webp, а не «вообще не картинка».
	broken := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8L"), []byte("garbage-garbage-garbage")...)
	b64 := base64.StdEncoding.EncodeToString(broken)
	if looksLikeWebP(broken) != true {
		t.Fatal("тест обязан использовать payload с magic-байтами webp")
	}

	if _, _, err := ImageSizeFromBase64(b64); err == nil {
		t.Error("битый webp: ImageSizeFromBase64 обязан вернуть ошибку")
	} else if !strings.Contains(err.Error(), "webp не декодирован") || !strings.Contains(err.Error(), "PNG") {
		t.Errorf("ImageSizeFromBase64: невнятная ошибка: %v", err)
	}

	if _, err := NormalizeMaskBase64(b64); err == nil {
		t.Error("битый webp: NormalizeMaskBase64 обязан вернуть ошибку, а не отдать маску движку")
	} else if !strings.Contains(err.Error(), "webp не декодирован") || !strings.Contains(err.Error(), "PNG") {
		t.Errorf("NormalizeMaskBase64: невнятная ошибка: %v", err)
	}

	if _, err := InvertMaskBase64(b64); err == nil {
		t.Error("битый webp: InvertMaskBase64 обязан вернуть ошибку (иначе движок получит неинвертированную маску)")
	} else if !strings.Contains(err.Error(), "webp не декодирован") {
		t.Errorf("InvertMaskBase64: невнятная ошибка: %v", err)
	}

	if _, err := NormalizeImageBase64(b64); err == nil {
		t.Error("битый webp: NormalizeImageBase64 обязан вернуть ошибку, а не отправить мусор в движок")
	} else if !strings.Contains(err.Error(), "webp не декодирован") {
		t.Errorf("NormalizeImageBase64: невнятная ошибка: %v", err)
	}

	// Ошибка через NormalizeGeneration обязана называть поле запроса.
	if _, err := NormalizeGeneration(GenerationRequest{MaskImage: b64}, nil, nil); err == nil {
		t.Error("NormalizeGeneration обязан завернуть ошибку маски")
	} else if !strings.Contains(err.Error(), "mask_image") {
		t.Errorf("ошибка обязана называть поле mask_image: %v", err)
	}
}

func TestLooksLikeEncodeOOM(t *testing.T) {
	for _, s := range []string{
		"ggml_backend_cuda: failed to allocate 4096 MB",
		"CUDA error: out of memory",
		"std::bad_alloc",
		"not enough memory to encode image",
	} {
		if !LooksLikeEncodeOOM(s) {
			t.Errorf("%q обязан распознаваться как OOM", s)
		}
	}
	for _, s := range []string{"", "prompt is required", "generation failed: invalid sampler"} {
		if LooksLikeEncodeOOM(s) {
			t.Errorf("%q НЕ должен считаться OOM (иначе hint превратится в шум)", s)
		}
	}
	if !strings.Contains(Img2ImgMemoryHint, "--vae-tiling") || !strings.Contains(Img2ImgMemoryHint, "--vae-conv-direct") {
		t.Fatalf("hint обязан называть флаги движка: %q", Img2ImgMemoryHint)
	}
}

// --- fallback: limits nil -----------------------------------------------------

func TestNormalizeGeneration_NilProfileNilLimits(t *testing.T) {
	got, err := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 513, Height: 511}, nil, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.Width != 512 || got.Height != 448 {
		t.Fatalf("size = %dx%d, want 512x448 (округление до 64 вниз)", got.Width, got.Height)
	}
	if got.CFGScale != 7.0 {
		t.Fatalf("cfg = %v, want 7 (общий дефолт)", got.CFGScale)
	}
}
