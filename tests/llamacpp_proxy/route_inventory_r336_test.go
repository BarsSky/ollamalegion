// route_inventory_r336_test.go — стюард «маршрут зарегистрирован, но не проверен».
//
// ЗАЧЕМ. Матрица (R335) проверяет контракт по списку маршрутов. Список живёт в
// тесте, а регистрация — в роутерах; если кто-то добавит маршрут в роутер и
// забудет про матрицу, контракт нового маршрута никто не проверит — и мы снова
// узнаем о расхождении от оператора, а не от теста.
//
// Стюард читает `case "..."` из роутеров (llamacpp_router.go, ollama_router.go) и
// сверяет с тем, что матрица обязана покрывать. Добавил маршрут — добавь строку в
// coveredMatrixRoutes (и тест в матрицу).
package llamacpp_proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// matrixRouterFiles — файлы, где регистрируются клиентские маршруты.
var matrixRouterFiles = []string{
	"../../internal/balancer/llamacpp_router.go",
	"../../internal/balancer/ollama_router.go",
}

// coveredMatrixRoutes — маршруты, которые обязана проверять матрица (R335).
//
// Пути с параметром ({id}, динамические префиксы) сюда попадают в том виде, в
// котором они встречаются в `case` роутера.
var coveredMatrixRoutes = map[string]bool{
	// Ollama read-only / mgmt
	"/api/version": true,
	"/api/tags":    true,
	"/api/ps":      true,
	"/api/show":    true,
	"/api/models":  true,
	// Ollama inference
	"/api/chat":       true,
	"/api/generate":   true,
	"/api/embed":      true,
	"/api/embeddings": true,
	// OpenAI
	"/v1/models":            true,
	"/v1/chat/completions":  true,
	"/v1/completions":       true,
	"/v1/embeddings":        true,
	// Anthropic / Responses: проверяются живой матрицей (этап 1); в стаб-матрицу
	// не входят — заглушка их не эмулирует. Держим их здесь, чтобы стюард не
	// падал, и это решение явно записано.
	"/v1/messages":              true,
	"/v1/messages/count_tokens": true,
	"/v1/responses":             true,
	// Служебные маршруты воркера, которые балансер проксирует как есть.
	"/api/models/files":             true,
	// Реестровые ручки Ollama (R336): проверяются TestR335_RegistryMgmtMatrix.
	"/api/pull":   true,
	"/api/push":   true,
	"/api/create": true,
	"/api/delete": true,
	"/api/copy":   true,
	"/api/v1/cppworker/config/runtime": true,
}

// TestR336_EveryRegisteredRouteIsCovered — стюард по исходникам роутеров.
func TestR336_EveryRegisteredRouteIsCovered(t *testing.T) {
	registered := map[string]string{} // path → файл, где зарегистрирован
	for _, file := range matrixRouterFiles {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, filepath.Clean(file), nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", file, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				path, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/") {
					registered[path] = filepath.Base(file)
				}
			}
			return true
		})
	}
	if len(registered) == 0 {
		t.Fatal("не найдено ни одного зарегистрированного маршрута — стюард сломался (изменилась структура роутеров)")
	}

	var missing []string
	for path, file := range registered {
		if !coveredMatrixRoutes[path] {
			missing = append(missing, path+" ("+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("маршруты зарегистрированы, но отсутствуют в матрице контракта (%d):\n  %s\n"+
			"Добавьте путь в coveredMatrixRoutes и проверку в matrix_contract_r335_test.go — иначе "+
			"контракт этого маршрута никто не проверяет (класс дефекта: расхождение находит оператор, а не тест)",
			len(missing), strings.Join(missing, "\n  "))
	}
}
