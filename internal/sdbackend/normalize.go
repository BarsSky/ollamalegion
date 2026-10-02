package sdbackend

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
	"unicode"

	// Декодеры для init_image/mask: без регистрации формата image.DecodeConfig
	// вернёт «image: unknown format», и геометрию картинки мы не узнаем.
	_ "image/jpeg"

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
	MinImageSide  = 64
	MaxImageSide  = 4096
	MaxBatchCount = 8
	MaxSteps      = 100
	MinSteps      = 1
	SizeMultiple  = 64
	maxCFGScale   = 30.0
	// MinStrength/MaxStrength — диапазон нативного `strength` (img2img).
	//
	// ПОЧЕМУ ЭТО ЖЁСТКО: sd.cpp validate() ОТВЕРГАЕТ значение вне [0,1] —
	// то есть запрос падает уже в движке, после того как мы приняли тело.
	// Поэтому проверяем у себя и отдаём 400 с понятным текстом.
	MinStrength = 0.0
	MaxStrength = 1.0
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
	// --- img2img / inpaint (OpenAI /v1/images/edits, A1111 /sdapi/v1/img2img) ---
	//
	// InitImage — стартовое изображение (base64 БЕЗ data-URL-префикса либо
	// data-URL: нормализуется в NormalizeGeneration). Пусто = txt2img.
	InitImage string
	// MaskImage — маска inpaint (base64/data-URL, 1 канал у движка).
	// Пусто = маски нет (обычный img2img).
	MaskImage string
	// Strength — сила денойза img2img, [0,1]. nil = НЕ передавать strength
	// вовсе (движок применит собственный дефолт) — подставлять сюда 0 или 1
	// нельзя: 0 означает «ничего не менять», а не «не задано».
	Strength *float64
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

	// --- img2img: init_image / mask_image / strength -------------------------
	//
	// Нормализуем ЗДЕСЬ, а не в хендлерах: и OpenAI-edits, и A1111-img2img, и
	// наш нативный путь обязаны получить на выходе ровно то, что понимает
	// движок («голый» base64 без data-URL-префикса).
	if out.InitImage != "" {
		clean, err := NormalizeImageBase64(out.InitImage)
		if err != nil {
			return out, fmt.Errorf("invalid init_image: %w", err)
		}
		out.InitImage = clean
	}
	if out.MaskImage != "" {
		clean, err := NormalizeMaskBase64(out.MaskImage)
		if err != nil {
			return out, fmt.Errorf("invalid mask_image: %w", err)
		}
		out.MaskImage = clean
	}
	if out.Strength != nil {
		if err := ValidateStrength(*out.Strength); err != nil {
			return out, err
		}
	}

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

// ============================================================
// img2img/inpaint: init_image, mask_image, strength
// ============================================================
//
// КОНТРАКТ ДВИЖКА (POST /sdcpp/v1/img_gen):
//   init_image  — base64 ИЛИ data-URL, каналы 3 или 4;
//   mask_image  — base64 ИЛИ data-URL, 1 канал;
//   strength    — [0,1], вне диапазона validate() отвергает запрос.
//
// Мы всегда отдаём движку «голый» base64 (data-URL-префикс срезаем сами):
// так на проводе нет двусмысленности, а клиенты (A1111 шлёт data-URL
// регулярно) не зависят от того, какой именно вариант разбирает движок.

// NormalizeImageBase64 — «голый» base64 ИЛИ data-URL → чистый base64.
//
// Принимает: "iVBORw0KG...", "data:image/png;base64,iVBOR...", base64 без
// padding (часть JS-клиентов режет '='), URL-safe base64 (A1111-обёртки).
// Пробелы/переводы строк внутри payload вырезаются: base64 в JSON-строке
// иногда переносят по 76 символов.
func NormalizeImageBase64(payload string) (string, error) {
	raw, err := DecodeImageBase64(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeImageBase64 — payload (base64/data-URL) → сырые байты.
//
// Отдельная функция (а не только NormalizeImageBase64), потому что маску
// нужно ДЕКОДИРОВАТЬ в картинку (инверсия), а не просто перекодировать.
func DecodeImageBase64(payload string) ([]byte, error) {
	s := strings.TrimSpace(payload)
	if s == "" {
		return nil, fmt.Errorf("empty image payload")
	}
	// data-URL: "data:image/png;base64,<payload>". Режем по ПЕРВОЙ запятой —
	// в самом base64 запятых не бывает (алфавит A–Z a–z 0–9 + / =).
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		idx := strings.Index(s, ",")
		if idx < 0 {
			return nil, fmt.Errorf("malformed data URL: no comma separator")
		}
		meta := strings.ToLower(s[:idx])
		if !strings.Contains(meta, ";base64") {
			return nil, fmt.Errorf("unsupported data URL %q: only base64 payloads are supported", truncate(s[:idx], 40))
		}
		s = s[idx+1:]
	}
	s = stripBase64Space(s)
	if s == "" {
		return nil, fmt.Errorf("empty image payload")
	}
	// Порядок важен: Std (канонический) → RawStd (без padding) → URL-safe.
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.URLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return nil, fmt.Errorf("payload is not valid base64 (len=%d)", len(s))
}

// stripBase64Space — удаление пробельных символов (переносы строк/табов).
func stripBase64Space(s string) string {
	if !strings.ContainsFunc(s, unicode.IsSpace) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ImageSizeFromBase64 — геометрия изображения из payload (PNG/JPEG).
//
// ЗАЧЕМ: img2img без явных width/height обязан работать в размере стартовой
// картинки (так делают и A1111, и OpenAI): иначе клиент, не приславший size,
// получил бы 512x512 из дефолта профиля при исходнике 1024x1024.
//
// webp НЕ декодируем: в go.mod нет golang.org/x/image, а тянуть зависимость
// ради чтения заголовка — лишнее. Вызывающий код обязан пережить ошибку
// (фолбэк на дефолты профиля).
func ImageSizeFromBase64(payload string) (int, int, error) {
	raw, err := DecodeImageBase64(payload)
	if err != nil {
		return 0, 0, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, fmt.Errorf("decode image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("image has no dimensions (%s)", format)
	}
	return cfg.Width, cfg.Height, nil
}

// InvertMaskBase64 — инверсия маски inpaint (A1111 inpainting_mask_invert).
//
// Инверсия = 255 − значение (см. maskToGray про то, что именно берётся за
// значение). ЧЕСТНО: это не «молчаливое игнорирование флага» — если картинку
// не удалось декодировать (например webp без x/image), вызывающий обязан
// вернуть клиенту ошибку, а не отправить движку неинвертированную маску.
func InvertMaskBase64(payload string) (string, error) {
	gray, err := maskGray(payload)
	if err != nil {
		return "", err
	}
	b := gray.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			gray.SetGray(x, y, color.Gray{Y: 255 - gray.GrayAt(x, y).Y})
		}
	}
	return encodeGrayPNG(gray, "encode inverted mask")
}

// NormalizeMaskBase64 — маска в том виде, который принимает движок.
//
// ЗАЧЕМ: у движка mask_image — РОВНО 1 КАНАЛ (в отличие от init_image, где
// 3 и 4 канала равнозначны). A1111-клиенты при этом всегда шлют RGBA-PNG, а
// OpenAI — чёрно-белый PNG. Приводим оба варианта к grayscale PNG; уже
// grayscale-маску не перекодируем (маски бывают мегабайтными).
func NormalizeMaskBase64(payload string) (string, error) {
	raw, err := DecodeImageBase64(payload)
	if err != nil {
		return "", err
	}
	if img, _, derr := image.Decode(bytes.NewReader(raw)); derr == nil {
		if _, ok := img.(*image.Gray); ok {
			return base64.StdEncoding.EncodeToString(raw), nil
		}
	}
	gray, err := maskGray(payload)
	if err != nil {
		return "", err
	}
	return encodeGrayPNG(gray, "encode mask")
}

// maskGray — маска → grayscale-картинка (1 канал).
//
// КОНВЕНЦИИ, КОТОРЫЕ ПРИХОДИТСЯ СОВМЕЩАТЬ:
//   - A1111 кладёт маску в АЛЬФА-канал PNG (непрозрачное = «перерисовать»);
//   - OpenAI/sd.cpp-примеры шлют ЧЁРНО-БЕЛУЮ картинку (белое = «перерисовать»).
//
// Порядок правил (осознанный, а не «как получилось»):
//  1. альфа РАЗНАЯ по картинке → это A1111-маска: значение = альфа;
//  2. альфа везде 0 (полностью прозрачная маска) → «перерисовывать нечего»:
//     значение = 0 (яркость здесь дала бы белое и инвертировала бы смысл);
//  3. иначе → значение = яркость (Rec.601).
//
// Известная неоднозначность (описана в отчёте): картинка с ПОСТОЯННОЙ
// полупрозрачной альфой трактуется по яркости — таких масок клиенты не шлют,
// а угадать за них нельзя.
func maskGray(payload string) (*image.Gray, error) {
	raw, err := DecodeImageBase64(payload)
	if err != nil {
		return nil, err
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode mask image (%s): %w", format, err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("mask image has no pixels")
	}

	minA, maxA := uint32(0xffff), uint32(0)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a < minA {
				minA = a
			}
			if a > maxA {
				maxA = a
			}
		}
	}
	alphaVaries := maxA > 0 && minA < maxA
	allTransparent := maxA == 0

	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			var v uint32
			switch {
			case allTransparent:
				v = 0
			case alphaVaries:
				v = a
			default:
				v = (299*r + 587*g + 114*bl) / 1000
			}
			out.SetGray(x-b.Min.X, y-b.Min.Y, color.Gray{Y: uint8(v >> 8)})
		}
	}
	return out, nil
}

// encodeGrayPNG — grayscale-картинка → base64 PNG (1 канал).
func encodeGrayPNG(img *image.Gray, what string) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ValidateStrength — ошибка, если strength вне [0,1] (движок его отвергает).
func ValidateStrength(v float64) error {
	if math.IsNaN(v) || v < MinStrength || v > MaxStrength {
		return fmt.Errorf("strength %.4g is out of range [%.0f,%.0f]: sd.cpp rejects it (validate())",
			v, MinStrength, MaxStrength)
	}
	return nil
}

// ClampStrength — clamp в [0,1]; NaN → значение по умолчанию 0.75.
func ClampStrength(v float64) float64 {
	if math.IsNaN(v) {
		return 0.75
	}
	if v < MinStrength {
		return MinStrength
	}
	if v > MaxStrength {
		return MaxStrength
	}
	return v
}

// NormalizeDenoisingStrength — A1111 `denoising_strength` → нативный `strength`.
//
// Второе значение = «пришлось зажать» (для notes/info): A1111 сам молча
// зажимает значение, и клиент не должен получить 400 там, где оригинальный
// WebUI просто сгенерировал бы картинку.
func NormalizeDenoisingStrength(v float64) (float64, bool) {
	if math.IsNaN(v) {
		return 0.75, true
	}
	c := ClampStrength(v)
	return c, c != v
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
		InitImage:         n.InitImage,
		MaskImage:         n.MaskImage,
		Strength:          n.Strength,
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
