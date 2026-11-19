package api

// selection_transparency_r493_test.go — сторож долга 12n после удаления поля.
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА (2026-11-17): «вычистить поле, совместимость не нужна».
// Почему сторож всё равно нужен: поле было «настройкой без эффекта» и уже один
// раз стоило красного CI и вопросов «почему не переключается». Вернуть его можно
// одной строкой в структуре — и тогда API снова начнёт обещать переключатель,
// которого нет. Поэтому проверяем: поля нет, GET не отдаёт сырое значение,
// PUT отвечает явным отказом, а остаток `LB_ALGORITHM` в окружении не молчит.

import (
	"os"
	"strings"
	"testing"
)

func TestR493_AlgorithmFieldRemoved(t *testing.T) {
	// 1) Эффективное значение объявлено ровно одно и оно правдиво.
	if effectiveSelectionAlgorithmR493 != "unified-multistage" {
		t.Fatalf("effectiveSelectionAlgorithmR493 = %q, ожидалось unified-multistage", effectiveSelectionAlgorithmR493)
	}

	// 2) Поля `Balancing.Algorithm` больше нет ни в одном продакшн-файле.
	// Ищем только НЕтестовые файлы: в тестах слово встречается в комментариях.
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("не прочитать каталог: %v", err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("не прочитать %s: %v", f.Name(), err)
		}
		if strings.Contains(string(src), "Balancing.Algorithm") && f.Name() != "selection_transparency.go" {
			t.Errorf("%s: обращение к удалённому полю Balancing.Algorithm — настройка без эффекта вернулась", f.Name())
		}
	}

	// 3) GET-ответы отдают ФАКТ выбора и не отдают сырое поле. Три места
	// (карточка конфига, снимок кластера, ответ сброса) — по одному вставлению.
	src, err := os.ReadFile("handlers_cluster.go")
	if err != nil {
		t.Fatalf("не прочитать handlers_cluster.go: %v", err)
	}
	// Вставок 4: три GET-ответа (карточка конфига, снимок кластера, ответ
	// сброса) + тело отказа PUT — там это подсказка «что работает вместо».
	if n := strings.Count(string(src), "effectiveSelectionAlgorithmR493"); n != 4 {
		t.Errorf("вставок effectiveSelectionAlgorithmR493: %d, ожидалось 4 (3 GET + отказ PUT)", n)
	}
	if strings.Contains(string(src), `"algorithm":`) {
		t.Error("GET снова отдаёт сырое поле algorithm — оно удалено, вместо него effectiveAlgorithm")
	}
}

// TestR493_PutRejectsAlgorithmField — PUT с полем обязан отказать ЯВНО, а не
// проигнорировать: тихое игнорирование и было исходным дефектом.
func TestR493_PutRejectsAlgorithmField(t *testing.T) {
	src, err := os.ReadFile("handlers_cluster.go")
	if err != nil {
		t.Fatalf("не прочитать handlers_cluster.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`if req.Algorithm != "" {`,
		`"error":              "algorithm_removed"`,
		`"effectiveAlgorithm": effectiveSelectionAlgorithmR493`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("в PUT нет %q — отказ перестанет быть явным", want)
		}
	}
}

// TestR493_AlgorithmWarnThrottled — остаток LB_ALGORITHM предупреждается один раз
// на (место, значение); пустое значение не предупреждается вовсе.
func TestR493_AlgorithmWarnThrottled(t *testing.T) {
	resetAlgorithmWarnSeenR493()
	warnAlgorithmFieldRemovedR493("test", "roundrobin")
	warnAlgorithmFieldRemovedR493("test", "roundrobin")
	algorithmWarnMuR493.Lock()
	seen := algorithmWarnSeenR493["test:roundrobin"]
	count := len(algorithmWarnSeenR493)
	algorithmWarnMuR493.Unlock()
	if !seen {
		t.Fatal("значение не помечено как предупреждённое")
	}
	if count != 1 {
		t.Fatalf("в карте %d значений, ожидалось 1 (повтор не должен добавлять записей)", count)
	}
	warnAlgorithmFieldRemovedR493("test", "")
	algorithmWarnMuR493.Lock()
	count = len(algorithmWarnSeenR493)
	algorithmWarnMuR493.Unlock()
	if count != 1 {
		t.Fatalf("пустое значение добавило запись в карту (стало %d)", count)
	}
	// Другое место предупреждается отдельно: иначе PUT «съел» бы warn при старте.
	warnAlgorithmFieldRemovedR493("other", "roundrobin")
	algorithmWarnMuR493.Lock()
	count = len(algorithmWarnSeenR493)
	algorithmWarnMuR493.Unlock()
	if count != 2 {
		t.Fatalf("после другого места в карте %d значений, ожидалось 2", count)
	}
	resetAlgorithmWarnSeenR493()
}
