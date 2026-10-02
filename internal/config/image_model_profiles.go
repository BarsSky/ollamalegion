// image_model_profiles.go — R-Image (2026-09-27): хранилище профилей
// image-моделей (bundle) на балансере.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ФАЙЛ/СТРУКТУРА, А НЕ ПОЛЕ В config.json.
//
// Профили llama.cpp живут в types.LoadBalancerConfig.LlamaCppModelProfiles и
// сохраняются общим configSaver()'ом. Для image-профилей этот путь недоступен:
// pkg/types/config.go — замороженный контракт Phase 1, и добавлять туда
// ImageModelProfiles нельзя (правка согласуется отдельно). Поэтому профили
// image-моделей хранятся в собственном JSON-файле
// (config/image-model-profiles.json, override через LB_IMAGE_MODEL_PROFILES_PATH)
// под собственным локом. Схема записи сознательно повторяет поведение
// cppworker-profile'ов: PUT валидирует и персистит, DELETE удаляет и персистит.
//
// ЗАЧЕМ ВООБЩЕ: одна диффузионная модель — это bundle (diffusion+vae+TE) плюс
// набор флагов sd-server; профиль нужен и балансеру (VRAM-гейт, таймауты,
// UI-редактор), и воркеру (spawn sd-server, см. types.ImageModelProfile.ServerArgs).
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// DefaultImageModelProfilesPath — путь по умолчанию (относительно CWD
	// процесса). Лежит рядом с остальными config/*.json балансера.
	DefaultImageModelProfilesPath = "config/image-model-profiles.json"

	// EnvImageModelProfilesPath — override пути файла профилей.
	EnvImageModelProfilesPath = "LB_IMAGE_MODEL_PROFILES_PATH"
)

// imageModelProfilesFile — on-disk формат файла профилей.
//
// Обёртка над map (а не голый map) нужна для версии схемы: при будущих
// изменениях контракта (types.ImageModelProfile) можно будет мигрировать файл,
// а не молча падать на неизвестных полях.
type imageModelProfilesFile struct {
	Version  int                                `json:"version"`
	Profiles map[string]types.ImageModelProfile `json:"profiles"`
}

// ImageModelProfileStore — потокобезопасное хранилище профилей image-моделей.
//
// Ленивая загрузка: Load() вызывается при первом обращении (см.
// EnsureLoaded), поэтому отсутствие файла — не ошибка, а «профилей пока нет».
type ImageModelProfileStore struct {
	mu       sync.RWMutex
	path     string
	profiles map[string]types.ImageModelProfile
	loaded   bool
}

// NewImageModelProfileStore создаёт хранилище (без чтения с диска).
func NewImageModelProfileStore(path string) *ImageModelProfileStore {
	if path == "" {
		path = ResolveImageModelProfilesPath()
	}
	return &ImageModelProfileStore{
		path:     path,
		profiles: make(map[string]types.ImageModelProfile),
	}
}

// ResolveImageModelProfilesPath — путь файла профилей: env → default.
func ResolveImageModelProfilesPath() string {
	if p := os.Getenv(EnvImageModelProfilesPath); p != "" {
		return p
	}
	return DefaultImageModelProfilesPath
}

// Path — путь файла профилей.
func (s *ImageModelProfileStore) Path() string { return s.path }

// EnsureLoaded — однократная загрузка файла (отсутствие файла = пустое хранилище).
func (s *ImageModelProfileStore) EnsureLoaded() error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	return s.Load()
}

// Load (пере)читает файл профилей.
func (s *ImageModelProfileStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	profiles := make(map[string]types.ImageModelProfile)
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read image model profiles %s: %w", s.path, err)
		}
		// Файла нет — это нормально (ни одного профиля ещё не создавали).
		s.profiles = profiles
		s.loaded = true
		return nil
	}

	var file imageModelProfilesFile
	// R-Image (2026-10-02, найдено Phase 6): снимаем UTF-8 BOM. Notepad и
	// PowerShell Set-Content -Encoding UTF8 пишут BOM, а json.Unmarshal на нём
	// падает с «invalid character 'ï'» — оператор видел бы «профилей нет»
	// (или ошибку чтения) при внешне корректном файле.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse image model profiles %s: %w", s.path, err)
	}
	for name, p := range file.Profiles {
		if p.Name == "" {
			p.Name = name
		}
		profiles[name] = p
	}
	s.profiles = profiles
	s.loaded = true
	logger.Get().Infow("image model profiles loaded", "path", s.path, "count", len(profiles))
	return nil
}

// Get — профиль по имени.
func (s *ImageModelProfileStore) Get(name string) (types.ImageModelProfile, bool) {
	_ = s.EnsureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.profiles[name]
	return p, ok
}

// List — копия карты профилей (мутация возвращённого значения не влияет на хранилище).
func (s *ImageModelProfileStore) List() map[string]types.ImageModelProfile {
	_ = s.EnsureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]types.ImageModelProfile, len(s.profiles))
	for k, v := range s.profiles {
		out[k] = v
	}
	return out
}

// Names — отсортированные имена профилей (стабильные ответы API).
func (s *ImageModelProfileStore) Names() []string {
	_ = s.EnsureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.profiles))
	for k := range s.profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Set — валидирует и сохраняет профиль.
//
// Валидация делегируется types.ValidateImageModelProfile (единый источник
// правил — движок sd-server собственные границы не проверяет: width/height он
// не валидирует вовсе, steps/batch клампятся МОЛЧА). Дублировать правила здесь
// запрещено: расхождение балансера и воркера даёт «сохранилось, но не работает».
func (s *ImageModelProfileStore) Set(name string, p types.ImageModelProfile) error {
	if name == "" {
		name = p.Name
	}
	if name == "" {
		return fmt.Errorf("profile name is required")
	}
	// Имя профиля = имя каталога bundle (models.ImageModelProfile.BundleDir),
	// поэтому path traversal недопустим.
	if err := validateImageProfileName(name); err != nil {
		return err
	}
	p.Name = name
	if err := types.ValidateImageModelProfile(&p); err != nil {
		return err
	}

	if err := s.EnsureLoaded(); err != nil {
		return err
	}
	s.mu.Lock()
	s.profiles[name] = p
	s.mu.Unlock()
	return s.Save()
}

// Delete — удаляет профиль; false если его не было.
func (s *ImageModelProfileStore) Delete(name string) (bool, error) {
	if err := s.EnsureLoaded(); err != nil {
		return false, err
	}
	s.mu.Lock()
	_, existed := s.profiles[name]
	if existed {
		delete(s.profiles, name)
	}
	s.mu.Unlock()
	if !existed {
		return false, nil
	}
	return true, s.Save()
}

// Save — атомарная запись файла профилей (tmp + rename).
//
// Атомарность важна: обрезанный JSON читается как «нет профилей», и после
// рестарта балансер молча терял бы все настройки моделей.
func (s *ImageModelProfileStore) Save() error {
	s.mu.RLock()
	profiles := make(map[string]types.ImageModelProfile, len(s.profiles))
	for k, v := range s.profiles {
		profiles[k] = v
	}
	path := s.path
	s.mu.RUnlock()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create image model profiles dir %s: %w", dir, err)
		}
	}

	data, err := json.MarshalIndent(imageModelProfilesFile{Version: 1, Profiles: profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image model profiles: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write image model profiles temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename image model profiles %s: %w", path, err)
	}
	logger.Get().Infow("image model profiles saved", "path", path, "count", len(profiles))
	return nil
}

// validateImageProfileName — имя профиля становится именем каталога bundle
// (types.ImageModelProfile.BundleDir = <modelsDir>/<name>), поэтому запрещаем
// всё, что может выйти за пределы modelsDir или создать скрытый каталог.
func validateImageProfileName(name string) error {
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid profile name %q: must not start with '.' or be a relative path element", name)
	}
	for _, r := range name {
		if r == '/' || r == '\\' {
			return fmt.Errorf("profile name %q must not contain path separators", name)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("profile name %q contains control characters", name)
		}
	}
	return nil
}
