// store.go — файловое хранилище `model-reference.json`.
//
// КОНВЕНЦИЯ РЕПОЗИТОРИЯ (docs/plans/2026-11-17-reference-templates-design.md §1.6,
// §2.5): вспомогательное состояние — отдельный файл в записываемом томе /app/data,
// запись АТОМАРНАЯ (tmp + rename, файл 0644, каталог 0755), путь при
// необходимости переопределяется env, ОТСУТСТВИЕ файла — не ошибка.
// Образцы: internal/balancer/profiles_persistence.go:162-205,
// internal/settings/settings.go:348-372.
//
// ПОЧЕМУ НЕ config.json: в bundled-развёртывании /app/config смонтирован
// read-only (§2.1, §7 п.5), запись туда молча теряется.
package reference

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ResolvePath — путь к файлу хранилища.
//
// Порядок (§2.1): env LB_MODEL_REFERENCE_PATH → каталог state.json
// (`LB_STATE_PATH`, иначе data/state.json — internal/config/config.go:155).
// Ровно как profilesFilePath (profiles_persistence.go:52-58) и settingsPath
// (cmd/balancer/main.go:233-237).
func ResolvePath(statePath string) string {
	if override := strings.TrimSpace(os.Getenv(EnvPathOverride)); override != "" {
		return override
	}
	if statePath == "" {
		return filepath.Join("data", FileName)
	}
	return filepath.Join(filepath.Dir(statePath), FileName)
}

// Store — хранилище записей и шаблонов под одним замком.
//
// Один замок на всё: записи и шаблоны живут в одном файле и меняются вместе
// (§2.1), поэтому двух замков не заводим.
type Store struct {
	mu   sync.RWMutex
	path string
	doc  Doc
	// seeded — сид выполнен в этом процессе (для лога/тестов).
	seeded bool
}

// NewStore — пустое хранилище с путём. Чтение — отдельным вызовом Load().
func NewStore(path string) *Store {
	return &Store{
		path: path,
		doc:  emptyDoc(),
	}
}

// emptyDoc — валидное пустое состояние: файла нет = пустые списки (§2.5).
func emptyDoc() Doc {
	return Doc{
		Version:         SchemaVersion,
		Recommendations: []Recommendation{},
		Templates:       []Template{},
	}
}

// Path — путь файла (для логов и тестов).
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Seeded — выполнялся ли сид в этом процессе.
func (s *Store) Seeded() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seeded
}

// Load — чтение при старте.
//
// Правила (§2.5):
//   - файла нет → ОДНОКРАТНЫЙ сид записями из docs/operations/model-tuning-reference.ru.md
//     (решение человека №4), файл создаётся;
//   - файл есть → читается как есть и НИКОГДА не перезаписывается сидом;
//   - битый JSON / неизвестная version → warn + работаем без данных (не падаем).
//
// Возвращает true, если в этом процессе был выполнен сид.
func (s *Store) Load() bool {
	if s == nil || s.path == "" {
		return false
	}
	data, missing, err := readFile(s.path)
	if err != nil {
		logger.Get().Warnw("reference: не удалось прочитать файл хранилища — работаем без данных",
			"path", s.path, "error", err)
		return false
	}
	if !missing {
		// Существующий файл: читаем, но сидом НЕ перезаписываем (решение №4).
		s.applyFile(data)
		return false
	}
	return s.seed()
}

// readFile — чтение файла с явным признаком «файла нет».
func readFile(path string) (data []byte, missing bool, err error) {
	data, err = os.ReadFile(path)
	if err == nil {
		return data, false, nil
	}
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	return nil, false, err
}

// applyFile — разбор содержимого файла. Ошибки не фатальны: warn + пусто.
func (s *Store) applyFile(data []byte) {
	var doc Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		logger.Get().Warnw("reference: файл хранилища не разобран — работаем без данных",
			"path", s.path, "error", err)
		return
	}
	if doc.Version != SchemaVersion {
		// Как profiles_persistence.go:86-90: неизвестная версия — warn и НЕ читаем.
		logger.Get().Warnw("reference: неизвестная версия файла хранилища — работаем без данных",
			"path", s.path, "version", doc.Version, "expected", SchemaVersion)
		return
	}
	s.mu.Lock()
	s.doc = Doc{
		Version:         SchemaVersion,
		Recommendations: nonNilRecommendations(doc.Recommendations),
		Templates:       nonNilTemplates(doc.Templates),
	}
	s.mu.Unlock()
	logger.Get().Infow("reference: файл хранилища загружен",
		"path", s.path,
		"recommendations", len(doc.Recommendations),
		"templates", len(doc.Templates))
}

// EnsureSeedTemplates — ДОБАВИТЬ недостающие seed-шаблоны в СУЩЕСТВУЮЩЕЕ
// хранилище и вернуть, сколько добавлено (R493f, 2026-11-17).
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ СИДА. Решение человека №4 запрещает перезаписывать
// существующий файл сидом — и это правильно: записи, которые правил оператор,
// трогать нельзя. Но у стоящего стенда файл уже создан (рекомендации есть), а
// шаблонов в нём НЕТ, потому что первый сид создавал `Templates: []`. Без
// добора страница «Справка и шаблоны» на живом стенде осталась бы с пустой
// вкладкой навсегда.
//
// Правила (идемпотентно и без потерь):
//   - добавляются ТОЛЬКО те seed-шаблоны, чьего id в файле нет;
//   - существующий шаблон с тем же id НЕ перезаписывается (даже если оператор
//     изменил его до неузнаваемости) — это его запись, а не наша;
//   - рекомендации не трогаются вообще;
//   - запись на диск — одна, только если что-то добавилось.
func (s *Store) EnsureSeedTemplates() int {
	if s == nil {
		return 0
	}
	seeds := seedTemplates(nowRFC3339())
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := make(map[string]bool, len(s.doc.Templates))
	for _, t := range s.doc.Templates {
		existing[t.ID] = true
	}
	added := 0
	for _, t := range seeds {
		if existing[t.ID] {
			continue
		}
		s.doc.Templates = append(s.doc.Templates, t)
		existing[t.ID] = true
		added++
	}
	if added == 0 {
		return 0
	}
	if warn := s.persistWarningLocked(); warn != "" {
		logger.Get().Warnw("reference: seed-шаблоны добавлены только в память",
			"path", s.path, "added", added, "warning", warn)
		return added
	}
	logger.Get().Infow("reference: недостающие seed-шаблоны добавлены",
		"path", s.path, "added", added, "templates_total", len(s.doc.Templates))
	return added
}

// seed — однократный сид из доков. Файл создаётся атомарной записью.
func (s *Store) seed() bool {
	doc := SeedDoc(nowRFC3339())
	s.mu.Lock()
	s.doc = doc
	s.seeded = true
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		// Сид не сохранился — не падаем: записи живут в памяти до рестарта,
		// оператор увидит warn в логе.
		logger.Get().Warnw("reference: сид из доков не записан на диск",
			"path", s.path, "error", err)
		return true
	}
	logger.Get().Infow("reference: однократный сид из docs/operations/model-tuning-reference.ru.md выполнен",
		"path", s.path,
		"recommendations", len(doc.Recommendations),
		"templates", len(doc.Templates))
	return true
}

// --- чтение ---------------------------------------------------------------

// Recommendations — снимок записей, отсортированный по id.
func (s *Store) Recommendations() []Recommendation {
	if s == nil {
		return []Recommendation{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Recommendation, len(s.doc.Recommendations))
	copy(out, s.doc.Recommendations)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Recommendation — одна запись по id.
func (s *Store) Recommendation(id string) (Recommendation, bool) {
	if s == nil {
		return Recommendation{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.doc.Recommendations {
		if rec.ID == id {
			return rec, true
		}
	}
	return Recommendation{}, false
}

// Templates — снимок шаблонов, отсортированный по id.
func (s *Store) Templates() []Template {
	if s == nil {
		return []Template{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Template, len(s.doc.Templates))
	copy(out, s.doc.Templates)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Template — один шаблон по id.
func (s *Store) Template(id string) (Template, bool) {
	if s == nil {
		return Template{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tmpl := range s.doc.Templates {
		if tmpl.ID == id {
			return tmpl, true
		}
	}
	return Template{}, false
}

// --- запись ---------------------------------------------------------------

// RevisionConflictError — оптимистичная блокировка не сошлась (§2.3, §2.4).
type RevisionConflictError struct {
	Current int
	Sent    int
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("revision conflict: current=%d sent=%d", e.Current, e.Sent)
}

// PutRecommendation — upsert записи (создание или обновление).
//
// Возвращает (сохранённая запись, предупреждение о персисте, ошибка).
// Предупреждение непустое, когда изменение осталось только в памяти (запись на
// диск не удалась) — по конвенции upsertModelProfile (handlers_cppworker_profiles.go:400-416)
// это не отказ, а честное «не сохранится после рестарта».
func (s *Store) PutRecommendation(rec Recommendation) (Recommendation, string, error) {
	if s == nil {
		return rec, "", errors.New("reference store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	index := -1
	for i := range s.doc.Recommendations {
		if s.doc.Recommendations[i].ID == rec.ID {
			index = i
			break
		}
	}
	now := nowRFC3339()
	if index >= 0 {
		existing := s.doc.Recommendations[index]
		if rec.Revision != 0 && rec.Revision != existing.Revision {
			return existing, "", &RevisionConflictError{Current: existing.Revision, Sent: rec.Revision}
		}
		// createdAt — момент создания, его не переписываем; author остаётся,
		// если клиент его не прислал (сервер не подменяет автора записи).
		rec.CreatedAt = existing.CreatedAt
		if strings.TrimSpace(rec.Author) == "" {
			rec.Author = existing.Author
		}
		rec.Revision = existing.Revision + 1
	} else {
		if rec.Revision != 0 {
			// Ревизия прислана, а записи нет — клиент правит то, чего нет.
			return rec, "", &RevisionConflictError{Current: 0, Sent: rec.Revision}
		}
		rec.Revision = 1
		rec.CreatedAt = now
	}
	if strings.TrimSpace(rec.Author) == "" {
		rec.Author = "operator"
	}
	rec.UpdatedAt = now
	if index >= 0 {
		s.doc.Recommendations[index] = rec
	} else {
		s.doc.Recommendations = append(s.doc.Recommendations, rec)
	}
	return rec, s.persistWarningLocked(), nil
}

// DeleteRecommendation — удаление записи. false = записи не было.
func (s *Store) DeleteRecommendation(id string) (bool, string) {
	if s == nil {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.Recommendations {
		if s.doc.Recommendations[i].ID == id {
			s.doc.Recommendations = append(s.doc.Recommendations[:i], s.doc.Recommendations[i+1:]...)
			return true, s.persistWarningLocked()
		}
	}
	return false, ""
}

// PutTemplate — upsert шаблона. Семантика та же, что у PutRecommendation.
func (s *Store) PutTemplate(tmpl Template) (Template, string, error) {
	if s == nil {
		return tmpl, "", errors.New("reference store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	index := -1
	for i := range s.doc.Templates {
		if s.doc.Templates[i].ID == tmpl.ID {
			index = i
			break
		}
	}
	now := nowRFC3339()
	if index >= 0 {
		existing := s.doc.Templates[index]
		if tmpl.Revision != 0 && tmpl.Revision != existing.Revision {
			return existing, "", &RevisionConflictError{Current: existing.Revision, Sent: tmpl.Revision}
		}
		tmpl.CreatedAt = existing.CreatedAt
		if strings.TrimSpace(tmpl.Author) == "" {
			tmpl.Author = existing.Author
		}
		tmpl.Revision = existing.Revision + 1
	} else {
		if tmpl.Revision != 0 {
			return tmpl, "", &RevisionConflictError{Current: 0, Sent: tmpl.Revision}
		}
		tmpl.Revision = 1
		tmpl.CreatedAt = now
	}
	if strings.TrimSpace(tmpl.Author) == "" {
		tmpl.Author = "operator"
	}
	if tmpl.Version <= 0 {
		tmpl.Version = 1
	}
	tmpl.UpdatedAt = now
	if index >= 0 {
		s.doc.Templates[index] = tmpl
	} else {
		s.doc.Templates = append(s.doc.Templates, tmpl)
	}
	return tmpl, s.persistWarningLocked(), nil
}

// DeleteTemplate — удаление шаблона. false = шаблона не было.
func (s *Store) DeleteTemplate(id string) (bool, string) {
	if s == nil {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.Templates {
		if s.doc.Templates[i].ID == id {
			s.doc.Templates = append(s.doc.Templates[:i], s.doc.Templates[i+1:]...)
			return true, s.persistWarningLocked()
		}
	}
	return false, ""
}

// persistWarningLocked — запись на диск без отказа операции.
func (s *Store) persistWarningLocked() string {
	if err := s.saveLocked(); err != nil {
		logger.Get().Warnw("reference: запись файла хранилища не удалась — изменение только в памяти",
			"path", s.path, "error", err)
		return "изменение сохранено в памяти, но файл " + s.path + " записать не удалось: " + err.Error()
	}
	return ""
}

// saveLocked — атомарная запись (tmp + rename, 0644, каталог 0755).
// Вызывается под s.mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil // хранилище без файла: только память (документировано)
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	s.doc.Version = SchemaVersion
	s.doc.Recommendations = nonNilRecommendations(s.doc.Recommendations)
	s.doc.Templates = nonNilTemplates(s.doc.Templates)
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal reference doc: %w", err)
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, s.path, err)
	}
	return nil
}

// --- мелочи ---------------------------------------------------------------

// nowRFC3339 — время в UTC без монотонных часов (стабильный формат в файле).
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// nonNilRecommendations — `[]` вместо `null` в JSON.
func nonNilRecommendations(in []Recommendation) []Recommendation {
	if in == nil {
		return []Recommendation{}
	}
	return in
}

// nonNilTemplates — `[]` вместо `null` в JSON.
func nonNilTemplates(in []Template) []Template {
	if in == nil {
		return []Template{}
	}
	return in
}
