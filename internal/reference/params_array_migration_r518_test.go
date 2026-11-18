package reference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// params_array_migration_r518_test.go — R518 (2026-11-19): params шаблона ДОЛЖНЫ
// быть плоским объектом, а сид писал МАССИВ.
//
// ЖИВОЙ ФАКТ (стенд, после R515/R516 — когда шаблоны наконец стали видны и
// применяемы из UI): у всех трёх seed-шаблонов `params` был массивом
// [{name,value}], потому что `seedTemplates` делал `json.Marshal(rec.Params)`, а
// `rec.Params` — это `[]Param`. Последствия:
//   - применение шаблона отвечало `400 invalid_params: params must be a JSON
//     object, got an array`;
//   - `usage.usedByProfiles` всегда приходил пустым (DecodeParamsObject падал),
//     то есть подсказка «совпадает с профилями» врала нулём.
//
// Контракт назван в самом типе: `Template.Params` — `{"contextLength":32768,...}`
// (reference.go:115-128), и именно его принимает PUT профиля модели.

// TestR518_SeedTemplatesHaveObjectParams — сид обязан давать объект.
func TestR518_SeedTemplatesHaveObjectParams(t *testing.T) {
	doc := SeedDoc("2026-11-19T00:00:00Z")
	if len(doc.Templates) == 0 {
		t.Fatalf("сид без шаблонов — проверка потеряла точку отсчёта")
	}
	for _, tmpl := range doc.Templates {
		obj, err := DecodeParamsObject(tmpl.Params)
		if err != nil {
			t.Fatalf("шаблон %q: params не объект: %v (%s)", tmpl.ID, err, string(tmpl.Params))
		}
		if len(obj) == 0 {
			t.Fatalf("шаблон %q: пустые params — применять нечего", tmpl.ID)
		}
	}
}

// TestR518_SeededStoreTemplatesApplyable — после первого сида store отдаёт шаблоны
// с объектными params (без ручной правки файла).
func TestR518_SeededStoreTemplatesApplyable(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "reference.json"))
	store.Load()
	templates := store.Templates()
	if len(templates) == 0 {
		t.Fatalf("store после сида без шаблонов")
	}
	for _, tmpl := range templates {
		if _, err := DecodeParamsObject(tmpl.Params); err != nil {
			t.Fatalf("шаблон %q из сида не применяем: %v", tmpl.ID, err)
		}
	}
}

// TestR518_LoadRepairsArrayParams — уже существующие файлы чинятся при загрузке.
func TestR518_LoadRepairsArrayParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference.json")
	arr, err := json.Marshal([]Param{
		{Name: "contextLength", Value: json.RawMessage("32768")},
		{Name: "kvCacheType", Value: json.RawMessage(`"q8_0"`)},
	})
	if err != nil {
		t.Fatalf("собрать массив params: %v", err)
	}
	doc := Doc{
		Version: SchemaVersion,
		Templates: []Template{{
			ID: "broken-array-params", Name: "сломанный", Version: 1,
			Params: arr, Revision: 1,
		}},
		Recommendations: []Recommendation{},
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("записать файл: %v", err)
	}

	store := NewStore(path)
	store.Load()

	templates := store.Templates()
	if len(templates) != 1 {
		t.Fatalf("шаблонов в store: %d, want 1", len(templates))
	}
	obj, err := DecodeParamsObject(templates[0].Params)
	if err != nil {
		t.Fatalf("после загрузки params не починен: %v (%s)", err, string(templates[0].Params))
	}
	if string(obj["contextLength"]) != "32768" || string(obj["kvCacheType"]) != `"q8_0"` {
		t.Fatalf("значения params потерялись при починке: %v", obj)
	}

	// Починка обязана попасть на диск: иначе после рестарта всё снова сломано.
	reloaded := NewStore(path)
	reloaded.Load()
	if _, err := DecodeParamsObject(reloaded.Templates()[0].Params); err != nil {
		t.Fatalf("на диск починка не записана: %v", err)
	}
}

// TestR518_UnparsableParamsStayUntouched — мусор не «починяем» молча.
func TestR518_UnparsableParamsStayUntouched(t *testing.T) {
	raw := json.RawMessage(`"not params at all"`)
	fixed, changed := repairTemplateParams(raw)
	if changed {
		t.Fatalf("мусорные params переписаны: %s", string(fixed))
	}
	if string(fixed) != string(raw) {
		t.Fatalf("мусорные params изменены: было %s, стало %s", string(raw), string(fixed))
	}
}
