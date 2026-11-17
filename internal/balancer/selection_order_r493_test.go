// selection_order_r493_test.go — R493 (2026-11-17): шов порядка выбора.
//
// ПОЧЕМУ ТЕСТ ЕСТЬ, А РЕЖИМ ВЫКЛЮЧЕН ПО УМОЛЧАНИЮ. `leastconn` — это смена
// распределения трафика, и включать её молча нельзя. Но шов обязан быть
// проверяемым уже сейчас, иначе через месяц окажется, что он «есть», а на деле
// ничего не делает (ровно класс дефекта 12n).
//
// Проверяется: (1) дефолт не меняет порядок; (2) leastconn ставит менее занятый
// узел первым и сохраняет исходный порядок при равенстве; (3) функция НЕ меняет
// состав и количество кандидатов; (4) неизвестное значение флага → unified +
// одно предупреждение.
package balancer

import (
	"reflect"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// orderProxyR493 — прокси с бэкендами и назначенными ActiveReqs.
func orderProxyR493(t *testing.T, active map[string]int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	for id, a := range active {
		p.backends[id] = &BackendState{Backend: &types.Backend{ID: id, Status: types.StatusHealthy}}
		p.backends[id].ActiveReqs = a
	}
	return p
}

func TestR493_SelectionOrderDefaultUnchanged(t *testing.T) {
	t.Setenv("LB_SELECTION_ORDER", "")
	p := orderProxyR493(t, map[string]int{"free": 5, "busy": 9})
	// Модель неизвестна по размеру → fit-порядок не меняется, значит вход = выход.
	got := p.orderCandidatesForSelectionR493([]string{"free", "busy"}, "unknown-model")
	want := []string{"free", "busy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("дефолтный режим изменил порядок: %v, ожидалось %v", got, want)
	}
}

func TestR493_SelectionOrderLeastConn(t *testing.T) {
	t.Setenv("LB_SELECTION_ORDER", "leastconn")
	p := orderProxyR493(t, map[string]int{"a": 3, "b": 0, "c": 7})
	got := p.orderCandidatesForSelectionR493([]string{"a", "b", "c"}, "unknown-model")
	want := []string{"b", "a", "c"} // 0, 3, 7 активных
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("leastconn порядок = %v, ожидалось %v", got, want)
	}
}

func TestR493_SelectionOrderLeastConnTieKeepsBaseOrder(t *testing.T) {
	t.Setenv("LB_SELECTION_ORDER", "leastconn")
	p := orderProxyR493(t, map[string]int{"x": 2, "y": 2, "z": 1})
	got := p.orderCandidatesForSelectionR493([]string{"x", "y", "z"}, "unknown-model")
	want := []string{"z", "x", "y"} // z (1) вперёд; при равных 2 сохраняется x,y
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("порядок при равенстве = %v, ожидалось %v (исходный порядок обязан сохраняться)", got, want)
	}
}

func TestR493_SelectionOrderKeepsCandidates(t *testing.T) {
	t.Setenv("LB_SELECTION_ORDER", "leastconn")
	p := orderProxyR493(t, map[string]int{"a": 1, "b": 2})
	in := []string{"a", "b"}
	got := p.orderCandidatesForSelectionR493(in, "m")
	if len(got) != len(in) {
		t.Fatalf("длина изменилась: %d → %d", len(in), len(got))
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range in {
		if !seen[id] {
			t.Fatalf("кандидат %q потерян: %v", id, got)
		}
	}
	// Один кандидат и пустой список — без паники и без изменений.
	if g := p.orderCandidatesForSelectionR493(nil, "m"); len(g) != 0 {
		t.Fatalf("пустой вход дал %v", g)
	}
	if g := p.orderCandidatesForSelectionR493([]string{"a"}, "m"); !reflect.DeepEqual(g, []string{"a"}) {
		t.Fatalf("один кандидат изменился: %v", g)
	}
}

func TestR493_SelectionOrderUnknownValueFallsBackToUnified(t *testing.T) {
	resetSelectionOrderWarningsR493()
	t.Setenv("LB_SELECTION_ORDER", "least-connections") // «красивое» имя, но не наше
	if got := selectionOrderModeR493(); got != selectionOrderUnifiedR493 {
		t.Fatalf("режим = %q, ожидался unified (неизвестное значение не включает новый путь)", got)
	}
	// И одно предупреждение, а не по одному на каждое обращение.
	selectionOrderWarnOnceR493("least-connections")
	selectionOrderWarnMuR493.Lock()
	n := len(selectionOrderWarnedR493)
	selectionOrderWarnMuR493.Unlock()
	if n != 1 {
		t.Fatalf("в карте предупреждений %d значений, ожидалось 1", n)
	}
	resetSelectionOrderWarningsR493()
}

func TestR493_SelectionOrderModeValues(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", selectionOrderUnifiedR493},
		{"unified", selectionOrderUnifiedR493},
		{"UNIFIED", selectionOrderUnifiedR493},
		{" leastconn ", selectionOrderLeastConnR493},
		{"LEASTCONN", selectionOrderLeastConnR493},
		{"roundrobin", selectionOrderUnifiedR493}, // остаток старого поля не включает ничего
	}
	for _, c := range cases {
		t.Setenv("LB_SELECTION_ORDER", c.env)
		if got := selectionOrderModeR493(); got != c.want {
			t.Errorf("LB_SELECTION_ORDER=%q → %q, ожидалось %q", c.env, got, c.want)
		}
	}
	t.Setenv("LB_SELECTION_ORDER", "")
	resetSelectionOrderWarningsR493()
}
