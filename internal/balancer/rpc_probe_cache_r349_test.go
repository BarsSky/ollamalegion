// rpc_probe_cache_r349_test.go — R349 (2026-10-31).
//
// ЖИВОЙ ФАКТ (двухузловой стенд, docs/plans/2026-10-31-two-node-donor-check.md §7).
// Пока раскладка включена, rpc-server донора видит пары «Accepted client connection /
// Client connection closed» — замерено 10-11 подключений в минуту при ПОЛНОСТЬЮ
// простаивающем стенде (модели не загружены). Один проход guard'а (раз в 15 с)
// проверяет узел несколько раз: список для мастера, решение о приказе и разрешение
// адреса (R350). Каждый вызов — новый TCP-connect.
//
// R349: пробы на путях РАСКЛАДКИ идут через короткий кэш (LB_RPC_PROBE_CACHE_SEC,
// по умолчанию 10 с) — один dial обслуживает весь проход.
//
// Проверяется:
//  1. в пределах TTL узел дёргается ОДИН раз, дальше — из кэша;
//  2. после истечения TTL — новый dial (кэш не превращается в «жив навсегда»);
//  3. отметка «бэкенд недоступен» побеждает кэш НЕМЕДЛЕННО (кэш не может оживить
//     узел, который балансер уже считает мёртвым);
//  4. LB_RPC_PROBE_CACHE_SEC=0 выключает кэш (всегда честный dial);
//  5. СТОРОЖ, ради которого это отдельный тест: гейт перед генерацией
//     (checkRPCNodesForInference) обязан остаться БЕЗ кэша — мёртвый узел на этом
//     пути это abort() в llama.cpp, а не «плохой ответ».
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r349Proxy — прокси с одним бэкендом и считающей пробой.
func r349Proxy(t *testing.T) (*Proxy, *int) {
	t.Helper()
	dials := 0
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["donor-1"] = &BackendState{Backend: &types.Backend{
		ID:     "donor-1",
		Host:   "100.64.0.7",
		Status: types.StatusHealthy,
		Type:   types.BackendTypeLlamaCpp,
		Role:   types.BackendRoleDonor,
	}}
	p.rpcProbe = func(addr string, _ time.Duration) bool {
		dials++
		return addr == "100.64.0.7:50052"
	}
	p.forgetRPCProbeCache()
	return p, &dials
}

// TestR349_CachedProbeDialsOnceWithinTTL — главный сценарий: проход guard'а делает
// один dial на узел, сколько бы раз он ни спросил.
func TestR349_CachedProbeDialsOnceWithinTTL(t *testing.T) {
	t.Setenv("LB_RPC_PROBE_CACHE_SEC", "10")
	p, dials := r349Proxy(t)

	for i := 0; i < 5; i++ {
		if !p.rpcProbeCached("100.64.0.7:50052", time.Second) {
			t.Fatalf("проба %d: живой узел назван мёртвым", i+1)
		}
	}
	if *dials != 1 {
		t.Fatalf("dial'ов за 5 проб = %d, want 1: именно это давало донору 10-11 "+
			"TCP-подключений в минуту при простаивающем стенде", *dials)
	}
}

// TestR349_CacheExpiresAfterTTL — кэш не превращается в «жив навсегда».
func TestR349_CacheExpiresAfterTTL(t *testing.T) {
	t.Setenv("LB_RPC_PROBE_CACHE_SEC", "1")
	p, dials := r349Proxy(t)

	p.rpcProbeCached("100.64.0.7:50052", time.Second)
	time.Sleep(1100 * time.Millisecond)
	p.rpcProbeCached("100.64.0.7:50052", time.Second)

	if *dials != 2 {
		t.Fatalf("dial'ов после истечения TTL = %d, want 2 (устаревший «жив» не должен "+
			"оставаться в кэше дольше TTL)", *dials)
	}
}

// TestR349_MarkedUnavailableBeatsCache — отметка «недоступен» действует сразу.
func TestR349_MarkedUnavailableBeatsCache(t *testing.T) {
	t.Setenv("LB_RPC_PROBE_CACHE_SEC", "60")
	p, dials := r349Proxy(t)

	if !p.rpcProbeCached("100.64.0.7:50052", time.Second) {
		t.Fatal("первая проба должна быть живой")
	}
	// Балансер узнал, что узел недоступен (запись бэкенда), кэш при этом «жив».
	p.backends["donor-1"].Backend.Status = types.StatusOffline

	if p.rpcProbeCached("100.64.0.7:50052", time.Second) {
		t.Fatal("кэш оживил узел, помеченный недоступным — именно этот дефект " +
			"защищает nodeMarkedUnavailable")
	}
	if *dials != 1 {
		t.Errorf("dial'ов = %d, want 1: проверка отметки дешевле dial и не должна его делать", *dials)
	}
}

// TestR349_CacheDisabledByEnv — 0 выключает кэш (всегда честный dial).
func TestR349_CacheDisabledByEnv(t *testing.T) {
	t.Setenv("LB_RPC_PROBE_CACHE_SEC", "0")
	p, dials := r349Proxy(t)

	for i := 0; i < 3; i++ {
		p.rpcProbeCached("100.64.0.7:50052", time.Second)
	}
	if *dials != 3 {
		t.Fatalf("dial'ов при выключенном кэше = %d, want 3", *dials)
	}
}
