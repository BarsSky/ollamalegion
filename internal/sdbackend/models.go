package sdbackend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Реестр image-моделей (bundle'ов)
// ============================================================
//
// Одна image-модель — это КАТАЛОГ (<modelsDir>/<name>/), внутри которого лежат
// файлы bundle'а (diffusion/VAE/text encoder'ы) и опционально profile.json с
// описанием (roles, defaults, runtime-плейсмент). Контракт профиля —
// pkg/types/image_model.go, здесь только обнаружение на диске и состояния.
//
// ПОЧЕМУ profile.json ВНУТРИ КАТАЛОГА МОДЕЛИ: bundle самодостаточен — его можно
// скопировать на другой воркер целиком, без правки глобального конфига.
// Глобальный каталог профилей на балансере (Phase 2) синкается в воркер и
// раскладывается по этим же каталогам.

// Состояния модели (отдаются в /api/image/models в snake_case, как у cppworker).
const (
	StateNotLoaded = "not_loaded"
	StateLoading   = "loading"
	StateLoaded    = "loaded"
	StateError     = "error"
)

// ProfileFileName — имя файла описания bundle'а внутри каталога модели.
const ProfileFileName = "profile.json"

// ModelInfo — запись реестра: профиль + состояние.
type ModelInfo struct {
	Name           string                 `json:"name"`
	BundlePath     string                 `json:"bundle_path,omitempty"`
	Family         string                 `json:"family"`
	State          string                 `json:"state"`
	SizeBytes      int64                  `json:"size_bytes"`
	VramEstimateMB int                    `json:"vram_estimate_mb,omitempty"`
	ActiveQueries  int64                  `json:"active_queries"`
	Disabled       bool                   `json:"disabled,omitempty"`
	Error          string                 `json:"error,omitempty"`
	LoadedAt       string                 `json:"loaded_at,omitempty"`
	LastUsedAt     string                 `json:"last_used_at,omitempty"`
	Defaults       types.ImageGenDefaults `json:"defaults"`
	Files          []types.ImageModelFile `json:"files,omitempty"`
	Notes          string                 `json:"notes,omitempty"`
}

// Registry — известные модели. Иммутабелен после Load (профили не меняются
// на лету: смена состава файлов = смена модели, а не hot-reload).
//
// ПОЧЕМУ СНИМОК (snapshot) ВМЕСТО ПРОСТОГО ПОЛЯ: с Phase 4 HF-bundle-загрузки
// реестр перезагружается В ПРОЦЕССЕ РАБОТЫ (после успешного скачивания
// bundle — см. internal/sdbackend/hf.go), а читают его HTTP-хендлеры
// (/api/image/models) параллельно из других горутин. Мутация map при чтении —
// это фатальная ошибка рантайма Go («concurrent map read and map write»),
// которая убивает воркер целиком. Поэтому Load строит НОВУЮ map и атомарно
// подменяет указатель: читатели видят либо старый, либо новый снимок целиком,
// без локов на горячем пути.
type Registry struct {
	modelsDir string
	// snapshot — текущий снимок (map иммортабельный: после Store не мутируется).
	snapshot atomic.Pointer[map[string]types.ImageModelProfile]
	// loadMu сериализует Load: два параллельных сканирования не должны
	// наперегонки подменять снимок (последний победил бы с неполными данными).
	loadMu sync.Mutex
	// warnings защищены тем же loadMu (читаются редко, только /health и список).
	warningsMu sync.RWMutex
	warnings   []string
	// lastScan — отпечаток каталога на момент последнего Load (число записей +
	// суммарное время модификации каталогов). Нужен для RefreshIfChanged: реестр
	// строится один раз при старте, а модели могут появиться на диске ПОЗЖЕ —
	// например одиночной загрузкой файла через HF (живой случай 2026-10-06:
	// модель появилась в списке только после перезапуска контейнера).
	lastScan string
}

// RefreshIfChanged — перечитать реестр, только если каталог моделей изменился.
//
// ЧТО СЧИТАЕМ ИЗМЕНЕНИЕМ: набор подкаталогов и время их модификации. Это дешёвая
// проверка (os.ReadDir + Stat, без чтения профилей и файлов) — её можно звать на
// каждый GET /api/image/models, и тогда «модель не видна до рестарта» больше не
// воспроизводится. Полный Load выполняется только при реальном изменении.
func (r *Registry) RefreshIfChanged() bool {
	if r == nil {
		return false
	}
	fp := r.dirFingerprint()
	r.loadMu.Lock()
	unchanged := fp != "" && fp == r.lastScan
	r.loadMu.Unlock()
	if unchanged {
		return false
	}
	if err := r.Load(); err != nil {
		if logger.Get() != nil {
			logger.Get().Warnw("registry refresh failed", "dir", r.modelsDir, "error", err)
		}
		return false
	}
	return true
}

// dirFingerprint — «отпечаток» каталога моделей: имена подкаталогов и их mtime.
//
// ВАЖНО ПРО ПУСТОЙ КАТАЛОГ: пустой каталог — это НОРМАЛЬНОЕ состояние (моделей
// ещё нет), и его отпечаток не должен совпадать с «каталог недоступен». Иначе
// первая же проверка RefreshIfChanged считала бы «неизвестно» изменением и
// запускала полный скан на КАЖДЫЙ запрос списка моделей. Поэтому добавляем
// разделитель после каждой записи: даже без записей отпечаток непустой ("#").
//
// Пустая строка = каталог прочитать не удалось (тогда RefreshIfChanged оставляет
// реестр как есть: Load сам обработает отсутствие каталога и вернёт предупреждение).
func (r *Registry) dirFingerprint() string {
	if r == nil || r.modelsDir == "" {
		return ""
	}
	entries, err := os.ReadDir(r.modelsDir)
	if err != nil {
		return ""
	}
	// Только ИМЕНА подкаталогов: mtime меняется и при дозаписи файлов внутрь
	// существующего bundle, а по такому изменению нельзя запускать полный скан —
	// можно прочитать профиль, пока файлы ещё качаются, и запомнить неполный
	// набор (следующий скан уже не сработает). Свою bundle-загрузку реестр
	// обновляет сам (hf.go: registerBundle → Registry.Load).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return "#" + strings.Join(names, ";")
}

// ModelsDir — каталог моделей реестра.
func (r *Registry) ModelsDir() string { return r.modelsDir }

// Warnings — некорректные каталоги, найденные при сканировании (для /health).
func (r *Registry) Warnings() []string {
	r.warningsMu.RLock()
	defer r.warningsMu.RUnlock()
	out := make([]string, len(r.warnings))
	copy(out, r.warnings)
	return out
}

// NewRegistry — пустой реестр на каталоге dir.
func NewRegistry(dir string) *Registry {
	r := &Registry{modelsDir: dir}
	empty := map[string]types.ImageModelProfile{}
	r.snapshot.Store(&empty)
	return r
}

// profilesMap — текущий снимок профилей (никогда не nil).
func (r *Registry) profilesMap() map[string]types.ImageModelProfile {
	if p := r.snapshot.Load(); p != nil {
		return *p
	}
	return map[string]types.ImageModelProfile{}
}

// Load сканирует каталог моделей и строит реестр.
//
// Правила:
//   - подкаталог без profile.json, но с файлами модели → синтезируем
//     минимальный профиль (family=other, роль diffusion по расширению);
//     так работает «просто положил .gguf в каталог»;
//   - некорректный профиль (ValidateImageModelProfile) НЕ ломает старт: он
//     попадает в warnings и доступен в /api/image/models со state=error —
//     иначе одна битая модель убивала бы весь воркер;
//   - отсутствующий каталог — не ошибка (воркер поднимется и покажет пустой
//     список: так удобнее в Docker, где volume может быть ещё пуст).
//
// Метод идемпотентен и может вызываться повторно во время работы (после
// успешной HF-bundle-загрузки): снимок профилей подменяется атомарно.
func (r *Registry) Load() error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	current := map[string]types.ImageModelProfile{}
	var warnings []string

	defer func() {
		r.snapshot.Store(&current)
		r.warningsMu.Lock()
		r.warnings = warnings
		r.warningsMu.Unlock()
		// Отпечаток каталога фиксируем здесь же: RefreshIfChanged сравнивает его,
		// чтобы не сканировать диск на каждый запрос списка моделей. loadMu уже
		// удерживается вызывающим Load, повторно его брать нельзя (дедлок).
		r.lastScan = r.dirFingerprint()
	}()

	entries, err := os.ReadDir(r.modelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("models_dir_missing: %s (создайте каталог и положите bundle)", r.modelsDir))
			return nil
		}
		return fmt.Errorf("read models dir %s: %w", r.modelsDir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		dir := filepath.Join(r.modelsDir, name)
		profile, err := r.loadProfile(name, dir)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", name, err))
			// Кладём «сломанный» профиль, чтобы UI видел модель и её ошибку.
			current[name] = types.ImageModelProfile{
				Name:   name,
				Family: "other",
				Files:  []types.ImageModelFile{},
				Notes:  "invalid profile: " + err.Error(),
			}
			continue
		}
		current[name] = *profile
	}
	return nil
}

// loadProfile читает profile.json либо синтезирует профиль по файлам каталога.
func (r *Registry) loadProfile(name, dir string) (*types.ImageModelProfile, error) {
	profilePath := filepath.Join(dir, ProfileFileName)
	var profile types.ImageModelProfile
	if data, err := os.ReadFile(profilePath); err == nil {
		// BOM в начале файла — типовая проблема Windows-редакторов; без снятия
		// json.Unmarshal падает на «invalid character 'ï'».
		data = stripBOM(data)
		if err := json.Unmarshal(data, &profile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", ProfileFileName, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", ProfileFileName, err)
	} else {
		synth, err := SynthesizeProfile(name, dir)
		if err != nil {
			return nil, err
		}
		profile = *synth
	}
	if profile.Name == "" {
		profile.Name = name
	}
	if profile.Family == "" {
		profile.Family = "other"
	}
	// LocalPath файлов: в profile.json указан только Filename — путь внутри
	// bundle-каталога. Разворачиваем здесь, чтобы ServerArgs получил валидные
	// пути без знания о modelsDir.
	for i := range profile.Files {
		if profile.Files[i].LocalPath == "" {
			profile.Files[i].LocalPath = filepath.Join(dir, profile.Files[i].Filename)
		}
	}
	if err := types.ValidateImageModelProfile(&profile); err != nil {
		return nil, err
	}
	return &profile, nil
}

// SynthesizeProfile — профиль из файлов каталога, когда profile.json нет.
//
// ЧЕСТНО О ГРАНИЦАХ: синтез различает только «один файл-чекпойнт» (all-in-one)
// и «DiT-набор» по эвристике имён (diffusion/vae/clip/t5/llm). Это заведомо
// менее точно, чем profile.json, поэтому такая модель помечается Notes и
// family=other — движок сам решит по содержимому, а точный плейсмент задаст
// оператор через profile.json.
func SynthesizeProfile(name, dir string) (*types.ImageModelProfile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]types.ImageModelFile, 0, 4)
	var total int64
	for _, e := range entries {
		if e.IsDir() || e.Name() == ProfileFileName {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".gguf" && ext != ".safetensors" && ext != ".ckpt" && ext != ".pt" {
			continue
		}
		role := roleFromFilename(e.Name())
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
			total += size
		}
		files = append(files, types.ImageModelFile{
			Role:      role,
			Repo:      "local",
			Filename:  e.Name(),
			LocalPath: filepath.Join(dir, e.Name()),
			SizeBytes: size,
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no model files (*.gguf/*.safetensors/*.ckpt) and no %s", ProfileFileName)
	}
	// Один diffusion-файл → all-in-one чекпойнт (SD1.x/2.x/SDXL).
	if len(files) == 1 {
		files[0].Role = types.ImageFileRoleDiffusion
	}
	defaults := types.DefaultImageGenDefaults("other")
	return &types.ImageModelProfile{
		Name:     name,
		Family:   "other",
		Files:    files,
		Defaults: defaults,
		Notes: "профиль синтезирован по содержимому каталога: " + ProfileFileName +
			" отсутствует, family=other, плейсмент по умолчанию (движок выберет сам)",
	}, nil
}

// SuggestRole — предложенная роль файла по его ИМЕНИ (экспорт эвристики
// roleFromFilename).
//
// ЗАЧЕМ ЭКСПОРТ (R-Image Phase 9): страница «Image-модели» показывает роль
// рядом с каждым файлом в HF-поиске, чтобы оператор собирал bundle не вслепую
// (diffusion / vae / clip_l / clip_g / t5xxl / llm / taesd / lora / upscaler /
// controlnet / ip_adapter / clip_vision). Эвристика обязана жить в ОДНОМ месте:
// ту же функцию применяют при чтении готового bundle, и если UI начнёт угадывать
// роли сам, правила со временем разъедутся.
//
// Возвращает ImageFileRoleDiffusion, если ничего не подошло: для однофайловых
// SD1.5-репозиториев это ровно верный ответ, а в многофайловых (FLUX/SDXL)
// оператор роль поправит — но ПУСТОЙ роли не бывает, иначе UI показал бы
// «неизвестно» там, где движку нужна конкретная роль.
func SuggestRole(filename string) string {
	return roleFromFilename(filename)
}

// roleFromFilename — эвристика роли по имени файла.
//
// R-Image (2026-10-03): принимается ПУТЬ внутри репозитория, а не только имя
// файла. DiT-репозитории раскладывают компоненты по каталогам (`vae/...`,
// `text_encoders/...`, `split_files/...`), и по одному имени роль не угадать:
// `text_encoders/qwen3vl_8b_bf16.safetensors` — это LLM-энкодер Qwen-Image, а не
// diffusion-файл (раньше он и попадал в diffusion, из-за чего в bundle
// оказывалось два diffusion-файла и загрузка падала на дубликате роли).
func roleFromFilename(filename string) string {
	n := strings.ToLower(filename)
	// Каталог — сильный сигнал, но только когда он есть в пути.
	inTextEncoderDir := strings.Contains(n, "text_encoder") || strings.Contains(n, "text-encoder") ||
		strings.Contains(n, "/te/") || strings.HasPrefix(n, "te/")
	if inTextEncoderDir {
		switch {
		case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
			return types.ImageFileRoleClipL
		case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
			return types.ImageFileRoleClipG
		case strings.Contains(n, "t5"):
			return types.ImageFileRoleT5xxl
		case isLLMEncoderName(n):
			return types.ImageFileRoleLLM
		}
	}
	switch {
	case strings.Contains(n, "vae") || strings.Contains(n, "ae.safetensors"):
		return types.ImageFileRoleVae
	case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
		return types.ImageFileRoleClipL
	case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
		return types.ImageFileRoleClipG
	case strings.Contains(n, "t5"):
		return types.ImageFileRoleT5xxl
	case strings.Contains(n, "taesd"):
		return types.ImageFileRoleTaesd
	case strings.Contains(n, "clip_vision") || strings.Contains(n, "vision"):
		return types.ImageFileRoleClipVision
	case strings.Contains(n, "control"):
		return types.ImageFileRoleControlNet
	case strings.Contains(n, "ip-adapter") || strings.Contains(n, "ip_adapter"):
		return types.ImageFileRoleIPAdapter
	case strings.Contains(n, "lora"):
		return types.ImageFileRoleLora
	case strings.Contains(n, "esrgan") || strings.Contains(n, "upscal"):
		return types.ImageFileRoleUpscaler
	}
	return types.ImageFileRoleDiffusion
}

// isLLMEncoderName — имя похоже на LLM-энкодер (Qwen-Image/Z-Image/FLUX-LLM).
//
// ЗАЧЕМ ОТДЕЛЬНО И ТОЛЬКО В КАТАЛОГЕ text_encoders: «qwen» встречается и в имени
// самого diffusion-файла (qwen-image-2.1-UC-Q4_0.gguf), поэтому вне каталога
// энкодеров такое правило давало бы ложные срабатывания.
func isLLMEncoderName(n string) bool {
	for _, marker := range []string{"qwen", "gemma", "mistral", "llama", "deepseek", "phi", "llm", "3vl", "_vl_"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// Profile — профиль по имени (копия: реестр не должен мутироваться извне).
func (r *Registry) Profile(name string) (types.ImageModelProfile, bool) {
	p, ok := r.profilesMap()[name]
	return p, ok
}

// Names — имена моделей в стабильном (алфавитном) порядке.
func (r *Registry) Names() []string {
	return types.ImageProfileNames(r.profilesMap())
}

// Profiles — все профили (копия карты).
func (r *Registry) Profiles() map[string]types.ImageModelProfile {
	src := r.profilesMap()
	out := make(map[string]types.ImageModelProfile, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// BundleSizeBytes — суммарный размер файлов модели на диске.
//
// Считаем по факту (а не по SizeBytes из профиля): файлы могли быть скачаны
// частично, и UI должен показывать реальное место на диске.
func (r *Registry) BundleSizeBytes(p *types.ImageModelProfile) int64 {
	if p == nil {
		return 0
	}
	dir := p.BundleDir(r.modelsDir)
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // размер — метрика, а не критичный путь
		}
		total += info.Size()
		return nil
	})
	if total > 0 {
		return total
	}
	for _, f := range p.Files {
		total += f.SizeBytes
	}
	return total
}

// Info собирает ModelInfo для модели с учётом состояния супервизора.
func (r *Registry) Info(p types.ImageModelProfile, state, lastErr string, loadedAt, lastUsed time.Time, active int64) ModelInfo {
	info := ModelInfo{
		Name:           p.Name,
		BundlePath:     p.BundleDir(r.modelsDir),
		Family:         p.Family,
		State:          state,
		SizeBytes:      r.BundleSizeBytes(&p),
		VramEstimateMB: p.VramEstimateMB,
		ActiveQueries:  active,
		Disabled:       p.Disabled,
		Error:          lastErr,
		Defaults:       p.Defaults,
		Files:          p.Files,
		Notes:          p.Notes,
	}
	if !loadedAt.IsZero() {
		info.LoadedAt = loadedAt.UTC().Format(time.RFC3339)
	}
	if !lastUsed.IsZero() {
		info.LastUsedAt = lastUsed.UTC().Format(time.RFC3339)
	}
	return info
}

// stripBOM убирает UTF-8 BOM (Windows-редакторы добавляют его молча).
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
