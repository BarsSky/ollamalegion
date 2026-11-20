// cold_load_probe_r663_test.go — R663 (2026-11-20): живая проверка кандидата
// перед «загрузкой вместо ожидания».
//
// ЧТО ПРОВЕРЯЕТСЯ (всё — из живого двухузлового прогона 2026-11-20):
//  1. узел УЖЕ держит модель — повторный POST /load не отправляется (кэш метрик
//     отстаёт на 30 с, и без пробы балансер просил загрузку второй раз);
//  2. узел ГРУЗИТ модель — то же;
//  3. узел — ДОНОР раскладки (`can_load_own_models=false`): он ответит 409, значит
//     его нельзя выбирать (на стенде это был реальный блокер: R659 выбрал узел,
//     а загрузка была отклонена);
//  4. живой ответ кэшируется: цикл ожидания не долбит воркер запросами;
//  5. неудавшаяся попытка повторяется через короткую паузу, а не через полный TTL;
//  6. недоступная проба не меняет поведение: решение остаётся за кэшем метрик.
package balancer

import (
	"net/http"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r663Proxy — мастер с занятым слотом + свободный узел-кандидат (как на стенде).
func r663Proxy(t *testing.T, worker *r659Worker) *Proxy {
	t.Helper()
	now := time.Now()
	p, _ := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)},
		},
		r659Node{id: "a10", port: portOfR659(t, worker), slots: 1, active: 0, totalMB: 23028, freeMB: 20490},
	)
	p.metricsMgr.MarkLlamaCppPolled("a10", now)
	return p
}

// TestR663_NoLoadWhenNodeAlreadyHasModel — кэш метрик отстал, узел уже с моделью.
func TestR663_NoLoadWhenNodeAlreadyHasModel(t *testing.T) {
	worker := newR659Worker(t)
	worker.setModelsResponse("loaded", nil, 0) // живой ответ: модель загружена
	p := r663Proxy(t, worker)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatalf("узел уже держит модель — выбирать его под загрузку нельзя (выбран %q)", plan.BackendID)
	}
	if got := r659NoLoad(t, worker); len(got) != 0 {
		t.Fatalf("POST /load отправлен узлу, где модель уже загружена: %v", got)
	}
}

// TestR663_NoLoadWhenNodeIsLoadingModel — модель грузится: просить нечего.
func TestR663_NoLoadWhenNodeIsLoadingModel(t *testing.T) {
	worker := newR659Worker(t)
	worker.setModelsResponse("loading", nil, 0)
	p := r663Proxy(t, worker)

	if _, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatal("узел уже грузит модель — повторная загрузка не нужна")
	}
	if got := r659NoLoad(t, worker); len(got) != 0 {
		t.Fatalf("POST /load при уже идущей загрузке: %v", got)
	}
}

// TestR663_DonorNodeNotChosen — донор раскладки собственных копий не держит (409).
func TestR663_DonorNodeNotChosen(t *testing.T) {
	worker := newR659Worker(t)
	canLoadOwn := false
	worker.setModelsResponse("", &canLoadOwn, 0)
	p := r663Proxy(t, worker)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatalf("донор раскладки отказал бы в загрузке (409), а выбран %q", plan.BackendID)
	}
	if got := r659NoLoad(t, worker); len(got) != 0 {
		t.Fatalf("POST /load отправлен узлу-донору: %v", got)
	}
}

// TestR663_ProbeCached — живой ответ кэшируется: одна проба на несколько решений.
func TestR663_ProbeCached(t *testing.T) {
	worker := newR659Worker(t)
	p := r663Proxy(t, worker)
	// ДЕЛЬТА: в /api/models ходит ещё и поллер метрик (раз в 30 с) — его запрос к
	// кэшу нашей пробы отношения не имеет.
	before := worker.modelsProbes()

	for i := 0; i < 3; i++ {
		if _, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
			t.Fatalf("попытка %d: холодный узел обязан выбираться", i+1)
		}
	}
	if got := worker.modelsProbes() - before; got != 1 {
		t.Fatalf("проб за 3 решения: %d, ожидалась 1 (кэш coldLoadProbeCacheTTL)", got)
	}
}

// TestR663_FailedAttemptRetriedAfterShortPause — попытка не подхватилась: повтор
// через coldLoadRetryAfterFailedR659, а не через полный TTL.
func TestR663_FailedAttemptRetriedAfterShortPause(t *testing.T) {
	worker := newR659Worker(t) // живой ответ: модели нет и она не грузится
	p := r663Proxy(t, worker)

	// Попытка 5 с назад: ждём короткую паузу.
	p.coldLoadStateR659.mu.Lock()
	p.coldLoadStateR659.started = map[string]time.Time{"a10\x00" + r659Model: time.Now().Add(-5 * time.Second)}
	p.coldLoadStateR659.mu.Unlock()
	if _, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatal("через 5 с после попытки повторять загрузку рано (это дедупликация)")
	}

	// Попытка 25 с назад: загрузка не подхватилась — повторяем.
	p.coldLoadStateR659.mu.Lock()
	p.coldLoadStateR659.started = map[string]time.Time{"a10\x00" + r659Model: time.Now().Add(-25 * time.Second)}
	p.coldLoadStateR659.mu.Unlock()
	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
		t.Fatal("попытка не подхватилась (модели нет, не грузится) — повтор обязан быть разрешён")
	} else if plan.BackendID != "a10" {
		t.Fatalf("выбран %q, ожидался a10", plan.BackendID)
	}
}

// TestR663_ProbeUnavailableFallsBackToMetrics — проба недоступна: поведение прежнее
// (решает кэш метрик), живая проверка не становится источником отказов.
func TestR663_ProbeUnavailableFallsBackToMetrics(t *testing.T) {
	worker := newR659Worker(t)
	worker.setModelsResponse("", nil, http.StatusInternalServerError)
	p := r663Proxy(t, worker)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
		t.Fatal("при недоступной пробе холодный по метрикам узел обязан остаться кандидатом")
	} else if plan.BackendID != "a10" {
		t.Fatalf("выбран %q, ожидался a10", plan.BackendID)
	}
}

// TestR663_ProbeReportsAbsentForFreshNode — нормальный ответ «модели нет» не мешает.
func TestR663_ProbeReportsAbsentForFreshNode(t *testing.T) {
	worker := newR659Worker(t)
	canLoadOwn := true
	worker.setModelsResponse("", &canLoadOwn, 0)
	p := r663Proxy(t, worker)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
		t.Fatal("свободный узел с разрешёнными собственными копиями обязан выбираться")
	} else if plan.BackendID != "a10" {
		t.Fatalf("выбран %q, ожидался a10", plan.BackendID)
	}
}
