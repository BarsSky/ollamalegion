//go:build llama_stub

// unload_routing_r503_test.go — R503 (2026-11-19), аудит S4, пункт X-2.
//
// ДЕФЕКТ (два звена, оба в одном планировщике):
//  1. «защита от выгрузки активных моделей» читала `queueMgr.processing`/`pending` —
//     legacy-списки, которые после R78 ВСЕГДА пусты. То есть модель, обслуживающая
//     живой запрос, считалась простаивающей;
//  2. «выгрузка» = метка `WarmingUpModels[lru_unload]` на 2 минуты. Метка означает
//     «модель грузится»: она уводит бэкенд в P2 селектора (режим ожидания загрузки),
//     публикуется в монитор и исключает бэкенд из размещения в
//     model_instance_controller. Реальной выгрузки при этом не происходило —
//     у Ollama нет endpoint'а, а планировщик смотрит именно Ollama-метрики.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// unloadR503Proxy — прокси с одним здоровым llama-бэкендом и метриками Ollama.
func unloadR503Proxy(t *testing.T, idleModels ...string) (*Proxy, *UnloadScheduler) {
	t.Helper()
	p := availProxyR493(t, map[string]int{"b1": 4})
	p.admission = newAdmissionQueue(time.Second)
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID: "b1",
		Ollama: types.OllamaMetrics{
			RunningModels: func() []types.RunningModel {
				out := make([]types.RunningModel, 0, len(idleModels))
				for _, name := range idleModels {
					out = append(out, types.RunningModel{Name: name})
				}
				return out
			}(),
		},
	}
	p.metricsMgr.mu.Unlock()

	us := NewUnloadScheduler(p)
	us.enabled = true
	us.idleTimeout = time.Minute
	for _, name := range idleModels {
		us.lastUsed[name+"@b1"] = time.Now().Add(-10 * time.Minute)
	}
	return p, us
}

// TestR503_IdleModelIsCandidateWhenBackendFree — контрольная точка: без нагрузки
// простаивающая модель действительно кандидат (иначе проверки ниже ничего не
// доказывают).
func TestR503_IdleModelIsCandidateWhenBackendFree(t *testing.T) {
	_, us := unloadR503Proxy(t, "m1")
	cands := us.getUnloadCandidates()
	if len(cands) != 1 || cands[0].Model != "m1" {
		t.Fatalf("простаивающая модель без нагрузки не стала кандидатом: %+v", cands)
	}
}

// TestR503_BusyBackendIsProtected — X-2: бэкенд с занятым слотом не отдаёт модели
// на «выгрузку», даже если по времени простоя они подходят.
func TestR503_BusyBackendIsProtected(t *testing.T) {
	p, us := unloadR503Proxy(t, "m1")
	p.backends["b1"].ActiveReqs = 1
	if cands := us.getUnloadCandidates(); len(cands) != 0 {
		t.Fatalf("бэкенд обслуживает запрос (ActiveReqs=1), а модель выставлена на "+
			"выгрузку: %+v (аудит X-2: защита читала мёртвые legacy-списки)", cands)
	}
}

// TestR503_AdmissionQueueIsProtected — X-2: бэкенд с очередью в admission занят.
func TestR503_AdmissionQueueIsProtected(t *testing.T) {
	p, us := unloadR503Proxy(t, "m1")
	p.admission.enqueue("b1", "sess-waiting")
	if cands := us.getUnloadCandidates(); len(cands) != 0 {
		t.Fatalf("на бэкенде стоит очередь admission, а модель выставлена на выгрузку: %+v", cands)
	}
}

// TestR503_ActiveStreamIsProtected — X-2: живой поток сессии (LastRequestAt не
// обновляется до завершения) защищает модель.
func TestR503_ActiveStreamIsProtected(t *testing.T) {
	p, us := unloadR503Proxy(t, "m1")
	p.sessionMgr.SetWithProtocol("sess-stream", "b1", "m1", "client", "127.0.0.1", "test", "")
	p.sessionMgr.SetStreamActive("sess-stream", true)
	if cands := us.getUnloadCandidates(); len(cands) != 0 {
		t.Fatalf("у модели живой поток сессии, а она выставлена на выгрузку: %+v", cands)
	}
}

// TestR503_UnloadKeepsModelInCandidateGroup — X-2 (проверка через селектор):
// после обработки простоя модель остаётся в группе P1.
func TestR503_UnloadKeepsModelInCandidateGroup(t *testing.T) {
	p := availProxyR493(t, map[string]int{"b1": 4})
	setMetricsR493(p, "b1", []string{"m1"}, 20000) // модель загружена на бэкенде
	us := NewUnloadScheduler(p)
	us.enabled = true
	us.idleTimeout = time.Minute
	us.lastUsed["m1@b1"] = time.Now().Add(-10 * time.Minute)
	us.unloadModel("m1", "b1")

	p.backends["b1"].mu.Lock()
	_, isWarming := p.backends["b1"].WarmingUpModels["m1"]
	p.backends["b1"].mu.Unlock()
	if isWarming {
		t.Fatal("обработка простоя поставила метку WarmingUpModels: селектор уведёт " +
			"запрос в режим ожидания загрузки (аудит X-2)")
	}

	groups := p.expandCandidates("m1", []types.BackendType{types.BackendTypeLlamaCpp})
	for _, g := range groups {
		if g.Priority != 1 {
			continue
		}
		for _, id := range g.BackendIDs {
			if id == "b1" {
				return
			}
		}
	}
	t.Fatalf("загруженная модель выпала из группы P1 после обработки простоя: %+v", groups)
}
