//go:build llama_stub

// seed_templates_r493f_test.go — R493f (2026-11-17).
//
// ЗАЧЕМ. Живой факт на стенде: страница «Справка и шаблоны» открывается,
// рекомендации в ней есть, а вкладка «Шаблоны» — ПУСТАЯ, применять нечего
// (`GET /api/v1/reference/templates` → items:[], total:0). Причина в коде сида:
// `SeedDoc` создавал `Templates: []Template{}` и на этом всё.
//
// Вторая половина задачи — УЖЕ СТОЯЩИЙ стенд. У него файл хранилища создан
// раньше, а решение №4 запрещает перезаписывать его сидом. Значит нужен добор:
// `EnsureSeedTemplates` добавляет ТОЛЬКО отсутствующие по id наборы, не трогая
// записи оператора и не переписывая существующие шаблоны.
package reference

import (
	"path/filepath"
	"testing"
)

// TestR493f_SeedContainsTemplates — сид обязан давать непустую вкладку шаблонов,
// и каждый шаблон обязан иметь источник (BasedOn на существующую запись).
func TestR493f_SeedContainsTemplates(t *testing.T) {
	const now = "2026-11-17T00:00:00Z"
	doc := SeedDoc(now)
	if len(doc.Templates) == 0 {
		t.Fatal("SeedDoc не создаёт ни одного шаблона: страница «Справка и шаблоны» " +
			"снова покажет пустую вкладку (живой дефект R493f)")
	}
	recs := make(map[string]bool, len(doc.Recommendations))
	for _, r := range doc.Recommendations {
		recs[r.ID] = true
	}
	for _, tmpl := range doc.Templates {
		if err := ValidateID(tmpl.ID); err != nil {
			t.Errorf("шаблон %q: id не проходит ValidateID: %v", tmpl.ID, err)
		}
		if tmpl.BasedOn == "" {
			t.Errorf("шаблон %q без BasedOn: в UI не видно, из какой записи он вырос", tmpl.ID)
		} else if !recs[tmpl.BasedOn] {
			t.Errorf("шаблон %q ссылается на несуществующую рекомендацию %q", tmpl.ID, tmpl.BasedOn)
		}
		if len(tmpl.Params) == 0 {
			t.Errorf("шаблон %q без параметров — применять нечего", tmpl.ID)
		}
		if tmpl.Revision != 1 {
			t.Errorf("шаблон %q: revision=%d, ожидалось 1", tmpl.ID, tmpl.Revision)
		}
	}
}

// TestR493f_SeedTemplateParamsMatchRecommendation — числа не дублируются:
// шаблон собирается из той же записи, поэтому правка в одном месте не расходится.
func TestR493f_SeedTemplateParamsMatchRecommendation(t *testing.T) {
	doc := SeedDoc("2026-11-17T00:00:00Z")
	byID := make(map[string]Recommendation, len(doc.Recommendations))
	for _, r := range doc.Recommendations {
		byID[r.ID] = r
	}
	for _, tmpl := range doc.Templates {
		rec := byID[tmpl.BasedOn]
		if len(rec.Params) == 0 {
			continue
		}
		if string(tmpl.Params) == "" {
			t.Fatalf("шаблон %q: пустые params", tmpl.ID)
		}
		// Сравниваем по составу имён: числа обязаны прийти из записи.
		names := ParamNames(rec.Params)
		for _, name := range names {
			if !containsSubstring(string(tmpl.Params), `"`+name+`"`) {
				t.Errorf("шаблон %q (из %q) не содержит параметр %q — числа разошлись с записью",
					tmpl.ID, tmpl.BasedOn, name)
			}
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestR493f_EnsureSeedTemplates_AddsMissingOnly — добор в существующий файл:
// добавляет отсутствующие, не переписывает существующие, не трогает записи.
func TestR493f_EnsureSeedTemplates_AddsMissingOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference.json")

	// «Старый» стенд: файл есть, рекомендации есть, шаблонов нет.
	old := SeedDoc("2026-11-16T00:00:00Z")
	old.Templates = []Template{}
	store := NewStore(path)
	store.mu.Lock()
	store.doc = old
	store.mu.Unlock()
	if err := store.saveLocked(); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}

	// Перезагружаем через Load — как при старте процесса.
	store2 := NewStore(path)
	store2.Load()
	if got := len(store2.Templates()); got != 0 {
		t.Fatalf("подготовка: шаблонов %d, ожидалось 0", got)
	}

	if added := store2.EnsureSeedTemplates(); added == 0 {
		t.Fatal("EnsureSeedTemplates не добавил ни одного шаблона на стенде без шаблонов")
	}

	// Идемпотентность: второй вызов ничего не добавляет.
	if added := store2.EnsureSeedTemplates(); added != 0 {
		t.Errorf("повторный добор добавил %d шаблонов — не идемпотентно", added)
	}

	// Записи оператора не тронуты.
	if len(store2.Recommendations()) != len(old.Recommendations) {
		t.Errorf("добор изменил число рекомендаций: было %d, стало %d",
			len(old.Recommendations), len(store2.Recommendations()))
	}

	// Файл на диске содержит шаблоны (иначе после рестарта снова пусто).
	reloaded := NewStore(path)
	reloaded.Load()
	if got := len(reloaded.Templates()); got == 0 {
		t.Error("после добора файл на диске не содержит шаблонов — при рестарте вкладка снова пустая")
	}
}

// TestR493f_EnsureSeedTemplates_KeepsOperatorTemplate — шаблон с тем же id, но
// изменённый оператором, НЕ перезаписывается: это его запись.
func TestR493f_EnsureSeedTemplates_KeepsOperatorTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference.json")

	seeded := seedTemplates("2026-11-16T00:00:00Z")
	if len(seeded) == 0 {
		t.Fatal("seedTemplates пуст — тест потерял смысл")
	}
	edited := seeded[0]
	edited.Name = "Мой набор (правил оператор)"
	edited.Comment = "переопределено"

	doc := SeedDoc("2026-11-16T00:00:00Z")
	doc.Templates = []Template{edited}

	store := NewStore(path)
	store.mu.Lock()
	store.doc = doc
	store.mu.Unlock()

	store.EnsureSeedTemplates()

	got, ok := store.Template(edited.ID)
	if !ok {
		t.Fatalf("шаблон %q исчез после добора", edited.ID)
	}
	if got.Name != "Мой набор (правил оператор)" {
		t.Errorf("добор перезаписал шаблон оператора: имя стало %q", got.Name)
	}
}
