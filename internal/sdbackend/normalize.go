package sdbackend

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Нормализация запросов (§12.4 плана)
// ============================================================
//
// ПОЧЕМУ ЭТО ОБЯЗАТЕЛЬНО, А НЕ «ХОРОШО БЫ»: sd-server на HTTP-уровне
// НЕ валидирует width/height вообще, batch_count клампится молча до 8, steps —
// до 100, а в OpenAI-ветке поле seed не читается совсем (берётся
// default_gen_params.seed = 42 → все картинки одинаковые). Всё это проверено по
// исходникам движка (plans/2026-09-27-image-generation-backend-plan.md §12.3).
//
// Здесь нормализация ФИКСИРУЕТСЯ в одну функцию с тестами, а не размазывается
// по хендлерам: каждый клиент (OpenAI/A1111/наш нативный) идёт через неё.

// Границы, применяемые, когда движок ещё не ответил (нет capabilities).
// После readiness актуальные значения берутся из /sdcpp/v1/capabilities.limits.
const (
	MinImageSide     = 64
	MaxImageSide     = 4096
	MaxBatchCount    = 8
	MaxSteps         = 100
	MinSteps         = 1
	SizeMultiple     = 64
	maxCFGScale      = 30.0
	// minRandomSeed — нижняя граница сгенерированного seed.
	//
	// ПОЧЕМУ НЕ 0/1: у движка подтверждён integer overflow при seed = -1
	// («generate_image returned no results» + зависание), а нулевой seed
	// исторически означает «случайный» в части клиентов. 256 — безопасно
	// далеко от обоих краёв int32, который движок использует внутри.
	minRandomSeed = 256
	// maxRandomSeed — верхняя граница (< 2^31, чтобы не переполнить int).
	maxRandomSeed = math.MaxInt32 - 1
)

// GenerationRequest — нормализованный запрос генерации (вход движка).
type GenerationRequest struct {
	Prompt         string
	NegativePrompt string
	Width          int
	Height         int
	Steps          int
	CFGScale       float64
	Seed           int64
	Sampler        string
	Scheduler      string
	BatchCount     int
	ClipSkip       int
	OutputFormat   string
	// OutputCompression — 0..100; nil = не передавать (движок применит свой
	// дефолт, у PNG это no-op).
	OutputCompression *int
	Lora              []LoraRef
	// SeedProvided — клиент задал seed явно (для A1111 seed = -1 = случайный).
	SeedProvided bool
	// SeedRandomized — seed сгенерирован воркером (а не пришёл от клиента).
	SeedRandomized bool
	// Notes — что именно пришлось поправить (отдаём клиенту в info/warnings).
	Notes []string
}

// SeedRequest — разобранный seed из запроса.
type SeedRequest struct {
	Provided bool
	Value    int64
}

// NormalizeGeneration — приводит значения к безопасным (§12.4 пп.1–3,5).
//
// profile передаётся, чтобы взять дефолты модели (steps/cfg/sampler/размер) и
// режим seed; nil допустим — тогда работают общие дефолты.
func NormalizeGeneration(in GenerationRequest, profile *types.ImageModelProfile, limits *CapLimits) (GenerationRequest, error) {
	out := in
	minSide, maxSide, maxBatch, maxSteps := limitsOrDefaults(limits)

	// --- размеры: "auto"/""/WxH, clamp 64..4096, округление до 64 -----------
	out.Width = clampRoundSide(out.Width, minSide, maxSide)
	out.Height = clampRoundSide(out.Height, minSide, maxSide)
	if out.Width == 0 || out.Height == 0 {
		// Не задано (или "auto") — берём размер профиля/движка.
		dw, dh := defaultSizeFor(profile)
		if out.Width == 0 {
			out.Width = clampRoundSide(dw, minSide, maxSide)
		}
		if out.Height == 0 {
			out.Height = clampRoundSide(dh, minSide, maxSide)
		}
	}

	// --- steps: clamp 1..100 -------------------------------------------------
	defSteps := 20
	if profile != nil && profile.Defaults.Steps > 0 {
		defSteps = profile.Defaults.Steps
	}
	if out.Steps <= 0 {
		out.Steps = defSteps
	}
	if out.Steps > maxSteps {
		out.Notes = append(out.Notes, fmt.Sprintf("steps clamped to %d (движок молча клампит сам, но длина data[] тогда расходится с ожиданием клиента)", maxSteps))
		out.Steps = maxSteps
	}
	if out.Steps < MinSteps {
		out.Steps = MinSteps
	}

	// --- batch/n: clamp 1..8 -------------------------------------------------
	if out.BatchCount <= 0 {
		out.BatchCount = 1
		if profile != nil && profile.Defaults.BatchCount > 0 {
			out.BatchCount = profile.Defaults.BatchCount
		}
	}
	if out.BatchCount > maxBatch {
		out.Notes = append(out.Notes, fmt.Sprintf("n/batch clamped to %d", maxBatch))
		out.BatchCount = maxBatch
	}

	// --- cfg ----------------------------------------------------------------
	if out.CFGScale <= 0 {
		if profile != nil && profile.Defaults.CFGScale > 0 {
			out.CFGScale = profile.Defaults.CFGScale
		} else {
			out.CFGScale = 7.0
		}
	}
	if out.CFGScale > maxCFGScale {
		out.Notes = append(out.Notes, fmt.Sprintf("cfg clamped to %.1f", maxCFGScale))
		out.CFGScale = maxCFGScale
	}

	// --- sampler/scheduler --------------------------------------------------
	if strings.TrimSpace(out.Sampler) == "" && profile != nil {
		out.Sampler = profile.Defaults.Sampler
	}
	if strings.TrimSpace(out.Scheduler) == "" && profile != nil {
		out.Scheduler = profile.Defaults.Scheduler
	}
	if out.NegativePrompt == "" && profile != nil {
		out.NegativePrompt = profile.Defaults.NegativePrompt
	}
	if out.ClipSkip <= 0 && profile != nil && profile.Defaults.ClipSkip > 1 {
		out.ClipSkip = profile.Defaults.ClipSkip
	}

	// --- seed ---------------------------------------------------------------
	// R-Image ловушка №1: без явного положительного seed все картинки
	// одинаковые (OpenAI-ветка движка берёт 42). Разрешаем ВСЕГДА.
	out.Seed, out.SeedRandomized = resolveSeed(out.Seed, out.SeedProvided, profile)

	// --- output_format ------------------------------------------------------
	of, err := NormalizeOutputFormat(out.OutputFormat)
	if err != nil {
		return out, err
	}
	out.OutputFormat = of
	if out.OutputCompression != nil {
		v := *out.OutputCompression
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		out.OutputCompression = &v
	}
	return out, nil
}

// limitsOrDefaults — границы из capabilities либо константы движка.
func limitsOrDefaults(limits *CapLimits) (minSide, maxSide, maxBatch, maxSteps int) {
	minSide, maxSide = MinImageSide, MaxImageSide
	maxBatch, maxSteps = MaxBatchCount, MaxSteps
	if limits == nil {
		return
	}
	if limits.MinWidth > 0 {
		minSide = limits.MinWidth
	} else if limits.MinHeight > 0 {
		minSide = limits.MinHeight
	}
	if limits.MaxWidth > 0 {
		maxSide = limits.MaxWidth
		if limits.MaxHeight > 0 && limits.MaxHeight < maxSide {
			maxSide = limits.MaxHeight
		}
	}
	if limits.MaxBatchCount > 0 {
		maxBatch = limits.MaxBatchCount
	}
	return
}

// defaultSizeFor — размер по умолчанию из профиля, иначе 512x512.
func defaultSizeFor(profile *types.ImageModelProfile) (int, int) {
	if profile != nil {
		w, h := profile.Defaults.Width, profile.Defaults.Height
		if w > 0 || h > 0 {
			return w, h
		}
	}
	return 512, 512
}

// clampRoundSide — clamp в [min,max] и округление до кратного 64.
func clampRoundSide(v, minSide, maxSide int) int {
	if v <= 0 {
		return 0
	}
	if v < minSide {
		v = minSide
	}
	if v > maxSide {
		v = maxSide
	}
	if r := v % SizeMultiple; r != 0 {
		v -= r
		if v < minSide {
			// Округление вниз ушло ниже минимума — округляем вверх.
			v += SizeMultiple
		}
	}
	if v > maxSide {
		v -= SizeMultiple
	}
	if v < minSide {
		v = minSide
	}
	return v
}

// ParseSize — разбор поля size клиента.
//
// Принимает: "" и "auto" (дефолт LibreChat/LobeChat — это НЕ ошибка),
// "512x512", "512X512", "1024*1024". Возвращает 0,0 для auto/пустого —
// дальше NormalizeGeneration подставит дефолт профиля.
func ParseSize(size string) (w, h int, err error) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" || s == "auto" {
		return 0, 0, nil
	}
	sep := "x"
	if strings.Contains(s, "*") {
		sep = "*"
	}
	parts := strings.Split(s, sep)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid size %q: expected WxH, \"auto\" or empty", size)
	}
	w, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid size %q: width is not a number", size)
	}
	h, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid size %q: height is not a number", size)
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q: dimensions must be positive", size)
	}
	return w, h, nil
}

// NormalizeOutputFormat — png|jpeg|webp (пусто → png).
//
// Почему не отдаём ошибку на "jpg": это очевидный синоним, а клиенты (SillyTavern
// через A1111-путь) шлют его регулярно; молчаливая замена на jpeg лучше 400.
func NormalizeOutputFormat(format string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(format))
	switch f {
	case "", "png":
		return "png", nil
	case "jpeg", "jpg":
		return "jpeg", nil
	case "webp":
		return "webp", nil
	case "webm", "avi":
		// Видео-контейнеры: Phase 3 их не генерирует (vid_gen вне объёма).
		return "", fmt.Errorf("output_format %q is not supported by this worker (image generation only)", format)
	}
	return "", fmt.Errorf("unsupported output_format %q (allowed: png, jpeg, webp)", format)
}

// resolveSeed — резолв seed в положительное число.
//
// Правила:
//   - клиент задал seed > 0 → используем как есть;
//   - клиент задал seed = 0 или отрицательный → это «случайный» в API обоих
//     миров (A1111: -1; OpenAI: отсутствие поля) → генерируем;
//   - клиент не задал seed вовсе → генерируем (иначе движок возьмёт 42);
//   - профиль в режиме fixed → берём профильный seed (детерминированная
//     генерация нужна для тестов/воспроизводимости картинок).
func resolveSeed(raw int64, provided bool, profile *types.ImageModelProfile) (int64, bool) {
	if profile != nil && !profile.SeedIsRandom() && profile.Defaults.Seed >= 0 {
		return profile.Defaults.Seed, false
	}
	if provided && raw > 0 {
		return raw, false
	}
	return RandomSeed(), true
}

// RandomSeed — криптослучайный seed в безопасном диапазоне.
//
// crypto/rand, а не math/rand: несколько одновременных запросов на одном
// процессе не должны получить одинаковый seed из общего источника с
// предсказуемым состоянием (иначе «два клиента просили разное — получили одно»).
func RandomSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Фолбэк: время в наносекундах (криптостойкость здесь не нужна —
		// нужна уникальность).
		return minRandomSeed + 1
	}
	span := int64(maxRandomSeed - minRandomSeed)
	return minRandomSeed + int64(binary.BigEndian.Uint64(b[:])%uint64(span))
}

// ============================================================
// seed для OpenAI-ветки: <sd_cpp_extra_args>
// ============================================================

// extraArgsOpen / extraArgsClose — формат блока, который движок вырезает из
// prompt и парсит по схеме нативного API (examples/server/api.md).
const (
	extraArgsOpen  = "<sd_cpp_extra_args>"
	extraArgsClose = "</sd_cpp_extra_args>"
)

// extraArgsSeed — JSON для инъекции seed.
type extraArgsSeed struct {
	Seed int64 `json:"seed"`
}

// InjectSeedIntoPrompt — дописывает seed в конец prompt через
// <sd_cpp_extra_args>{"seed":N}</sd_cpp_extra_args>.
//
// ЗАЧЕМ: OpenAI-хендлер sd-server НЕ читает seed и берёт
// default_gen_params.seed = 42 (ловушка №1). Блок движок вырезает из промпта
// сам, поэтому на картинку он не влияет.
func InjectSeedIntoPrompt(prompt string, seed int64) string {
	payload, err := json.Marshal(extraArgsSeed{Seed: seed})
	if err != nil {
		return prompt
	}
	block := extraArgsOpen + string(payload) + extraArgsClose
	if strings.TrimSpace(prompt) == "" {
		return block
	}
	// Блок В КОНЕЦ и через пробел: движок ищет его по маркерам, а «склеенный»
	// текст (…warm</sd_cpp_extra_args>) испортил бы последнее слово промпта.
	return prompt + " " + block
}

// BuildImgGenRequest — нормализованный GenerationRequest → тело img_gen.
//
// injectSeedInPrompt — для OpenAI-совместимой поверхности (движок там seed не
// читает). Для нативного img_gen seed передаётся обычным полем, дублировать
// его в промпте НЕЛЬЗЯ: блок extra_args имеет приоритет и это сбивало бы с
// толку при отладке.
func BuildImgGenRequest(n GenerationRequest, injectSeedInPrompt bool) *ImgGenRequest {
	prompt := n.Prompt
	if injectSeedInPrompt {
		prompt = InjectSeedIntoPrompt(prompt, n.Seed)
	}
	req := &ImgGenRequest{
		Prompt:            prompt,
		NegativePrompt:    n.NegativePrompt,
		Width:             n.Width,
		Height:            n.Height,
		Seed:              n.Seed,
		BatchCount:        n.BatchCount,
		OutputFormat:      n.OutputFormat,
		OutputCompression: n.OutputCompression,
		Lora:              n.Lora,
	}
	if n.ClipSkip > 1 {
		req.ClipSkip = intPtr(n.ClipSkip)
	}
	if n.Sampler != "" || n.Scheduler != "" || n.Steps > 0 || n.CFGScale > 0 {
		sp := &MaskImageParams{SampleMethod: n.Sampler, Scheduler: n.Scheduler, SampleSteps: n.Steps}
		if n.CFGScale > 0 {
			sp.Guidance = &GuidanceParams{TxtCfg: floatPtr(n.CFGScale)}
		}
		req.SampleParams = sp
	}
	return req
}

// IsSSEAccept — примитивная проверка Accept на text/event-stream (SSE-вариант
// прогресса загрузки; полноценный SSE-клиент подписывается на /stream).
func IsSSEAccept(accept string) bool {
	return strings.Contains(strings.ToLower(accept), "text/event-stream")
}
