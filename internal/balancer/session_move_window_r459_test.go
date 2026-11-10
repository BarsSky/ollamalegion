// session_move_window_r459_test.go — R459 (2026-11-06): «менее загруженный» не
// значит «подходит».
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, A10 ×2, Qwen3.8-27B, Cline/DSH). Живая сессия
// работала на прогретой копии с окном 65536 на слот. Вторая копия была поднята
// вручную с окном МЕНЬШЕ (32768) и оказалась менее загруженной — ребалансировка
// увела туда сессию, окно уменьшилось, агентский ход сломался посреди работы.
//
// Тесты фиксируют правило: переезд живого запроса запрещён, если кандидат даёт
// меньше окна (или модели на нём нет вовсе — переезд означает unload+load).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const r459Model = "Qwen3.8-27B"

// r459Proxy — два healthy бэкенда с моделью; b2 менее загружен (ActiveReqs=1).
func r459Proxy(fromPerSeq, candPerSeq int) *Proxy {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Status: types.StatusHealthy}},
			"b2": {Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	for id, perSeq := range map[string]int{"b1": fromPerSeq, "b2": candPerSeq} {
		if perSeq <= 0 {
			continue // модель не загружена — записи нет вовсе
		}
		p.metricsMgr.llamaMetrics[id] = &types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: r459Model, ContextLength: perSeq}},
		}
		p.metricsMgr.metrics[id] = &types.BackendMetrics{
			ID:       id,
			LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: r459Model}}},
		}
	}
	p.backends["b1"].ActiveReqs = 9
	p.backends["b2"].ActiveReqs = 1
	return p
}

// TestR459_MoveRejectedWhenCandidateWindowSmaller — ровно живой случай.
func TestR459_MoveRejectedWhenCandidateWindowSmaller(t *testing.T) {
	p := r459Proxy(65536, 32768)

	ok, why := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 0)
	if ok {
		t.Fatal("переезд на копию с окном 32768 при работающих 65536 обязан быть запрещён")
	}
	if why == "" {
		t.Fatal("причина отказа не названа — оператор не узнает, почему не переехали")
	}

	// Тот же проход отбора: b2 менее загружен, но отсечён предикатом.
	gate := p.sessionMoveGateR459("b1", r459Model, 0)
	if got := p.findLessLoadedBackendWithModelWhere(r459Model, "b1", nil, gate); got != "" {
		t.Fatalf("выбран %q, а окно кандидата меньше — переезд запрещён", got)
	}
	// Без предиката выбор прежний (поведение других путей не изменилось).
	if got := p.findLessLoadedBackendWithModel(r459Model, "b1", nil); got != "b2" {
		t.Fatalf("без предиката = %q, want b2 (прежнее поведение)", got)
	}
}

// TestR459_MoveAllowedWhenCandidateWindowCovers — равное или большее окно
// переезду не мешает.
func TestR459_MoveAllowedWhenCandidateWindowCovers(t *testing.T) {
	for _, cand := range []int{65536, 131072} {
		p := r459Proxy(65536, cand)
		if ok, why := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 0); !ok {
			t.Fatalf("кандидат с окном %d покрывает 65536, а переезд запрещён: %s", cand, why)
		}
	}
}

// TestR459_ClientWindowIsAuthoritative — клиент назвал окно: оно и есть
// требование, даже если у сессии сейчас меньше.
func TestR459_ClientWindowIsAuthoritative(t *testing.T) {
	p := r459Proxy(32768, 65536)

	if ok, _ := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 0); !ok {
		t.Fatal("кандидат с 65536 покрывает текущие 32768 — переезд разрешён")
	}
	if ok, _ := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 131072); ok {
		t.Fatal("клиент требует 131072, а у кандидата 65536 — переезд запрещён")
	}
}

// TestR459_ColdCandidateRejected — модели на кандидате нет: переезд означал бы
// unload+load посреди сессии. Живой класс: «свободный бэкенд» без готовой копии.
//
// R480 (P1-5): «нет модели» — это ФАКТ свежей метрики. Без отметки опроса
// (метрика устарела/не приходила) кандидат считается неизвестным и допускается,
// см. cold_candidate_ttl_r480_test.go.
func TestR459_ColdCandidateRejected(t *testing.T) {
	p := r459Proxy(65536, 0)
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now())

	if ok, _ := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 0); ok {
		t.Fatal("переезд на бэкенд без загруженной модели запрещён: это unload+load посреди хода")
	}
	gate := p.sessionMoveGateR459("b1", r459Model, 0)
	if got := p.findLessLoadedBackendAnyWhere(r459Model, "b1", nil, gate); got != "" {
		t.Fatalf("выбран холодный %q — переезд живого запроса запрещён", got)
	}
}

// TestR459_UnknownWindowsKeepOldBehaviour — окон не знаем: не выдумываем предел
// (прежнее поведение сохраняется).
func TestR459_UnknownWindowsKeepOldBehaviour(t *testing.T) {
	p := r459Proxy(0, 0)

	if ok, why := p.sessionMoveKeepsWindowR459("b2", "b1", r459Model, 0); !ok {
		t.Fatalf("окна неизвестны — ограничивать нечем, получено: %s", why)
	}
}
