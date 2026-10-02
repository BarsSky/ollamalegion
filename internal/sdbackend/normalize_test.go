package sdbackend

import (
	"encoding/json"
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
		{"", 0, 0, false},        // дефолт LibreChat/AnythingLLM
		{"auto", 0, 0, false},    // дефолт LobeChat/Open WebUI
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
		inW, inH   int
		wantW, wantH int
	}{
		{512, 512, 512, 512},
		{4096, 4096, 4096, 4096},
		{5000, 5000, 4096, 4096},   // верхний clamp
		{10, 10, 64, 64},           // нижний clamp
		{700, 300, 640, 256},       // округление ВНИЗ до 64
		{650, 650, 640, 640},       // 650 → 640 (не 704)
		{63, 63, 64, 64},           // ниже минимума → ровно минимум
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
