// image_catalog.go — R85 (2026-10-06): каталог image-моделей кластера.
//
// ЗАЧЕМ. До этого текстовая модель узнавала про image-модели только их ИМЕНА
// (enum в схеме generate_image). Этого мало для осмысленного выбора: у моделей
// разные семейства, требования к VRAM, размер картинки, число шагов и время
// генерации — то есть модель не могла ответить на вопрос «чем sd15-q8-0 лучше
// flux-schnell-q3-k» и выбирала наугад. Каталог отдаёт то, что нужно для
// решения: состояние на воркере, дефолты профиля, оценку VRAM, размер файлов и
// человекочитаемые strengths/notes («для чего эта модель хороша»).
//
// ИСТОЧНИКИ ПОЛЕЙ (не выдумываем ничего):
//   - снимок воркера (imageResources: GET /api/image/models) — state,
//     sizeBytes, family, vram_estimate_mb от самого воркера;
//   - профиль балансера (config.ImageModelProfileStore) — defaults,
//     vramEstimateMb, notes/strengths; профиль ПРИОРИТЕТНЕЕ каталога пресетов,
//     потому что профиль — это живое намерение оператора, а каталог — лишь
//     поставляемый набор заготовок;
//   - каталог пресетов (config/image-model-catalog.json) — фолбэк для моделей,
//     у которых профиля ещё нет (модель скачали, профиль не сохраняли).
//
// ГРАНИЦЫ: файл только ЧИТАЕТ (снимки + профили) и собирает DTO. Загрузку
// модели здесь не делаем — это image_tool.go (по решению оператора), а гейт
// VRAM — image_resources.beforeGeneration. Каталог обязан быть дешёвым: он
// вызывается и API-хендлером, и схемой инструмента на каждый запрос чата.
package balancer

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

// imageCatalogCacheTTL — TTL кэша каталога.
//
// ПОЧЕМУ 5 СЕКУНД (как imageMetricsStaleAfter): каталог читают и API, и
// инструмент на КАЖДЫЙ запрос чата. Без кэша каждый такой запрос делал бы
// синхронный опрос воркеров (ensureFreshFor) — на живом стенде это лишние
// HTTP-запросы в sdworker там, где данные меняются только при load/unload.
// 5 с — тот же порог, по которому imageResources считает снимок свежим, то есть
// кэш каталога не может быть «старее», чем кэш метрик под ним.
const imageCatalogCacheTTL = 5 * time.Second

// ============================================================
// JSON-контракт GET /api/v1/image/models/catalog
// ============================================================

// ImageCatalog — каталог моделей кластера (один ответ по всем живым
// image_cpp-бэкендам). Его же использует инструмент list_image_models.
type ImageCatalog struct {
	// GeneratedAt — момент СБОРА (не момент отдачи из кэша).
	GeneratedAt time.Time `json:"generatedAt"`
	// Cached — ответ отдан из кэша (данные те же, что при последнем сборе).
	// Клиенту полезно для отладки «почему модель не видна после load».
	Cached bool `json:"cached"`
	// CacheTTLSeconds — текущий TTL кэша: насколько каталог может отставать.
	CacheTTLSeconds int `json:"cacheTtlSeconds"`

	Backends []ImageCatalogBackend `json:"backends"`
	Models   []ImageCatalogModel   `json:"models"`
	Limits   ImageCatalogLimits    `json:"limits"`

	// HasLoadedModel — хотя бы на одном бэкенде есть модель в VRAM. Прямая
	// подсказка инструменту: генерация возможна без загрузки.
	HasLoadedModel bool `json:"hasLoadedModel"`

	// Warnings — почему какой-то бэкенд не попал в каталог (нездоров,
	// недостоверный снимок). Оператору и модели это объясняет «пустой каталог».
	Warnings []string `json:"warnings,omitempty"`
}

// ImageCatalogBackend — один живой image-бэкенд кластера.
type ImageCatalogBackend struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// Status — статус бэкенда в балансере (healthy/...).
	Status string `json:"status"`
	// State — состояние воркера: loaded | loading | not_loaded | error.
	State string `json:"state"`
	// CurrentModel — модель, о которой воркер сообщает как о текущей.
	CurrentModel string `json:"currentModel,omitempty"`
	// ContractOK — воркер ответил контрактом /api/image/models (ключ models).
	ContractOK bool `json:"contractOk"`
	// VRAM — свободная/полная VRAM хоста, если воркер её сообщил (0 = неизвестно).
	VramFreeMB  int `json:"vramFreeMb,omitempty"`
	VramTotalMB int `json:"vramTotalMb,omitempty"`
	// Models — имена моделей этого бэкенда (для быстрого сопоставления).
	Models []string `json:"models"`
}

// ImageCatalogModel — одна модель на конкретном бэкенде.
//
// Плоские JSON-имена (name/backendId/state/…) выбраны сознательно: этот же
// объект уходит в tool-результат list_image_models, то есть его читает
// языковая модель, а не только человек.
type ImageCatalogModel struct {
	Name      string `json:"name"`
	BackendID string `json:"backendId"`
	Family    string `json:"family,omitempty"`
	// State: loaded | loading | not_loaded | error.
	State     string `json:"state"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// VramEstimateMB — оценка пиковой VRAM (0 = неизвестно).
	VramEstimateMB int `json:"vramEstimateMb,omitempty"`
	// VramSource — откуда оценка: profile | worker | files | unknown.
	VramSource string `json:"vramSource,omitempty"`
	// Defaults — дефолты генерации, с которыми модель будет запущена.
	Defaults types.ImageGenDefaults `json:"defaults"`
	// Loaded — модель уже в VRAM (можно генерировать прямо сейчас).
	Loaded bool `json:"loaded"`
	// Strengths — «для чего модель хороша» (одна строка для выбора модели).
	Strengths string `json:"strengths,omitempty"`
	// Notes — подробное описание (происхождение, состав, оговорки).
	Notes string `json:"notes,omitempty"`
	// Source — откуда взяты defaults/описания: profile | catalog | worker.
	Source string `json:"source,omitempty"`
	// Error — причина, по которой модель не поднялась (state=error).
	Error string `json:"error,omitempty"`
}

// ImageCatalogLimits — границы, в которых инструмент примет запрос.
// Значения — из замороженных лимитов движка (internal/sdbackend: их дублирует
// internal/imagetool, см. imagetool.Default*).
type ImageCatalogLimits struct {
	MinSide      int `json:"minSide"`
	MaxSide      int `json:"maxSide"`
	SizeMultiple int `json:"sizeMultiple"`
	MinSteps     int `json:"minSteps"`
	MaxSteps     int `json:"maxSteps"`
}

// ============================================================
// Сборка каталога
// ============================================================

// ImageCatalogFor собирает каталог моделей кластера.
//
// ctx — контекст запроса (ленивая догрузка снимков). tokenFor не нужен: снимки
// берутся из imageResources, который уже знает токен бэкенда
// (proxyRequestToBackend). Параметр оставлен у вызывающего хендлера.
//
// Возвращает ВСЕГДА непустой каталог (даже когда image-бэкендов нет): пустой
// ответ — это тоже ответ, и он не должен превращаться в null/ошибку у клиента.
func (p *Proxy) ImageCatalogFor(ctx context.Context) *ImageCatalog {
	if p == nil {
		return emptyImageCatalog()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	backends := make([]types.Backend, 0)
	for _, b := range p.GetAllBackends() {
		if b.Type == types.BackendTypeImage {
			backends = append(backends, b)
		}
	}
	// Детерминированный порядок (как в AggregateImageCapabilities): ответ API не
	// должен «дрожать» между вызовами — иначе тесты и diff'ы контракта бесполезны.
	sortBackendsByID(backends)

	key := imageCatalogCacheKey(backends)
	now := time.Now()

	imageCatalogCacheMu.Lock()
	if imageCatalogCache.valid && imageCatalogCache.key == key &&
		now.Sub(imageCatalogCache.at) < imageCatalogCacheTTL {
		hit := *imageCatalogCache.value
		hit.Cached = true
		imageCatalogCacheMu.Unlock()
		return &hit
	}
	imageCatalogCacheMu.Unlock()

	cat := p.buildImageCatalog(ctx, backends)
	cat.Cached = false
	cat.GeneratedAt = now.UTC()
	cat.CacheTTLSeconds = int(imageCatalogCacheTTL.Seconds())

	imageCatalogCacheMu.Lock()
	imageCatalogCache = imageCatalogCacheEntry{valid: true, key: key, at: now, value: cat}
	imageCatalogCacheMu.Unlock()
	return cat
}

// buildImageCatalog — собственно сборка (без кэша).
func (p *Proxy) buildImageCatalog(ctx context.Context, backends []types.Backend) *ImageCatalog {
	cat := emptyImageCatalog()
	res := p.imageResources()

	// Дедупликация моделей по (backendID, name): один и тот же bundle может
	// попасть в снимок дважды (воркер иногда отдаёт и current_model, и запись
	// в models) — две одинаковые строки каталога только путали бы модель.
	seen := map[string]bool{}

	for _, b := range backends {
		entry := ImageCatalogBackend{
			ID:     b.ID,
			Host:   b.Host,
			Port:   b.EffectiveImagePort(),
			Status: string(b.Status),
		}
		if res == nil {
			cat.Backends = append(cat.Backends, entry)
			continue
		}
		snap := res.ensureFreshFor(ctx, b.ID)
		if snap == nil {
			cat.Warnings = append(cat.Warnings,
				"backend "+b.ID+": snapshot unavailable")
			cat.Backends = append(cat.Backends, entry)
			continue
		}
		entry.State = snap.state
		entry.CurrentModel = snap.currentModel
		entry.ContractOK = snap.contractOK
		entry.VramFreeMB = snap.vramFreeMB
		entry.VramTotalMB = snap.vramTotalMB
		if !snap.contractOK || snap.lastErr != "" {
			// Снимок недостоверен: не выдумываем модели, но и бэкенд показываем —
			// оператор должен видеть, что воркер есть, а данных от него нет.
			cat.Warnings = append(cat.Warnings,
				"backend "+b.ID+": "+firstNonEmptyStr(snap.lastErr, "snapshot not trustworthy"))
			cat.Backends = append(cat.Backends, entry)
			continue
		}

		names := make([]string, 0, len(snap.models))
		for i := range snap.models {
			m := snap.models[i]
			if strings.TrimSpace(m.Name) == "" {
				continue
			}
			names = append(names, m.Name)
			if seen[b.ID+"\x00"+m.Name] {
				continue
			}
			seen[b.ID+"\x00"+m.Name] = true
			cat.Models = append(cat.Models, p.catalogModelFor(b.ID, m))
		}
		entry.Models = names
		cat.Backends = append(cat.Backends, entry)

		if snap.loaded() != nil {
			cat.HasLoadedModel = true
		}
	}

	cat.Limits = imageCatalogDefaultLimits()
	return cat
}

// catalogModelFor — одна запись каталога: снимок воркера + профиль/каталог.
func (p *Proxy) catalogModelFor(backendID string, m imageModelEntry) ImageCatalogModel {
	out := ImageCatalogModel{
		Name:           m.Name,
		BackendID:      backendID,
		Family:         m.Family,
		State:          m.State,
		SizeBytes:      m.SizeBytes,
		VramEstimateMB: m.VramEstimateMB,
		Loaded:         m.State == imageStateLoaded,
		Error:          m.Error,
		Source:         "worker",
	}
	if m.VramEstimateMB > 0 {
		out.VramSource = "worker"
	}

	profile, fromProfile := imageCatalogProfileFor(p, m.Name)
	fromCatalog := false
	if !fromProfile {
		var ok bool
		profile, ok = imageCatalogPresetFor(m.Name)
		if ok {
			fromCatalog = true
			out.Source = "catalog"
		}
	} else {
		out.Source = "profile"
	}
	if !fromProfile && !fromCatalog {
		// Ни профиля, ни пресета (модель скачали, профиль не сохраняли): отдаём
		// хотя бы дефолты семейства, чтобы модель знала, с какими шагами звать.
		out.Defaults = types.DefaultImageGenDefaults(firstNonEmptyStr(m.Family, "other"))
		if out.Strengths == "" {
			out.Strengths = imageFamilyStrengths(firstNonEmptyStr(m.Family, "other"))
		}
		return out
	}

	out.Defaults = profile.Defaults
	out.Notes = strings.TrimSpace(profile.Notes)
	out.Strengths = strings.TrimSpace(profile.Strengths)
	out.Family = firstNonEmptyStr(m.Family, profile.Family)

	// VRAM: профиль (или пресет каталога) приоритетнее числа от воркера — это
	// намерение оператора и калибровка, а не оценка самого движка; та же
	// иерархия, что в resolveVRAMEstimate. Источник без vramEstimateMb (0 = «не
	// задано») НЕ затирает живую оценку воркера: иначе заполненный «всё кроме
	// VRAM» профиль обнулял бы поле, которое воркер честно сообщил.
	wantProfile := profile.VramEstimateMB
	if wantProfile <= 0 {
		wantProfile = m.VramEstimateMB
	}
	if wantProfile > 0 {
		out.VramEstimateMB = wantProfile
		switch {
		case profile.VramEstimateMB > 0 && fromCatalog:
			out.VramSource = "catalog"
		case profile.VramEstimateMB > 0:
			out.VramSource = "profile"
		default:
			out.VramSource = "worker"
		}
	}
	if out.VramEstimateMB <= 0 {
		if sum := imageProfileBundleBytes(profile); sum > 0 {
			out.SizeBytes = sum
		}
		if out.SizeBytes > 0 {
			if mb := int(float64(out.SizeBytes) / (1024 * 1024) * imageVramFileCoefficient); mb > 0 {
				out.VramEstimateMB = mb
				out.VramSource = "files"
			}
		}
	}
	if out.Strengths == "" {
		out.Strengths = imageFamilyStrengths(out.Family)
	}
	return out
}

// imageCatalogProfileFor — профиль балансера (read-only, как r.profileFor).
//
// Отдельная функция, а не profileFor: здесь нужен ещё и ОТВЕТ «профиля нет»
// без WARN-спама на каждую модель без профиля (profileFor логирует ошибку
// загрузки хранилища — это редкий и важный случай, но вызывать его на каждую
// запись каталога не нужно).
func imageCatalogProfileFor(p *Proxy, name string) (types.ImageModelProfile, bool) {
	if p == nil || p.imageRes == nil || strings.TrimSpace(name) == "" {
		return types.ImageModelProfile{}, false
	}
	return p.imageRes.profileFor(name)
}

// imageCatalogPresetFor — пресет из поставляемого каталога (config/image-model-catalog.json).
//
// Читается на каждый промах кэша каталога: файл маленький, а кэш каталога — 5 с.
// Ошибка чтения НЕ является ошибкой каталога: каталог пресетов — необязательный
// источник (модели может не быть в нём вовсе), поэтому просто «нет данных».
func imageCatalogPresetFor(name string) (types.ImageModelProfile, bool) {
	if strings.TrimSpace(name) == "" {
		return types.ImageModelProfile{}, false
	}
	catalog, err := config.LoadImageModelCatalog("")
	if err != nil || catalog == nil {
		return types.ImageModelProfile{}, false
	}
	if p, ok := catalog.Get(name); ok {
		return config.CloneImageModelProfile(p), true
	}
	// Регистр: имя каталога bundle могло быть сохранено в другом регистре.
	lower := strings.ToLower(name)
	for _, p := range catalog.Presets {
		if strings.ToLower(p.Name) == lower {
			return config.CloneImageModelProfile(p), true
		}
	}
	return types.ImageModelProfile{}, false
}

// imageFamilyStrengths — краткая характеристика СЕМЕЙСТВА, когда оператор не
// заполнил strengths. Это знание о семействах (а не о конкретном файле), и оно
// нужно, чтобы модель вообще могла сравнивать модели при пустых описаниях.
func imageFamilyStrengths(family string) string {
	switch family {
	case "sd15", "sd21":
		return "классический SD1.5: мало VRAM, быстро, 512x512, качество базовое"
	case "sd_turbo":
		return "SD-Turbo: 1-4 шага, очень быстро, качество ниже обычного SD1.5"
	case "sdxl":
		return "SDXL: заметно лучше композиция и детали, 1024x1024, требует больше VRAM"
	case "sdxl_turbo":
		return "SDXL-Turbo: 1024x1024 за 1-4 шага, быстрее обычного SDXL"
	case "sd3":
		return "SD3: хорошая типографика и следование промпту, 1024x1024"
	case "flux", "flux2":
		return "FLUX: лучшее качество и следование промпту, но тяжелее по VRAM и времени"
	case "chroma":
		return "Chroma: FLUX-подобная архитектура, расслабленные (не gated) веса"
	case "qwen_image":
		return "Qwen-Image: сильный текст на картинке и промпт-адхеренция, тяжёлая"
	case "z_image":
		return "Z-Image Turbo: современное качество за 8 шагов, компактная"
	default:
		return ""
	}
}

// imageProfileBundleBytes — суммарный размер файлов bundle'а.
func imageProfileBundleBytes(p types.ImageModelProfile) int64 {
	var sum int64
	for _, f := range p.Files {
		if f.SizeBytes > 0 {
			sum += f.SizeBytes
		}
	}
	return sum
}

// imageCatalogDefaultLimits — границы генерации. Источник — imagetool (движок
// sd.cpp их не проверяет, значения держит наша сторона). Дублируем здесь
// ЛИТЕРАЛАМИ осознанно: internal/balancer не должен тянуть internal/imagetool
// только ради пяти чисел (и наоборот — см. комментарий в imagetool/schema.go),
// а расхождение ловит тест image_catalog_test.go.
func imageCatalogDefaultLimits() ImageCatalogLimits {
	return ImageCatalogLimits{
		MinSide:      64,
		MaxSide:      4096,
		SizeMultiple: 64,
		MinSteps:     1,
		MaxSteps:     100,
	}
}

// ============================================================
// Кэш
// ============================================================

type imageCatalogCacheEntry struct {
	valid bool
	key   string
	at    time.Time
	value *ImageCatalog
}

var (
	imageCatalogCacheMu sync.Mutex
	imageCatalogCache   imageCatalogCacheEntry
)

// ResetImageCatalogCache — сброс кэша каталога (тесты и ручная диагностика
// оператором после load/unload модели).
func ResetImageCatalogCache() {
	imageCatalogCacheMu.Lock()
	defer imageCatalogCacheMu.Unlock()
	imageCatalogCache = imageCatalogCacheEntry{}
}

// imageCatalogCacheKey — отпечаток топологии image-бэкендов.
//
// ЗАЧЕМ ПОДПИСЬ, А НЕ ТОЛЬКО TTL: кэш пакетный (один на процесс), и без подписи
// разные тесты/кластеры получали бы данные друг друга (та же причина, что у
// imageCapabilitiesSignature). Статус в подписи → переход healthy→unhealthy
// сразу инвалидирует каталог.
func imageCatalogCacheKey(backends []types.Backend) string {
	h := fnv.New64a()
	for _, b := range backends {
		_, _ = h.Write([]byte(b.ID))
		_, _ = h.Write([]byte{'@'})
		_, _ = h.Write([]byte(b.Host))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(strconv.Itoa(b.EffectiveImagePort())))
		_, _ = h.Write([]byte{'/'})
		_, _ = h.Write([]byte(b.Status))
		_, _ = h.Write([]byte{';'})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// emptyImageCatalog — каталог без данных (не nil: сериализация в null ломала бы
// клиентов, ожидающих объект).
func emptyImageCatalog() *ImageCatalog {
	return &ImageCatalog{
		Backends: []ImageCatalogBackend{},
		Models:   []ImageCatalogModel{},
		Limits:   imageCatalogDefaultLimits(),
	}
}

// sortBackendsByID — детерминированный порядок бэкендов.
func sortBackendsByID(backends []types.Backend) {
	for i := 1; i < len(backends); i++ {
		for j := i; j > 0 && backends[j-1].ID > backends[j].ID; j-- {
			backends[j-1], backends[j] = backends[j], backends[j-1]
		}
	}
}

// firstNonEmptyStr — первое непустое значение (после TrimSpace).
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
