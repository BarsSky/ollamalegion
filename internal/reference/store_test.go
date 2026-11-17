// store_test.go — инварианты файлового хранилища.
//
// Проверяется то, что видно только на диске: битый JSON и неизвестная версия НЕ
// ломают запуск (warn + пусто), путь вычисляется как «рядом со state.json» с
// override через LB_MODEL_REFERENCE_PATH, запись атомарна (в каталоге не
// остаётся .tmp), каталог создаётся с 0755, файл — 0644.
//
// Контракт: docs/plans/2026-11-17-reference-templates-design.md §2.1, §2.5.
package reference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolvePath — путь: env-override, иначе каталог state.json, иначе data/.
func TestResolvePath(t *testing.T) {
	t.Run("env_override", func(t *testing.T) {
		t.Setenv(EnvPathOverride, "/tmp/custom-reference.json")
		if got := ResolvePath("/app/data/state.json"); got != "/tmp/custom-reference.json" {
			t.Errorf("ResolvePath = %q, ожидали env-override", got)
		}
	})
	t.Run("next_to_state", func(t *testing.T) {
		t.Setenv(EnvPathOverride, "")
		want := filepath.Join(filepath.Dir(filepath.Join("app", "data", "state.json")), FileName)
		if got := ResolvePath(filepath.Join("app", "data", "state.json")); got != want {
			t.Errorf("ResolvePath = %q, ожидали %q", got, want)
		}
	})
	t.Run("no_state_path", func(t *testing.T) {
		t.Setenv(EnvPathOverride, "")
		want := filepath.Join("data", FileName)
		if got := ResolvePath(""); got != want {
			t.Errorf("ResolvePath = %q, ожидали %q", got, want)
		}
	})
}

// TestLoad_CorruptJSONIsNotFatal — битый файл: warn + пусто, без паники.
func TestLoad_CorruptJSONIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	store := NewStore(path)
	if seeded := store.Load(); seeded {
		t.Errorf("сид не должен выполняться поверх существующего (битого) файла")
	}
	if got := len(store.Recommendations()); got != 0 {
		t.Errorf("рекомендаций = %d, ожидали 0 (битый файл = работаем без данных)", got)
	}
	if got := len(store.Templates()); got != 0 {
		t.Errorf("шаблонов = %d, ожидали 0", got)
	}
}

// TestLoad_UnknownVersionIgnored — неизвестная version: warn + пусто.
func TestLoad_UnknownVersionIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	doc := `{"version":99,"recommendations":[{"id":"x1","title":"t","params":[{"name":"contextLength","value":1024}]}],"templates":[]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	store := NewStore(path)
	store.Load()
	if got := len(store.Recommendations()); got != 0 {
		t.Errorf("рекомендаций = %d, ожидали 0 (версия 99 не читается)", got)
	}
}

// TestSave_AtomicAndPermissions — запись атомарна: каталог создан, файл 0644,
// временного .tmp не осталось, содержимое разбирается как Doc.
func TestSave_AtomicAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	path := filepath.Join(dir, FileName)
	store := NewStore(path)
	store.Load() // файла нет → сид + запись

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл хранилища не создан: %v", err)
	}
	var doc Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("содержимое файла не разбирается как Doc: %v", err)
	}
	if doc.Version != SchemaVersion {
		t.Errorf("version в файле = %d, ожидали %d", doc.Version, SchemaVersion)
	}
	if len(doc.Recommendations) == 0 {
		t.Errorf("сид не записал ни одной записи")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("после записи остался временный файл %s.tmp", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat файла: %v", err)
	}
	// Права проверяем только на POSIX: Windows не хранит биты режима для
	// обычных файлов и отдаёт 0666 независимо от переданного 0644.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("права файла = %o, ожидали 644", perm)
		}
	}
}

// TestStore_PutKeepsCreatedAtAndBumpsRevision — служебные поля ставит сервер.
func TestStore_PutKeepsCreatedAtAndBumpsRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store := NewStore(path)
	store.Load()

	rec := Recommendation{
		ID:     "agent-note",
		Title:  "Заметка",
		Params: []Param{{Name: "contextLength", Value: json.RawMessage("32768")}},
	}
	saved, warn, err := store.PutRecommendation(rec)
	if err != nil {
		t.Fatalf("PutRecommendation: %v", err)
	}
	if warn != "" {
		t.Errorf("неожиданное предупреждение: %s", warn)
	}
	if saved.Revision != 1 {
		t.Errorf("revision = %d, ожидали 1", saved.Revision)
	}
	if saved.CreatedAt == "" {
		t.Errorf("createdAt не заполнен")
	}
	created := saved.CreatedAt

	updated, _, err := store.PutRecommendation(Recommendation{
		ID:     "agent-note",
		Title:  "Заметка (правка)",
		Params: []Param{{Name: "contextLength", Value: json.RawMessage("65536")}},
	})
	if err != nil {
		t.Fatalf("повторный PutRecommendation: %v", err)
	}
	if updated.Revision != 2 {
		t.Errorf("revision = %d, ожидали 2", updated.Revision)
	}
	if updated.CreatedAt != created {
		t.Errorf("createdAt переписан: %q → %q", created, updated.CreatedAt)
	}

	if _, _, err := store.PutRecommendation(Recommendation{
		ID:       "agent-note",
		Title:    "Старая ревизия",
		Revision: 1,
	}); err == nil {
		t.Errorf("ожидалась ошибка конфликта ревизий")
	} else {
		conflict, ok := err.(*RevisionConflictError)
		if !ok {
			t.Fatalf("ошибка не RevisionConflictError: %T", err)
		}
		if conflict.Current != 2 || conflict.Sent != 1 {
			t.Errorf("конфликт current/sent = %d/%d, ожидали 2/1", conflict.Current, conflict.Sent)
		}
	}
}
