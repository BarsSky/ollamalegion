// image_resources.go — R84 (2026-10-03): переопределение политики
// сосуществования image-генерации с текстовым инференсом (balancing.image).
//
// ЗАЧЕМ ФАЙЛ-ПЕРЕОПРЕДЕЛЕНИЕ, А НЕ ПРАВКА config.json.
//
// Единственное место, где эти поля задавались раньше, — config/config.json
// (секция balancing.image). В едином стенде (docker-compose.stack.yml) этот
// каталог смонтирован в контейнер ТОЛЬКО ДЛЯ ЧТЕНИЯ (`../config:/app/config:ro`),
// поэтому штатный configSaver() физически не может туда писать, а env-переменных
// для этих полей в проекте нет вовсе. Оператор, желающий поменять политику, был
// обязан править файл на хосте и перезапускать балансер.
//
// Ровно эта же проблема уже решалась для профилей image-моделей
// (internal/config/image_model_profiles.go): значения уехали в собственный файл
// в ЗАПИСЫВАЕМОМ томе /app/data с override-путём через env. Здесь тот же приём,
// потому что он проверен в бою и не трогает config.json — эталон конфигурации
// остаётся в репозитории, а всё «операторское» живёт отдельно и сбрасывается
// одной кнопкой.
//
// ЗАЧЕМ ВООБЩЕ МЕНЯТЬ ПОЛИТИКУ: на карте 8 ГБ крупная текстовая модель и
// image-модель одновременно не помещаются, и гейт (правильно) отказывает в
// генерации. Политика exclusive — безопасный дефолт; offload разрешает
// совместную работу (если image-воркер реально стартовал с offload в RAM), а
// dedicated отключает ограничения для отдельной GPU.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// DefaultImageResourcesPath — путь по умолчанию (относительно CWD процесса).
	DefaultImageResourcesPath = "config/image-resources.json"

	// EnvImageResourcesPath — override пути файла политики.
	//
	// В едином стенде указывает на записываемый том:
	// LB_IMAGE_RESOURCES_PATH=/app/data/image-resources.json (см. compose).
	EnvImageResourcesPath = "LB_IMAGE_RESOURCES_PATH"

	// imageResourcesFileVersion — версия схемы файла: при будущем изменении
	// контракта types.ImageResourceSettings файл можно мигрировать, а не молча
	// падать на неизвестных полях.
	imageResourcesFileVersion = 1
)

// Границы полей — те же смысловые пределы, что и у самого гейта: за их
// пределами значение бессмысленно (таймаут лока больше суток, резерв VRAM больше
// любой карты и т.п.), и лучше отклонить его на входе, чем потом искать причину
// «генерация не работает».
const (
	ImageResourceMaxHeadroomMB = 65536
	ImageResourceMaxQueueWaitS = 3600
	ImageResourceMaxLockFuseS  = 86400
	// ImageResourceMaxToolLoadTimeoutS — верхняя граница ожидания загрузки модели
	// из вызова инструмента. Сутки: заведомо больше, чем нужно на любой стенд
	// (даже 12 ГБ с медленного диска), но не «бесконечность» — иначе один вызов
	// держал бы слот и запрос клиента неограниченно.
	ImageResourceMaxToolLoadTimeoutS = 86400
	// ImageResourceMinToolLoadTimeoutS — нижняя граница ожидания загрузки.
	//
	// R88 (2026-10-08): 1 → 0. Ноль больше НЕ означает «отказ от ожидания»:
	// по доктрине таймаутов 0 = «капа нет», то есть ждём терминального состояния
	// (loaded / error / смерть процесса sd-server). Раньше 0 действительно был
	// бессмыслен (мгновенный таймаут), но теперь это осмысленный дефолт — и
	// форма WebUI обязана его принимать, иначе оператор не сможет сохранить
	// политику, не выдумав число.
	ImageResourceMinToolLoadTimeoutS = 0
)

// imageResourcesFile — on-disk формат файла политики.
type imageResourcesFile struct {
	Version  int                         `json:"version"`
	SavedAt  time.Time                   `json:"savedAt"`
	Settings types.ImageResourceSettings `json:"settings"`
}

// ImageResourcesStore — потокобезопасное хранилище переопределения политики.
//
// Ленивая загрузка: отсутствие файла — не ошибка, а «переопределения нет,
// действуют значения из config.json».
type ImageResourcesStore struct {
	mu       sync.RWMutex
	path     string
	settings types.ImageResourceSettings
	loaded   bool
	// present — файл переопределения существует (оператор что-то сохранял).
	present bool
}

// NewImageResourcesStore создаёт хранилище (без чтения с диска).
func NewImageResourcesStore(path string) *ImageResourcesStore {
	if strings.TrimSpace(path) == "" {
		path = ResolveImageResourcesPath()
	}
	return &ImageResourcesStore{path: path}
}

// ResolveImageResourcesPath — путь файла: env, иначе значение по умолчанию.
func ResolveImageResourcesPath() string {
	if p := strings.TrimSpace(os.Getenv(EnvImageResourcesPath)); p != "" {
		return p
	}
	return DefaultImageResourcesPath
}

// Path — путь файла политики.
func (s *ImageResourcesStore) Path() string { return s.path }

// Load (пере)читает файл переопределения.
func (s *ImageResourcesStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded = true
			s.present = false
			return nil
		}
		return fmt.Errorf("read image resources %s: %w", s.path, err)
	}
	var doc imageResourcesFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse image resources %s: %w", s.path, err)
	}
	if err := ValidateImageResourceSettings(doc.Settings); err != nil {
		return fmt.Errorf("invalid image resources in %s: %w", s.path, err)
	}
	s.settings = doc.Settings
	s.loaded = true
	s.present = true
	logger.Get().Infow("image resources override loaded",
		"path", s.path, "coexistence", doc.Settings.EffectiveCoexistencePolicy())
	return nil
}

// EnsureLoaded — однократная загрузка (отсутствие файла = переопределения нет).
func (s *ImageResourcesStore) EnsureLoaded() error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	return s.Load()
}

// Present — есть ли сохранённое переопределение.
func (s *ImageResourcesStore) Present() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.present
}

// Settings — сохранённые значения (валидны только при Present()==true).
func (s *ImageResourcesStore) Settings() types.ImageResourceSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// Save валидирует и атомарно сохраняет переопределение.
func (s *ImageResourcesStore) Save(in types.ImageResourceSettings) error {
	if err := ValidateImageResourceSettings(in); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := imageResourcesFile{Version: imageResourcesFileVersion, SavedAt: time.Now().UTC(), Settings: in}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image resources: %w", err)
	}
	raw = append(raw, '\n')

	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	// Атомарно: temp в том же каталоге + rename. Иначе падение в момент записи
	// оставило бы обрезанный JSON, и балансер не поднялся бы после рестарта.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write image resources temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename image resources %s: %w", s.path, err)
	}
	s.settings = in
	s.loaded = true
	s.present = true
	logger.Get().Infow("image resources override saved",
		"path", s.path, "coexistence", in.EffectiveCoexistencePolicy(),
		"headroomMB", in.VramHeadroomMB, "gateDisabled", in.GateDisabled)
	return nil
}

// Remove удаляет переопределение (возврат к значениям config.json).
func (s *ImageResourcesStore) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove image resources %s: %w", s.path, err)
	}
	s.settings = types.ImageResourceSettings{}
	s.loaded = true
	s.present = false
	logger.Get().Infow("image resources override removed", "path", s.path)
	return nil
}

// ValidateImageResourceSettings — проверка значений политики.
//
// ВАЛИДАЦИЯ ЗДЕСЬ, А НЕ В UI: файл можно положить руками и прислать значение
// curl'ом; невалидная политика («exlusive» с опечаткой) молча превратилась бы в
// дефолт exclusive, и оператор не понял бы, почему «ничего не поменялось».
func ValidateImageResourceSettings(s types.ImageResourceSettings) error {
	if !types.IsValidImageCoexistencePolicy(s.Coexistence) {
		return fmt.Errorf("unknown coexistence policy %q (допустимо: exclusive, offload, dedicated или пусто = exclusive)", s.Coexistence)
	}
	if s.VramHeadroomMB < 0 || s.VramHeadroomMB > ImageResourceMaxHeadroomMB {
		return fmt.Errorf("vramHeadroomMb вне диапазона 0..%d (получено %d)", ImageResourceMaxHeadroomMB, s.VramHeadroomMB)
	}
	if s.QueueWaitTimeoutSec < 0 || s.QueueWaitTimeoutSec > ImageResourceMaxQueueWaitS {
		return fmt.Errorf("queueWaitTimeoutSec вне диапазона 0..%d (получено %d)", ImageResourceMaxQueueWaitS, s.QueueWaitTimeoutSec)
	}
	if s.ExclusiveLockTimeoutSec < 0 || s.ExclusiveLockTimeoutSec > ImageResourceMaxLockFuseS {
		return fmt.Errorf("exclusiveLockTimeoutSec вне диапазона 0..%d (получено %d)", ImageResourceMaxLockFuseS, s.ExclusiveLockTimeoutSec)
	}
	// nil = «не задано» (действует env), поэтому проверяем только заданное значение.
	if s.ToolLoadTimeoutSec != nil {
		v := *s.ToolLoadTimeoutSec
		if v < ImageResourceMinToolLoadTimeoutS || v > ImageResourceMaxToolLoadTimeoutS {
			return fmt.Errorf("toolLoadTimeoutSec вне диапазона %d..%d (получено %d)",
				ImageResourceMinToolLoadTimeoutS, ImageResourceMaxToolLoadTimeoutS, v)
		}
	}
	return nil
}

// ApplyImageResourcesOverride — применить переопределение к конфигу при старте.
//
// Возвращает true, если значения из файла переопределили config.json. Ошибка НЕ
// фатальна: стенд должен подниматься и на битом файле переопределения (иначе
// оператор не смог бы зайти в WebUI и сбросить его).
func ApplyImageResourcesOverride(target *types.LoadBalancerConfig, store *ImageResourcesStore) bool {
	if target == nil || store == nil {
		return false
	}
	if err := store.EnsureLoaded(); err != nil {
		logger.Get().Warnw("image resources override ignored", "path", store.Path(), "error", err)
		return false
	}
	if !store.Present() {
		return false
	}
	target.Balancing.Image = store.Settings()
	return true
}
