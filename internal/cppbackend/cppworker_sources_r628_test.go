//go:build llama_stub

// cppworker_sources_r628_test.go — R628 (2026-11-19): единый способ прочитать
// исходники `cmd/cppworker/handlers_model*.go` для сторожей R131/R132.
//
// ЗАЧЕМ. Шаги R622–R624 разложили `handlers_model.go` (2782 строки) на четыре файла:
// сам `handlers_model.go` (загрузка/выгрузка и прогресс), `handlers_model_list.go`
// (`handleListModels`), `handlers_model_dir.go` (каталог моделей на диске) и
// `handlers_model_load_params.go` (разбор параметров загрузки). Сторожа R131/R132
// искали `handleListModels` по конкретному имени файла — и покраснели не потому, что
// дозвон вернулся, а потому что файл переехал:
//
//	в handlers_model.go нет функции handleListModels — тест устарел
//
// Это ложная тревога, но опасная: следующий, кто её увидит, скорее поверит тесту
// («дозвон вернулся») или удалит проверку. Поэтому чтение вынесено в helper, который
// знает про перенос, — и следующий перенос кода между этими файлами ничего не сломает.
//
// ВАЖНО, ЧЕГО HELPER НЕ ДЕЛАЕТ: он не «успокаивает» проверку. Свойство из R132
// («справочная ручка не дозванивается») проверяется по объединённому тексту всех
// файлов: если вызов дозвона появится в ЛЮБОМ из них, тест по-прежнему красный.
package cppbackend

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cppworkerModelFilesR628 — каталог с исходниками воркера относительно internal/cppbackend.
const cppworkerModelDirR628 = "../../cmd/cppworker"

// cppworkerModelSourceR628 — склеенный текст всех боевых `handlers_model*.go`.
func cppworkerModelSourceR628(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(cppworkerModelDirR628)
	if err != nil {
		t.Fatalf("чтение %s: %v", cppworkerModelDirR628, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "handlers_model") || !strings.HasSuffix(n, ".go") {
			continue
		}
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		t.Fatal("не найдено ни одного handlers_model*.go: сторож вырожден")
	}
	sort.Strings(names) // порядок обхода не должен зависеть от файловой системы
	var sb strings.Builder
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(cppworkerModelDirR628, n))
		if err != nil {
			t.Fatalf("чтение %s: %v", n, err)
		}
		sb.Write(raw)
		sb.WriteString("\n")
	}
	return sb.String()
}

// funcBodyInSourceR628 — тело функции `name` из склеенного текста.
//
// Границы — до следующего `\nfunc `, как в funcBody (R132): тест проверяет «в этой
// функции нет вызовов с дозвоном», а не «в файле»; разница важна, потому что в
// соседних функциях вызовы с дозвоном быть обязаны (путь решения).
func funcBodyInSourceR628(t *testing.T, src, name string) string {
	t.Helper()
	marker := "func " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("в исходниках cmd/cppworker нет функции %s — тест устарел", name)
	}
	rest := src[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}
