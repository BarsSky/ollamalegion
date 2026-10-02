package types

import (
	"strings"
	"testing"
)

// R-Image (2026-09-27): контракт image-моделей — тесты фиксируют поведение,
// на которое опираются слой моделей/HF (internal/api, internal/cppbackend)
// и супервизор sd-server (cmd/sdworker).

func validFluxProfile() *ImageModelProfile {
	return &ImageModelProfile{
		Name:   "flux-schnell-q4",
		Family: "flux",
		Files: []ImageModelFile{
			{Role: ImageFileRoleDiffusion, Repo: "leejet/FLUX.1-schnell-gguf", Filename: "flux1-schnell-Q4_0.gguf", LocalPath: "/m/flux.gguf"},
			{Role: ImageFileRoleVae, Repo: "black-forest-labs/FLUX.1-schnell", Filename: "ae.safetensors", LocalPath: "/m/ae.safetensors"},
			{Role: ImageFileRoleT5xxl, Repo: "leejet/t5", Filename: "t5xxl-Q4_K_M.gguf", LocalPath: "/m/t5.gguf"},
		},
		Defaults: DefaultImageGenDefaults("flux"),
		Runtime:  ImageRuntime{DiffusionFA: true, OffloadToCPU: true},
	}
}

func TestDefaultImageGenDefaults_DistilledFamilies(t *testing.T) {
	// Distilled-модели обязаны иметь cfg = 1.0 (иначе картинка деградирует).
	for _, family := range []string{"sd_turbo", "sdxl_turbo", "flux", "flux2", "z_image"} {
		d := DefaultImageGenDefaults(family)
		if d.CFGScale != 1.0 {
			t.Errorf("family %s: cfg = %v, want 1.0 (distilled)", family, d.CFGScale)
		}
		if d.Steps < 1 || d.Steps > 9 {
			t.Errorf("family %s: steps = %d, want 1..9 (distilled)", family, d.Steps)
		}
	}
	if got := DefaultImageGenDefaults("sd15").Steps; got != 25 {
		t.Errorf("sd15 steps = %d, want 25", got)
	}
	if got := DefaultImageGenDefaults("z_image").Scheduler; got != "smoothstep" {
		t.Errorf("z_image scheduler = %q, want smoothstep", got)
	}
	// Дефолты должны проходить собственную валидацию.
	for _, family := range ImageModelFamilies {
		p := &ImageModelProfile{
			Name:     "m",
			Family:   family,
			Files:    []ImageModelFile{{Role: ImageFileRoleDiffusion, Repo: "r", Filename: "f"}},
			Defaults: DefaultImageGenDefaults(family),
		}
		if family == "flux" || family == "flux2" || family == "sd3" || family == "chroma" || family == "qwen_image" || family == "z_image" {
			p.Files = append(p.Files, ImageModelFile{Role: ImageFileRoleVae, Repo: "r", Filename: "v"})
		}
		if err := ValidateImageModelProfile(p); err != nil {
			t.Errorf("family %s: default profile must validate, got %v", family, err)
		}
	}
}

func TestValidateImageModelProfile_RejectsWhatEngineDoesNotCheck(t *testing.T) {
	// Движок НЕ валидирует width/height и МОЛЧА клампит steps/batch —
	// эти проверки обязаны жить на нашей стороне.
	cases := []struct {
		name   string
		mutate func(*ImageModelProfile)
	}{
		{"empty name", func(p *ImageModelProfile) { p.Name = "" }},
		{"unknown family", func(p *ImageModelProfile) { p.Family = "nope" }},
		{"no files", func(p *ImageModelProfile) { p.Files = nil }},
		{"no diffusion file", func(p *ImageModelProfile) { p.Files = p.Files[1:] }},
		{"flux without vae", func(p *ImageModelProfile) { p.Files = []ImageModelFile{p.Files[0], p.Files[2]} }},
		{"steps 0", func(p *ImageModelProfile) { p.Defaults.Steps = 0 }},
		{"steps 101", func(p *ImageModelProfile) { p.Defaults.Steps = 101 }},
		{"cfg negative", func(p *ImageModelProfile) { p.Defaults.CFGScale = -1 }},
		{"width 100 (not multiple of 64)", func(p *ImageModelProfile) { p.Defaults.Width = 100 }},
		{"height 32 (too small)", func(p *ImageModelProfile) { p.Defaults.Height = 32 }},
		{"width 8192 (too big)", func(p *ImageModelProfile) { p.Defaults.Width = 8192 }},
		{"batch 9", func(p *ImageModelProfile) { p.Defaults.BatchCount = 9 }},
		{"duplicate role", func(p *ImageModelProfile) {
			p.Files = append(p.Files, ImageModelFile{Role: ImageFileRoleVae, Repo: "r", Filename: "v2"})
		}},
		{"vae tiling negative", func(p *ImageModelProfile) { p.Runtime.VaeTileSize = -1 }},
		{"offload + paramsBackend", func(p *ImageModelProfile) { p.Runtime.ParamsBackend = "cpu" }},
		{"bad split mode", func(p *ImageModelProfile) { p.Runtime.SplitMode = "tensor" }},
		{"bad seed mode", func(p *ImageModelProfile) { p.Runtime.SeedMode = "sometimes" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validFluxProfile()
			c.mutate(p)
			if err := ValidateImageModelProfile(p); err == nil {
				t.Fatalf("profile must be rejected (%s)", c.name)
			}
		})
	}

	if err := ValidateImageModelProfile(validFluxProfile()); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

func TestServerArgs_AllInOneVsDiT(t *testing.T) {
	// SDXL — all-in-one: грузится через --model, VAE/TE не отдельными файлами.
	sdxl := &ImageModelProfile{
		Name:     "sdxl-turbo-q8",
		Family:   "sdxl_turbo",
		Files:    []ImageModelFile{{Role: ImageFileRoleDiffusion, Repo: "OlegSkutte/SDXL-Turbo", Filename: "sd_xl_turbo.q8_0.gguf"}},
		Defaults: DefaultImageGenDefaults("sdxl_turbo"),
	}
	args := strings.Join(sdxl.ServerArgs(), " ")
	if !strings.Contains(args, "--model sd_xl_turbo.q8_0.gguf") {
		t.Errorf("all-in-one must use --model, got: %s", args)
	}
	if strings.Contains(args, "--diffusion-model") {
		t.Errorf("all-in-one must NOT use --diffusion-model, got: %s", args)
	}

	// FLUX — DiT: --diffusion-model + --vae + text encoder.
	flux := strings.Join(validFluxProfile().ServerArgs(), " ")
	for _, want := range []string{"--diffusion-model /m/flux.gguf", "--vae /m/ae.safetensors", "--t5xxl /m/t5.gguf", "--diffusion-fa", "--offload-to-cpu"} {
		if !strings.Contains(flux, want) {
			t.Errorf("flux args missing %q: %s", want, flux)
		}
	}
}

func TestServerArgs_SeedHandling(t *testing.T) {
	// Ловушка №1: OpenAI-путь sd-server не читает seed и берёт 42.
	// По умолчанию мы обязаны передать --seed -1 (рандом на запрос).
	p := validFluxProfile()
	if args := strings.Join(p.ServerArgs(), " "); !strings.Contains(args, "--seed -1") {
		t.Errorf("default seed mode must pass --seed -1, got: %s", args)
	}

	// Фиксированный seed — ровно как в профиле, без -1.
	p.Runtime.SeedMode = "fixed"
	p.Defaults.Seed = 12345
	if args := strings.Join(p.ServerArgs(), " "); !strings.Contains(args, "--seed 12345") {
		t.Errorf("fixed seed mode must pass profile seed, got: %s", args)
	}
}

func TestServerArgs_VAETilingAndClipSkip(t *testing.T) {
	p := validFluxProfile()
	p.Runtime.OffloadToCPU = false
	p.Runtime.VaeTiling = true
	p.Runtime.VaeTileSize = 512
	p.Runtime.ParamsBackend = "diffusion=disk"
	p.Defaults.ClipSkip = 2
	args := strings.Join(p.ServerArgs(), " ")

	// ParamsBackend имеет приоритет над шорткатом --offload-to-cpu.
	if strings.Contains(args, "--offload-to-cpu") {
		t.Errorf("paramsBackend must replace --offload-to-cpu: %s", args)
	}
	for _, want := range []string{"--params-backend diffusion=disk", "--vae-tiling", "--vae-tile-size 512", "--clip-skip 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}

	// clip_skip <= 1 не передаём: у sd.cpp clip_skip=1 даёт пустые картинки (ST).
	p.Defaults.ClipSkip = 1
	if args := strings.Join(p.ServerArgs(), " "); strings.Contains(args, "--clip-skip") {
		t.Errorf("clipSkip <= 1 must not be passed, got: %s", args)
	}
}

func TestServerArgs_SeedModeAndBundleDir(t *testing.T) {
	p := validFluxProfile()
	if !p.SeedIsRandom() {
		t.Error("default profile must resolve seed per request")
	}
	if got := p.BundleDir("/models"); got != "/models/flux-schnell-q4" && got != `\models\flux-schnell-q4` {
		t.Logf("bundle dir (platform separator): %s", got)
	}
	if got := p.BundleDir("/models"); !strings.HasSuffix(got, "flux-schnell-q4") {
		t.Errorf("bundle dir must end with profile name, got %s", got)
	}
}
