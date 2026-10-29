// Package settings — R278 (2026-10-29): серверный реестр «горячих» настроек.
//
// ЗАЧЕМ. Аудит настроек WebUI (docs/plans/2026-10-29-webui-settings-audit.md)
// показал: 209 env-переменных читаются точечно в местах использования, и
// «изменяемо из WebUI» — свойство ОТДЕЛЬНОЙ переменной, а не системы. Отсюда
// живые жалобы оператора:
//
//   - правит .env, перезапускает контейнер и не понимает, почему значение не
//     применилось (переменную забыли пробросить в compose — системно закрыто
//     в R274, но осталось «а применить без рестарта нельзя»);
//   - чтобы изменить лимит строки стрима, период keepalive или предел буфера,
//     нужен рестарт контейнера, хотя значение читается НА КАЖДЫЙ запрос и
//     могло бы применяться на лету.
//
// ЧТО ЗДЕСЬ. Реестр ОПИСАНИЙ настроек: имя (оно же имя env-переменной), тип,
// дефолт, группа, описание для человека, признак HotReload, валидация и
// функция нормализации env-значения (историческая лояльность: мусор в .env не
// должен ломать стенд — см. NormalizeEnv).
//
// Источники значения, по убыванию приоритета:
//
//  1. override — файл в writable-томе, его пишет API/WebUI;
//  2. env — .env контейнера (как было до R278);
//  3. default — значение из кода.
//
// env остаётся ДЕФОЛТОМ, а решение оператора из WebUI побеждает. Это ровно то
// правило, которое просил оператор: «всё, что задаётся в env, должно быть
// изменяемо из WebUI».
//
// ПОЧЕМУ ГЕТТЕРЫ В КОДЕ СМОТРЯТ СЮДА, А НЕ В os.Getenv. Каждая «горячая»
// переменная теперь вычисляется ОДНОЙ функцией (HotInt), которая и есть
// единственная формула приоритета. Если реестр не инициализирован (юнит-тесты,
// лёгкие бинарники), значение вычисляется по тем же правилам здесь же — две
// реализации разошлись бы, а этот класс дефекта («два мнения об одной
// настройке») мы ловили в серии четыре раза.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// Type — тип значения. Строки/перечисления сюда сознательно не добавлены:
// этап 2 аудита закрывает только числовые «горячие» лимиты, а расширение типа
// требует отдельного решения о хранении (nil-значение vs пустая строка).
type Type string

const (
	TypeInt         Type = "int"
	TypeDurationSec Type = "duration_sec" // секунды, целое
	TypeBytes       Type = "bytes"
	TypeBool        Type = "bool" // 0 = false, 1 = true
)

// Source — откуда взято действующее значение.
type Source string

const (
	SourceDefault  Source = "default"
	SourceEnv      Source = "env"
	SourceOverride Source = "override"
)

// ErrNotHot — настройка не применяется на лету.
var ErrNotHot = errors.New("setting requires restart (hot reload is not supported)")

// ErrUnknown — настройка не зарегистрирована.
var ErrUnknown = errors.New("unknown setting")

// Setting — описание одной настройки.
type Setting struct {
	// Name — имя настройки; оно же имя env-переменной (LB_...). Так оператору
	// не нужно держать в голове две номенклатуры.
	Name        string
	Type        Type
	Default     int64
	Group       string
	Description string
	HotReload   bool

	// Validate проверяет значение, пришедшее из override (API/WebUI). Для env
	// проверка не применяется: там действует NormalizeEnv (см. ниже).
	Validate func(int64) error

	// NormalizeEnv приводит значение из env к допустимому. Нужна ради
	// исторической лояльности: прежние геттеры молча игнорировали мусор и
	// подозрительно маленькие значения, и менять это поведение нельзя —
	// иначе одна опечатка в .env сломала бы стенд. nil = значение как есть.
	NormalizeEnv func(int64) int64
}

// Registry — набор настроек и записанных оператором значений.
type Registry struct {
	mu        sync.RWMutex
	order     []string
	defs      map[string]Setting
	overrides map[string]int64
	path      string
}

// New создаёт реестр с override-файлом path ("" = без персиста, только память)
// и поднимает уже записанные значения. Ошибка чтения файла не фатальна: реестр
// продолжает работать на env/дефолтах, в лог уходит warn (иначе битый файл
// настроек ронял бы балансер).
func New(path string) *Registry {
	r := &Registry{
		defs:      make(map[string]Setting),
		overrides: make(map[string]int64),
		path:      path,
	}
	r.load()
	return r
}

// Register добавляет настройку. Повторная регистрация того же имени — ошибка
// программиста, но не паника в рантайме: пишем в лог и оставляем первую
// (по аналогии с internal/i18n, где дубликат ключа тоже не роняет процесс).
func (r *Registry) Register(s Setting) {
	if r == nil || s.Name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.defs[s.Name]; exists {
		logger.Get().Warnw("settings: duplicate registration ignored", "name", s.Name)
		return
	}
	r.defs[s.Name] = s
	r.order = append(r.order, s.Name)
	sort.Strings(r.order)
}

// Definition — описание настройки по имени.
func (r *Registry) Definition(name string) (Setting, bool) {
	if r == nil {
		return Setting{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.defs[name]
	return s, ok
}

// Effective — действующее значение и его источник.
func (r *Registry) Effective(name string) (int64, Source, bool) {
	if r == nil {
		return 0, SourceDefault, false
	}
	r.mu.RLock()
	def, ok := r.defs[name]
	override, hasOverride := r.overrides[name]
	r.mu.RUnlock()
	if !ok {
		return 0, SourceDefault, false
	}
	if hasOverride {
		return override, SourceOverride, true
	}
	if v, ok := envInt(def); ok {
		// Если нормализация env свела значение к дефолту (мусор, «слишком
		// маленькое»), честнее показать источник default: env фактически ни на
		// что не повлиял. Иначе API отдавал бы source=env рядом с effective,
		// равным дефолту, и оператор снова искал бы «почему не применилось».
		if v == def.Default && def.NormalizeEnv != nil {
			return def.Default, SourceDefault, true
		}
		return v, SourceEnv, true
	}
	return def.Default, SourceDefault, true
}

// Set записывает значение оператора. НЕ-hot настройки отклоняются: молча
// сохранить значение, которое применится «когда-нибудь после рестарта», хуже,
// чем честно сказать «нужен рестарт» (аудит §F, риск «в»).
func (r *Registry) Set(name string, v int64) (Source, error) {
	if r == nil {
		return SourceDefault, ErrUnknown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	def, ok := r.defs[name]
	if !ok {
		return SourceDefault, ErrUnknown
	}
	if !def.HotReload {
		return SourceDefault, ErrNotHot
	}
	if def.Validate != nil {
		if err := def.Validate(v); err != nil {
			return SourceDefault, err
		}
	}
	old, oldSrc, _ := r.effectiveLocked(name)
	r.overrides[name] = v
	if err := r.persistLocked(); err != nil {
		// Откатываем: обещать «применилось», но потерять при рестарте — хуже,
		// чем вернуть ошибку сохранения. Само значение при этом остаётся
		// действующим до конца процесса.
		logger.Get().Warnw("settings: persist failed", "name", name, "error", err)
		return SourceOverride, err
	}
	logger.Get().Infow("settings: override applied",
		"name", name, "old", old, "old_source", string(oldSrc), "new", v)
	return SourceOverride, nil
}

// Reset удаляет override по имени (возврат к env/дефолту).
func (r *Registry) Reset(name string) error {
	if r == nil {
		return ErrUnknown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.defs[name]; !ok {
		return ErrUnknown
	}
	if _, ok := r.overrides[name]; !ok {
		return nil // уже на env/дефолте — не ошибка
	}
	delete(r.overrides, name)
	logger.Get().Infow("settings: override reset", "name", name)
	return r.persistLocked()
}

// ResetAll удаляет все override.
func (r *Registry) ResetAll() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.overrides) == 0 {
		return nil
	}
	r.overrides = make(map[string]int64)
	logger.Get().Infow("settings: all overrides reset")
	return r.persistLocked()
}

// SnapshotEntry — строка ответа API: описание + действующее значение.
type SnapshotEntry struct {
	Name        string `json:"name"`
	Type        Type   `json:"type"`
	Group       string `json:"group"`
	Description string `json:"description"`
	HotReload   bool   `json:"hotReload"`
	Default     int64  `json:"default"`
	Effective   int64  `json:"effective"`
	Source      Source `json:"source"`
}

// Snapshot — все настройки в детерминированном порядке (по имени).
func (r *Registry) Snapshot() []SnapshotEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	names := append([]string(nil), r.order...)
	r.mu.RUnlock()
	out := make([]SnapshotEntry, 0, len(names))
	for _, name := range names {
		def, ok := r.Definition(name)
		if !ok {
			continue
		}
		v, src, _ := r.Effective(name)
		out = append(out, SnapshotEntry{
			Name:        def.Name,
			Type:        def.Type,
			Group:       def.Group,
			Description: def.Description,
			HotReload:   def.HotReload,
			Default:     def.Default,
			Effective:   v,
			Source:      src,
		})
	}
	return out
}

// OverrideCount — сколько значений записано оператором (для строки лога).
func (r *Registry) OverrideCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.overrides)
}

// --- персист ---------------------------------------------------------------

// FileName — имя файла override рядом со state.json (тот же writable-том).
const FileName = "settings-overrides.json"

func (r *Registry) effectiveLocked(name string) (int64, Source, bool) {
	def, ok := r.defs[name]
	if !ok {
		return 0, SourceDefault, false
	}
	if v, has := r.overrides[name]; has {
		return v, SourceOverride, true
	}
	if v, ok := envInt(def); ok {
		if v == def.Default && def.NormalizeEnv != nil {
			return def.Default, SourceDefault, true
		}
		return v, SourceEnv, true
	}
	return def.Default, SourceDefault, true
}

// load поднимает записанные значения. Битый файл не фатален: warn и работаем
// дальше на env/дефолтах (настройки не должны мешать запуску балансера).
func (r *Registry) load() {
	if r == nil || r.path == "" {
		return
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		logger.Get().Warnw("settings: read overrides failed", "path", r.path, "error", err)
		return
	}
	var raw map[string]int64
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Get().Warnw("settings: parse overrides failed", "path", r.path, "error", err)
		return
	}
	r.mu.Lock()
	for name, v := range raw {
		r.overrides[name] = v
	}
	r.mu.Unlock()
}

// persistLocked пишет файл атомарно (tmp + rename): недописанный файл настроек
// при следующем старте молча вернул бы старые значения.
func (r *Registry) persistLocked() error {
	if r.path == "" {
		return nil
	}
	snapshot := make(map[string]int64, len(r.overrides))
	for k, v := range r.overrides {
		snapshot[k] = v
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// --- env -------------------------------------------------------------------

// envInt — значение из env по правилам типа. Для env действует NormalizeEnv
// (а не Validate): мусор и «слишком маленькое» значение не ошибка запуска, а
// повод взять дефолт — ровно как было до R278.
func envInt(def Setting) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(def.Name))
	if raw == "" {
		return 0, false
	}
	var v int64
	switch def.Type {
	case TypeBool:
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "on", "enabled":
			v = 1
		case "0", "false", "no", "off", "disabled":
			v = 0
		default:
			return 0, false
		}
	default:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false
		}
		v = n
	}
	if def.NormalizeEnv != nil {
		v = def.NormalizeEnv(v)
	}
	return v, true
}

// --- глобальный реестр -----------------------------------------------------

var (
	globalMu sync.RWMutex
	global   *Registry
)

// Init создаёт глобальный реестр, регистрирует встроенные «горячие» настройки
// и поднимает записанные значения. Вызывается один раз из cmd/balancer.
//
// path "" означает «без персиста» (например тесты): значения живут только в
// памяти, env и дефолты работают как обычно.
func Init(path string) *Registry {
	r := New(path)
	registerBuiltinsR278(r)
	globalMu.Lock()
	global = r
	globalMu.Unlock()
	logger.Get().Infow("settings registry: initialised",
		"settings", len(r.Snapshot()), "overrides", r.OverrideCount(), "path", path)
	return r
}

// Default — глобальный реестр (nil, если Init не вызывался).
func Default() *Registry {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

// HotInt — действующее значение «горячей» настройки.
//
// ok=false означает «настройка не зарегистрирована»; вызывающий код в этом
// случае использует свой исторический путь чтения (так юнит-тесты и лёгкие
// сборки без Init ведут себя как раньше).
//
// Если глобального реестра нет, значение всё равно вычисляется по той же
// формуле (env → default) из встроенного описания — иначе получилось бы две
// реализации приоритета, которые со временем разойдутся.
func HotInt(name string) (int64, bool) {
	if r := Default(); r != nil {
		if v, _, ok := r.Effective(name); ok {
			return v, true
		}
		return 0, false
	}
	def, ok := builtinsR278(name)
	if !ok {
		return 0, false
	}
	if v, ok := envInt(def); ok {
		return v, true
	}
	return def.Default, true
}

// Describe — человекочитаемое имя источника (для логов и ответов API).
func Describe(s Source) string {
	switch s {
	case SourceOverride:
		return "записано из WebUI/API"
	case SourceEnv:
		return "значение из env"
	default:
		return "значение по умолчанию"
	}
}

// String — удобно для логов.
func (s Setting) String() string {
	return fmt.Sprintf("%s(%s, default=%d)", s.Name, s.Type, s.Default)
}
