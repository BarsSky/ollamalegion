//go:build llama_stub

// node_capacity_r121_test.go — R121 (2026-10-15), пункт 1: заявка узла о
// вместимости обязана переживать перезапуск балансера.
//
// ЖИВОЙ ЗАМЕР (node _diag/probe-capacity.js):
//
//	до:   cppworker-gpu-bundled-agent | effective=2 | source=static
//	после рестарта cppworker (агент зарегистрировался заново):
//	      cppworker-gpu-bundled-agent | effective=1 | source=static   ← верно, но БЕЗ владельца
//	после рестарта балансера:
//	      cppworker-gpu-bundled-agent | effective=2 | source=static   ← заявка узла потеряна
//
// Причина: заявка узла (1) записывалась в статическое MaxConcurrentReqs, а
// MaxConcurrentReqs читается из config.json при каждом старте балансера (там 2).
// Агент заново не регистрируется (он регистрируется только при СВОЁМ старте),
// поэтому значение оставалось 2, и heartbeat нёс узлу 2.
package balancer

import (
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// nodeClaimProxy — прокси с одним llama.cpp-бэкендом, у которого статика
// конфига равна 2 (ровно как на стенде), а заявки узла ещё нет.
func nodeClaimProxy(t *testing.T) (*Proxy, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
		Backends: []types.Backend{{
			ID:                "cppworker-gpu",
			Host:              "cppworker-gpu",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			MaxConcurrentReqs: 2, // статика конфига: ровно тот «2» со стенда
			Labels:            []string{"auto-registered"},
		}},
	}
	p := newProxyWithCleanup(t, cfg)
	return p, statePath
}

// TestR121_AdoptCapacityFromNode_RecordsClaim — заявка узла попадает в СВОЁ поле
// и становится владельцем вместимости (источник — «от ноды», а не «static»).
func TestR121_AdoptCapacityFromNode_RecordsClaim(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	p, _ := nodeClaimProxy(t)

	p.AdoptCapacityFromNode("cppworker-gpu", 1)

	got := p.GetBackend("cppworker-gpu")
	if got.NodeClaimedCapacity != 1 {
		t.Errorf("NodeClaimedCapacity = %d, want 1 (агент сообщил n_parallel=1)",
			got.NodeClaimedCapacity)
	}
	if !got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = false: заявка узла снова живёт в статике и " +
			"потеряется при перезапуске балансера")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, want 1: статика конфига (2) "+
			"перебивает заявку узла — это и есть живой дефект", eff)
	}
	if _, source := got.ResolveCapacity(); source != types.CapacitySourceNode {
		t.Errorf("source = %q, want %q: на стенде отчёт врал «static» при значении от узла",
			source, types.CapacitySourceNode)
	}
}

// TestR121_AdoptCapacityFromNode_OperatorThrottleNotOverwritten — если оператор
// задал троттлинг, заявка узла НЕ перезаписывает runtime-лимит, но фиксируется
// как факт (резолвер применит понижение сам).
func TestR121_AdoptCapacityFromNode_OperatorThrottleNotOverwritten(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	p, _ := nodeClaimProxy(t)

	// Оператор: не больше 2 параллельно (путь PUT /limits снимает владельца-ноду).
	if err := p.UpdateBackendLimits("cppworker-gpu", 1, 2); err != nil {
		t.Fatalf("UpdateBackendLimits: %v", err)
	}
	// Узел заявляет 1.
	p.AdoptCapacityFromNode("cppworker-gpu", 1)

	got := p.GetBackend("cppworker-gpu")
	if got.RuntimeMaxConcurrentRequests != 2 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, want 2: заявка узла не имеет права "+
			"перетирать операторский троттлинг", got.RuntimeMaxConcurrentRequests)
	}
	if got.NodeClaimedCapacity != 1 {
		t.Errorf("NodeClaimedCapacity = %d, want 1 (факт от узла обязан фиксироваться)", got.NodeClaimedCapacity)
	}
	// Узел физически обслуживает 1 → лимит оператора может только понизить.
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, want 1 (узел заявил 1)", eff)
	}
}

// TestR121_NodeClaim_SurvivesBalancerRestart — главное: после перезапуска
// балансера конфиг снова даёт статику 2, но вместимость обязана остаться 1.
func TestR121_NodeClaim_SurvivesBalancerRestart(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")

	// 1. Живой балансер: узел заявил 1, состояние сохранено на диск.
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
		Backends: []types.Backend{{
			ID:                "cppworker-gpu",
			Host:              "cppworker-gpu",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			MaxConcurrentReqs: 2,
			Labels:            []string{"auto-registered"},
		}},
	}
	p := newProxyWithCleanup(t, cfg)
	p.AdoptCapacityFromNode("cppworker-gpu", 1)
	if err := p.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// 2. Перезапуск: новый прокси с ТЕМ ЖЕ конфигом (статика 2) и состоянием.
	restarted := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "cppworker-gpu",
		Host:              "cppworker-gpu",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 2, // конфиг остался прежним
	}})

	got := restarted.GetBackend("cppworker-gpu")
	if got == nil {
		t.Fatal("бэкенд не восстановлен из state.json")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("после перезапуска EffectiveMaxConcurrentRequests = %d, want 1: "+
			"статика конфига (2) снова победила заявку узла. Это живой дефект: "+
			"admission-очередь пускает два запроса на узел, который сериализует "+
			"llama_decode, и второй клиент ждёт МОЛЧА внутри cppworker", eff)
	}
	if _, source := got.ResolveCapacity(); source != types.CapacitySourceNode {
		t.Errorf("после перезапуска source = %q, want %q", source, types.CapacitySourceNode)
	}
}
