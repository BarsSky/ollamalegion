package api

// algorithm_transparency_r493_test.go — сторож долга 12n: настройка `algorithm`
// не переключает селектор, и это должно оставаться видимым.
//
// ПОЧЕМУ СТРАЖ. Поведение клиента не меняется (значение принимается как
// раньше), меняется только честность: предупреждение в лог при старте/смене/
// импорте и `effectiveAlgorithm` в ответе API. Такое ломается молча — правкой,
// которая «просто убрала лишний warn»; поэтому проверяем исходники, как и
// остальные сторожа серии R493.

import (
	"os"
	"strings"
	"testing"
)

func TestR493_AlgorithmFieldIsTransparent(t *testing.T) {
	// 1) Эффективное значение объявлено ровно одно.
	if effectiveSelectionAlgorithmR493 != "unified-multistage" {
		t.Fatalf("effectiveSelectionAlgorithmR493 = %q, ожидалось unified-multistage", effectiveSelectionAlgorithmR493)
	}

	// 2) Три точки, где значение приходит: старт, PUT конфига, импорт.
	for _, c := range []struct{ file, where string }{
		{"handlers_cluster.go", "startup"},
		{"handlers_cluster.go", "PUT /api/v1/cluster/config"},
		{"handlers_config_export.go", "config import"},
	} {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", c.file, err)
		}
		if !strings.Contains(string(src), `"`+c.where+`"`) {
			t.Errorf("%s: нет предупреждения для %q — оператор снова не узнает, "+
				"что algorithm не переключает выбор", c.file, c.where)
		}
	}

	// 3) GET отдаёт и запрошенное, и эффективное значение. Три ответа в
	// handlers_cluster.go: карточка конфига, экспорт и снимок кластера.
	src, err := os.ReadFile("handlers_cluster.go")
	if err != nil {
		t.Fatalf("не прочитать handlers_cluster.go: %v", err)
	}
	if n := strings.Count(string(src), "effectiveSelectionAlgorithmR493"); n != 3 {
		t.Errorf("effectiveAlgorithm в ответах API: %d вставок, ожидалось 3 (по одной на "+
			"GET-ответ с полем algorithm)", n)
	}

	// 4) Предупреждение не должно повторяться на каждый PUT (карта «уже сказали»).
	h, err := os.ReadFile("algorithm_transparency_r493.go")
	if err != nil {
		t.Fatalf("не прочитать helper: %v", err)
	}
	if !strings.Contains(string(h), "algorithmWarnSeenR493") {
		t.Error("helper не помнит уже предупреждённые значения — warn будет на каждый PUT")
	}
}

// TestR493_AlgorithmWarnThrottled — одно и то же значение предупреждается один
// раз за процесс (иначе пачка PUT'ов даст пачку одинаковых warn).
func TestR493_AlgorithmWarnThrottled(t *testing.T) {
	resetAlgorithmWarnSeenR493()
	logAlgorithmIgnoredR493("test", "roundrobin")
	logAlgorithmIgnoredR493("test", "roundrobin")
	algorithmWarnMuR493.Lock()
	seen := algorithmWarnSeenR493["roundrobin"]
	count := len(algorithmWarnSeenR493)
	algorithmWarnMuR493.Unlock()
	if !seen {
		t.Fatal("значение не помечено как предупреждённое")
	}
	if count != 1 {
		t.Fatalf("в карте %d значений, ожидалось 1 (повтор не должен добавлять записей)", count)
	}
	// Пустое значение — не повод для warn.
	logAlgorithmIgnoredR493("test", "")
	algorithmWarnMuR493.Lock()
	count = len(algorithmWarnSeenR493)
	algorithmWarnMuR493.Unlock()
	if count != 1 {
		t.Fatalf("пустое значение добавило запись в карту (стало %d)", count)
	}
	resetAlgorithmWarnSeenR493()
}
