package balancer

// queue_budget_fields_d41_test.go — D4.1 (2026-11-25): «сколько ждать» больше не
// одно безымянное число.
//
// ЖИВОЙ ДЕФЕКТ (реестр дефектов 2026-11-24, D4.1). В архиве второго стенда
// оператор видел `queue_stats.admission.wait_max_sec=900` при
// `LB_ADMISSION_WAIT_SEC=300` и делал вывод «лимит очереди — 900 секунд».
// На деле 900 — это бюджет ЗАПРОСА: max(ожидание слота, ожидание загрузки
// модели). Правило верное, но из отчёта не было видно, из чего сложилось
// число, поэтому панель и разбор читали его как лимит очереди.
//
// ПРАВИЛО. `admission.wait_max_sec` остаётся бюджетом (совместимость), рядом
// публикуются оба слагаемых: `admission_wait_max_sec` (только слот),
// `auto_load_wait_sec` (только загрузка) и `request_wait_budget_sec` (максимум).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestD41_BudgetIsMaxOfAdmissionAndAutoLoad — числа: при admission=300 и
// auto-load=900 бюджет обязан быть 900, а слагаемые — 300 и 900.
func TestD41_BudgetIsMaxOfAdmissionAndAutoLoad(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "300")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "900")

	p := &Proxy{admission: newAdmissionQueue(0)}

	if got := int(p.admissionWaitTimeout().Seconds()); got != 300 {
		t.Fatalf("admissionWaitTimeout = %d c, want 300", got)
	}
	if got := int(AutoLoadWaitTimeout().Seconds()); got != 900 {
		t.Fatalf("AutoLoadWaitTimeout = %d c, want 900", got)
	}
	if got := int(p.admissionWaitBudget().Seconds()); got != 900 {
		t.Fatalf("admissionWaitBudget = %d c, want 900 (max(300, 900))", got)
	}
}

// TestD41_BudgetFallsBackToAdmission — при выключенном автозагрузочном ожидании
// бюджет равен лимиту очереди (не ноль и не «бесконечность»).
func TestD41_BudgetFallsBackToAdmission(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "120")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	p := &Proxy{admission: newAdmissionQueue(0)}
	if got := int(p.admissionWaitBudget().Seconds()); got != 120 {
		t.Fatalf("admissionWaitBudget = %d c, want 120 (auto-load выключен)", got)
	}
}

// TestD41_WireNamesPresent — страж проводки: три поля обязаны быть объявлены с
// JSON-именами и присвоены при сборке QueueStats. Иначе числа посчитаны, но
// наружу не выходят, и дефект «оператор читает не то» вернётся молча.
func TestD41_WireNamesPresent(t *testing.T) {
	fset := token.NewFileSet()

	af, err := parser.ParseFile(fset, "queue_manager.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать queue_manager.go: %v", err)
	}
	tags := map[string]bool{}
	ast.Inspect(af, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Tag == nil {
			return true
		}
		for _, name := range []string{
			"admission_wait_max_sec", "auto_load_wait_sec", "request_wait_budget_sec",
		} {
			if strings.Contains(field.Tag.Value, name) {
				tags[name] = true
			}
		}
		return true
	})
	for _, name := range []string{
		"admission_wait_max_sec", "auto_load_wait_sec", "request_wait_budget_sec",
	} {
		if !tags[name] {
			t.Errorf("в QueueStats нет поля с json-именем %q — слагаемые бюджета не публикуются", name)
		}
	}

	cf, err := parser.ParseFile(fset, "cluster_state.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать cluster_state.go: %v", err)
	}
	assigned := map[string]bool{}
	ast.Inspect(cf, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok {
			assigned[id.Name] = true
		}
		return true
	})
	for _, name := range []string{
		"AdmissionWaitMaxSec", "AutoLoadWaitSec", "RequestWaitBudgetSec",
	} {
		if !assigned[name] {
			t.Errorf("QueueStats.%s не присвоен при сборке статистики (cluster_state.go)", name)
		}
	}
}
