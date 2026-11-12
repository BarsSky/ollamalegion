// window_align_r494_test.go — R494 (2026-11-12): предложение выровнять окна
// копий одной модели.
//
// ЖИВАЯ РАСКЛАДКА ИЗ АРХИВА (A10 ×2, Qwen3.8-27B):
//
//	мастер   — окно 198656, 1 живая сессия (и слот занят);
//	A10-126  — окно 131072, сессий нет, активных 0;
//	WIN-I9   — 8 ГБ, модели нет (в метриках отсутствует).
//
// Правило оператора: «если копия не занята сессией, то принимать решение по
// выравниванию; если же занята, а есть бэкенд с нужным окном — ставить в очередь
// к нему на обработку». Отсюда: предлагаем поднять ТОЛЬКО свободную копию;
// занятую не трогаем и предложения по ней не делаем (иначе это обрыв чужой
// генерации, а не выравнивание).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	alignModelR494  = "Qwen3.8-27B-UD-Q4_K_M"
	alignMasterR494 = "master"
	alignNarrowR494 = "a10-126"
)

// alignProxyR494 — два узла с одной моделью и разными окнами.
func alignProxyR494(t *testing.T, wide, narrow int) *Proxy {
	t.Helper()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: alignMasterR494, Name: "master", Host: "127.0.0.1", OllamaPort: 1,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: alignNarrowR494, Name: "a10-126", Host: "127.0.0.1", OllamaPort: 2,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, MetricsInterval: 60},
	}
	p := newProxyWithCleanup(t, conf)
	set := func(id string, win int) {
		lm := &types.LlamaCppMetrics{
			LoadedModels:    []types.LlamaCppModel{{Name: alignModelR494, State: "loaded", ContextLength: win, MaxSlots: 1}},
			VramKnown:       true,
			AvailableVRAMMB: 3000,
		}
		p.metricsMgr.mu.Lock()
		p.metricsMgr.llamaMetrics[id] = lm
		p.metricsMgr.metrics[id] = &types.BackendMetrics{ID: id, LlamaCpp: *lm}
		p.metricsMgr.mu.Unlock()
		p.metricsMgr.MarkLlamaCppPolled(id, time.Now())
	}
	set(alignMasterR494, wide)
	set(alignNarrowR494, narrow)
	return p
}

func alignCopyR494(id string, win, sess, active int) WindowAlignCopyR494 {
	return WindowAlignCopyR494{
		BackendID: id, Window: win, Sessions: sess, Active: active,
		Free: sess == 0 && active == 0,
	}
}

// TestR494_Decide_FreeNarrowCopyIsSuggested — главный случай архива.
func TestR494_Decide_FreeNarrowCopyIsSuggested(t *testing.T) {
	s, ok := windowAlignDecideR494(alignModelR494, []WindowAlignCopyR494{
		alignCopyR494(alignMasterR494, 198656, 1, 1), // занята сессией
		alignCopyR494(alignNarrowR494, 131072, 0, 0), // свободна
	}, 262144)
	if !ok {
		t.Fatal("предложение не построено: свободная узкая копия — ровно случай из архива")
	}
	if s.TargetWindow != 198656 {
		t.Fatalf("TargetWindow = %d, ожидалось 198656 (окно самой широкой копии)", s.TargetWindow)
	}
	if len(s.Alignable) != 1 || s.Alignable[0] != alignNarrowR494 {
		t.Fatalf("alignable = %v, ожидался только свободный %q", s.Alignable, alignNarrowR494)
	}
	if len(s.Busy) != 0 {
		t.Fatalf("busy = %v, ожидалось пусто: занятая копия шире цели и не предлагается", s.Busy)
	}
}

// TestR494_Decide_BusyNarrowCopyIsNotSuggested — правило оператора: занятую
// копию не выравниваем (это обрыв чужой генерации).
func TestR494_Decide_BusyNarrowCopyIsNotSuggested(t *testing.T) {
	if _, ok := windowAlignDecideR494(alignModelR494, []WindowAlignCopyR494{
		alignCopyR494(alignMasterR494, 198656, 1, 1),
		alignCopyR494(alignNarrowR494, 131072, 1, 1),
	}, 262144); ok {
		t.Fatal("предложение построено для занятой узкой копии: перезагрузка оборвала бы чужую сессию")
	}
}

// TestR494_Decide_BusyIsReportedBesideFree — узкая занятая копия попадает в
// busy (оператору видно, почему её нет в alignable), свободная — в alignable.
func TestR494_Decide_BusyIsReportedBesideFree(t *testing.T) {
	s, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("wide", 198656, 0, 0),
		alignCopyR494("narrow-busy", 131072, 1, 1),
		alignCopyR494("narrow-free", 65536, 0, 0),
	}, 262144)
	if !ok {
		t.Fatal("предложение не построено")
	}
	if len(s.Alignable) != 1 || s.Alignable[0] != "narrow-free" {
		t.Fatalf("alignable = %v, ожидался [narrow-free]", s.Alignable)
	}
	if len(s.Busy) != 1 || s.Busy[0] != "narrow-busy" {
		t.Fatalf("busy = %v, ожидался [narrow-busy]", s.Busy)
	}
}

// TestR494_Decide_CapBoundsTarget — потолок оператора обязателен: без него
// предложение «поднять до 198656» уходило бы на узел, где столько не влезет.
func TestR494_Decide_CapBoundsTarget(t *testing.T) {
	s, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("wide", 198656, 0, 0),
		alignCopyR494("narrow", 65536, 0, 0),
	}, 131072)
	if !ok {
		t.Fatal("предложение не построено")
	}
	if s.TargetWindow != 131072 {
		t.Fatalf("TargetWindow = %d, ожидался потолок 131072", s.TargetWindow)
	}
}

// TestR494_Decide_CapBelowCopiesDisables — потолок ниже всех копий: выравнивать
// некуда, предложения нет (а не «предложим 131072» рядом с копией 198656).
func TestR494_Decide_CapBelowCopiesDisables(t *testing.T) {
	if _, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("wide", 198656, 0, 0),
		alignCopyR494("mid", 131072, 0, 0),
	}, 131072); ok {
		t.Fatal("предложение построено, хотя цель совпадает со всеми копиями — двигать нечего")
	}
}

// TestR494_Decide_ZeroCapDisables — оператор выключил выравнивание (0).
func TestR494_Decide_ZeroCapDisables(t *testing.T) {
	if _, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("wide", 198656, 0, 0),
		alignCopyR494("narrow", 65536, 0, 0),
	}, 0); ok {
		t.Fatal("при потолке 0 предложение обязано отсутствовать (настройка выключена)")
	}
}

// TestR494_Decide_EqualWindowsNeedNoAlign — копии уже выровнены.
func TestR494_Decide_EqualWindowsNeedNoAlign(t *testing.T) {
	if _, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("a", 131072, 0, 0),
		alignCopyR494("b", 131072, 0, 0),
	}, 262144); ok {
		t.Fatal("предложение построено при равных окнах: выравнивать нечего")
	}
}

// TestR494_Decide_UnknownWindowIsNotNarrow — «окно неизвестно» (0) не считается
// узкой копией: предлагать по незнакомому числу нельзя.
func TestR494_Decide_UnknownWindowIsNotNarrow(t *testing.T) {
	if _, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("wide", 198656, 0, 0),
		alignCopyR494("unknown", 0, 0, 0),
	}, 262144); ok {
		t.Fatal("копия с неизвестным окном принята за узкую — предложение соврёт")
	}
}

// TestR494_Decide_SingleCopy — одна копия: выравнивать не с чем.
func TestR494_Decide_SingleCopy(t *testing.T) {
	if _, ok := windowAlignDecideR494("m", []WindowAlignCopyR494{
		alignCopyR494("only", 131072, 0, 0),
	}, 262144); ok {
		t.Fatal("предложение построено для единственной копии")
	}
}

// TestR494_ProxySuggestion_UsesSessionsAndActive — интеграция: предложение
// считается по РЕАЛЬНЫМ сессиям и активным запросам, а не по догадке.
func TestR494_ProxySuggestion_UsesSessionsAndActive(t *testing.T) {
	p := alignProxyR494(t, 198656, 131072)
	p.sessionMgr.Set("sess-master", alignMasterR494, alignModelR494, "client", "127.0.0.1", "ua")
	p.backends[alignMasterR494].ActiveReqs = 1

	sugg := p.WindowAlignSuggestionsR494("")
	if len(sugg) != 1 {
		t.Fatalf("предложений = %d, ожидалось 1 (A10-126 свободен и уже мастера): %+v", len(sugg), sugg)
	}
	got := sugg[0]
	if got.Model != alignModelR494 {
		t.Fatalf("Model = %q, ожидалось %q", got.Model, alignModelR494)
	}
	if got.TargetWindow != 198656 {
		t.Fatalf("TargetWindow = %d, ожидалось 198656", got.TargetWindow)
	}
	if len(got.Alignable) != 1 || got.Alignable[0] != alignNarrowR494 {
		t.Fatalf("alignable = %v, ожидался [%s]", got.Alignable, alignNarrowR494)
	}
	// Узкая копия свободна — free=true, sessions=0, active=0.
	var narrow *WindowAlignCopyR494
	for i := range got.Copies {
		if got.Copies[i].BackendID == alignNarrowR494 {
			narrow = &got.Copies[i]
		}
	}
	if narrow == nil {
		t.Fatalf("в copies нет %q: %+v", alignNarrowR494, got.Copies)
	}
	if !narrow.Free || narrow.Sessions != 0 || narrow.Active != 0 {
		t.Fatalf("узкая копия описана как занятая: %+v", *narrow)
	}
}

// TestR494_ProxySuggestion_SessionOnNarrowCopyBlocksIt — если на узкой копии
// появилась сессия, предложение исчезает (правило «не занята сессией»).
func TestR494_ProxySuggestion_SessionOnNarrowCopyBlocksIt(t *testing.T) {
	p := alignProxyR494(t, 198656, 131072)
	p.sessionMgr.Set("sess-narrow", alignNarrowR494, alignModelR494, "client", "127.0.0.1", "ua")

	if sugg := p.WindowAlignSuggestionsR494(""); len(sugg) != 0 {
		t.Fatalf("предложение построено при сессии на узкой копии: %+v", sugg)
	}
}

// TestR494_ProxySuggestion_ModelFilter — фильтр по модели: чужая модель не
// подмешивается в предложение.
func TestR494_ProxySuggestion_ModelFilter(t *testing.T) {
	p := alignProxyR494(t, 198656, 131072)
	if sugg := p.WindowAlignSuggestionsR494("another-model"); len(sugg) != 0 {
		t.Fatalf("фильтр по модели не работает: %+v", sugg)
	}
}
