//go:build llama_stub

// capacity_reads_guard_r501_test.go — R501 (2026-11-19), аудит S3, пункт C-2.
//
// ЗАЧЕМ. Вместимость бэкенда живёт в четырёх полях (MaxConcurrentReqs — статика
// конфига, RuntimeMaxConcurrentRequests — лимит оператора, RuntimeModelSlots —
// факт модели, NodeClaimedCapacity — заявка узла). Правило приоритета живёт в
// ОДНОМ месте — EffectiveMaxConcurrentRequests(). До правки 20+ боевых точек
// читали СТАТИКУ напрямую, и операторский лимит на этих путях не соблюдался:
// узел с n_parallel=1 и статикой 4 получал 4 параллельных запроса от селектора,
// скоринга и prewarm.
//
// Скомпилированный тест на такое правило не написать (чтение поля — не функция),
// поэтому проверка идёт по AST: вне allow-списка чтений `.MaxConcurrentReqs` быть
// не должно. Allow-список — места, где СТАТИКА и есть предмет: регистрация и
// нормализация, self-heal состояния, подсказка «что записать в конфиг», отчет,
// отличающий «не задано» от «эффективная ноль».
//
// Мутационная проверка: заменить в backend_selector.go одно чтение обратно на
// `state.Backend.MaxConcurrentReqs` — тест падает с координатой файла.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capacityStaticReadAllowR501 — файлы пакета balancer, которым можно читать
// статическое поле, и причина.
var capacityStaticReadAllowR501 = map[string]string{
	"backend_registry.go":        "регистрация, нормализация статики и запись заявки узла",
	"state.go":                   "reconcileNodeCapacity и LoadState работают со статикой по определению",
	"autosuggest.go":             "советует, какое значение записать В КОНФИГ",
	"backend_rejections_r460.go": "отличает «не задано в конфиге» от «эффективная ноль»",
}

// capacityStaticReadAllowAPI_R501 — то же для пакета api: конфиг-ручки.
var capacityStaticReadAllowAPI_R501 = map[string]bool{
	"handlers_backends.go":    true, // CRUD лимитов (то, что оператор записывает)
	"handlers_cluster.go":     true, // merge конфигурации копированием
	"handlers_agents.go":      true, // регистрация агента
	"handlers_autosuggest.go": true, // отчёт «что записать в конфиг»
}

// TestR501_StaticCapacityNotReadOnRoutingPaths — статику читают только файлы из
// allow-списка; на маршрутных путях — только EffectiveMaxConcurrentRequests().
func TestR501_StaticCapacityNotReadOnRoutingPaths(t *testing.T) {
	type pkg struct {
		// Порядок полей — под fieldalignment (govet в CI): сначала map, затем
		// строка, иначе объект занимает лишние 8 байт и линтер краснеет.
		allow map[string]string
		dir   string
	}
	pkgs := []pkg{
		{dir: ".", allow: capacityStaticReadAllowR501},
		{dir: "../../internal/api", allow: func() map[string]string {
			m := map[string]string{}
			for name := range capacityStaticReadAllowAPI_R501 {
				m[name] = "конфиг-ручка"
			}
			return m
		}()},
	}

	allowHits := 0
	for _, p := range pkgs {
		entries, err := os.ReadDir(p.dir)
		if err != nil {
			t.Fatalf("чтение каталога %s: %v", p.dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(p.dir, name), nil, 0)
			if err != nil {
				t.Fatalf("разбор %s: %v", name, err)
			}
			allowed := false
			if _, ok := p.allow[name]; ok {
				allowed = true
			}
			hits := 0
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil || sel.Sel.Name != "MaxConcurrentReqs" {
					return true
				}
				hits++
				if !allowed {
					t.Errorf("%s:%d: боевой путь читает СТАТИЧЕСКУЮ вместимость (%s) — "+
						"используйте EffectiveMaxConcurrentRequests() (аудит C-2)",
						name, fset.Position(sel.Pos()).Line, "MaxConcurrentReqs")
				}
				return true
			})
			if allowed {
				allowHits += hits
			}
		}
	}

	// Тест обязан быть непустым: если allow-список перестал совпадать с реальными
	// файлами (переименование/удаление), проверка ниже молча ничего не проверяет.
	if allowHits < 3 {
		t.Fatalf("в allow-списке найдено всего %d чтений статического поля: "+
			"проверка выродилась — сверьте список файлов", allowHits)
	}
}
