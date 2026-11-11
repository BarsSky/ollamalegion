// availability_first_r493_test.go — R493 (2026-11-11): доступность как предикат.
//
// ЖИВОЙ ДЕФЕКТ (архивы второго стенда): мастер A10 с загруженной 27B и 2.4 ГБ
// свободной VRAM был занят одной генерацией; рядом простаивал A10-126 с 20.5 ГБ
// свободных. Запрос встал в очередь к мастеру на 300 с, а в логах свободных
// узлов не было ни одной попытки поднять модель. Причина: занятость судили по
// коэффициенту ActiveReqs/MaxConcurrentReqs (у всех узлов max=1 → только 0/1), а
// привязка сессии действовала как замок.
//
// Тесты фиксируют четыре правила R493:
//  1. загруженный узел без свободного слота не выбирается (счёт идёт по
//     ЭФФЕКТИВНОЙ вместимости, а не по статическому лимиту конфига);
//  2. привязка сессии не действует, когда у бэкенда сессии нет слота, — и
//     освобождение привязки действительно подключено в proxy.go;
//  3. заявка уезжает на свободный узел под загрузку модели вместо очереди к
//     занятому — но только если автозагрузка разрешена;
//  4. «свободный под загрузку» не выбирается, если модель физически не влезает.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// availProxyR493 — прокси с healthy llama-бэкендами заданной ёмкости.
func availProxyR493(t *testing.T, slots map[string]int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:          &types.LoadBalancerConfig{},
		backends:        map[string]*BackendState{},
		metricsMgr:      NewMetricsManager(),
		sessionMgr:      NewSessionManager(),
		lbAutoLoadAsync: true, // как в бандле: LB_AUTO_LOAD_ASYNC=1
	}
	p.config.Balancing.Algorithm = types.AlgorithmRoundRobin
	for id, max := range slots {
		p.backends[id] = &BackendState{Backend: &types.Backend{
			ID: id, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
			MaxConcurrentReqs: max,
		}}
	}
	return p
}

// llamaMetricsR493 — метрики llama.cpp с известной свободной VRAM.
func llamaMetricsR493(loaded []string, freeMB uint64) *types.LlamaCppMetrics {
	models := make([]types.LlamaCppModel, 0, len(loaded))
	for _, name := range loaded {
		models = append(models, types.LlamaCppModel{Name: name, State: "loaded"})
	}
	return &types.LlamaCppMetrics{
		LoadedModels:    models,
		VramKnown:       true,
		AvailableVRAMMB: freeMB,
	}
}

// setGpuR493 — GPU/CPU-составляющая метрик бэкенда (её читает скоринг P4).
//
// Нужна, чтобы тест мог сделать ЗАНЯТЫЙ узел привлекательным по score, а
// свободный — непривлекательным: иначе положительный и отрицательный исходы
// теста неразличимы, потому что P4 сам по себе предпочитает свободную карту.
func setGpuR493(p *Proxy, id string, usage float64, memTotal, memFree uint64, cpu float64) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	bm := p.metricsMgr.metrics[id]
	if bm == nil {
		return
	}
	bm.GPU.UsagePercent = usage
	bm.GPU.MemoryTotal = memTotal
	bm.GPU.MemoryUsed = memTotal - memFree
	bm.GPU.MemoryFree = memFree
	bm.System.CPUUsagePercent = cpu
}

// setMetricsR493 — метрики бэкенда так, как их кладёт боевой путь агента:
// ЗАПИСЬ В metrics (её читает expandCandidates для групп P1..P4) и в llamaMetrics
// (оттуда берётся свободная VRAM и список загруженных моделей).
//
// Почему не хватает одного UpdateLlamaCppMetrics: expandCandidates считает бэкенд
// «без метрик» и отправляет его только в FALLBACK, если записи в metrics нет.
func setMetricsR493(p *Proxy, id string, loaded []string, freeMB uint64) {
	lm := llamaMetricsR493(loaded, freeMB)
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[id] = &types.BackendMetrics{ID: id, LlamaCpp: *lm}
	p.metricsMgr.llamaMetrics[id] = lm
	p.metricsMgr.mu.Unlock()
}

// TestR493_SaturatedBackendHasNoFreeSlot — предикат отвечает по слотам, а не по
// коэффициенту: при max=1 и одном активном запросе слота нет.
func TestR493_SaturatedBackendHasNoFreeSlot(t *testing.T) {
	p := availProxyR493(t, map[string]int{"master": 1, "free": 1})
	if !p.hasFreeSlotNowR493("free") {
		t.Fatal("свободный узел обязан быть доступен")
	}
	p.backends["master"].ActiveReqs = 1
	if p.hasFreeSlotNowR493("master") {
		t.Fatal("занятый узел (1/1) не должен считаться доступным — именно на этом заявка уходила в очередь")
	}
	p.backends["master"].ActiveReqs = 0
	if !p.hasFreeSlotNowR493("master") {
		t.Fatal("освободившийся узел снова доступен")
	}
}

// TestR493_StickyBackendNotPinnedWhenSaturated — привязка сессии не держит
// заявку на занятом бэкенде.
func TestR493_StickyBackendNotPinnedWhenSaturated(t *testing.T) {
	p := availProxyR493(t, map[string]int{"master": 1, "free": 1})
	p.sessionMgr.Set("sess-1", "master", "m", "client", "127.0.0.1", "ua")

	if got := p.stickyBackendIfAvailableR493("sess-1"); got != "master" {
		t.Fatalf("при свободном слоте привязка должна действовать, got %q", got)
	}
	p.backends["master"].ActiveReqs = 1
	if got := p.stickyBackendIfAvailableR493("sess-1"); got != "" {
		t.Fatalf("при занятом слоте привязка не должна замыкать заявку, got %q", got)
	}
}

// TestR493_P1UsesEffectiveCapacity — загруженный узел без свободного слота не
// выбирается на этапе P1 (Model Affinity).
//
// Раскладка специально «обманчивая»: статический лимит конфига 4, но узел заявил
// вместимость 1 (NodeClaimedCapacity). Коэффициент по СТАТИЧЕСКОМУ лимиту равен
// 0.25 и проходит порог 0.8, хотя слота нет; по ЭФФЕКТИВНОЙ вместимости (R71)
// он равен 1.0 и отсекается. Ровно на этом старый P1 отдавал заявку занятому узлу.
func TestR493_P1UsesEffectiveCapacity(t *testing.T) {
	p := availProxyR493(t, map[string]int{"master": 4, "free": 1})
	p.backends["master"].Backend.NodeClaimedCapacity = 1
	setMetricsR493(p, "master", []string{"m27b"}, 2471)
	setMetricsR493(p, "free", nil, 20490)
	setGpuR493(p, "master", 0, 23000, 20000, 0)
	setGpuR493(p, "free", 100, 23000, 0, 100)
	p.backends["master"].ActiveReqs = 1

	if got := p.backends["master"].Backend.EffectiveMaxConcurrentRequests(); got != 1 {
		t.Fatalf("условие теста не выполнено: эффективная вместимость мастера %d, нужна 1", got)
	}
	// Условие «обманки»: по статике коэффициент проходит порог, по эффективной — нет.
	if ratio := float64(1) / float64(p.backends["master"].Backend.MaxConcurrentReqs); ratio >= p.loadThresholdR479() {
		t.Fatalf("условие теста не выполнено: статический коэффициент %.2f не проходит порог", ratio)
	}
	if got := p.selectBackend("m27b", types.BackendTypeLlamaCpp); got == "master" {
		t.Fatal("P1 выбрал загруженный узел без свободного слота — заявка снова уйдёт в очередь")
	}
}

// TestR493_SelectorSkipsSaturatedLoadedBackend — ключевая проверка: занятый узел
// с загруженной моделью не выигрывает у свободного.
func TestR493_SelectorSkipsSaturatedLoadedBackend(t *testing.T) {
	p := scenarioR493(t)

	got := p.selectBackend("m27b", types.BackendTypeLlamaCpp)
	if got == "master" {
		t.Fatal("занятый узел с загруженной моделью выбран вместо свободного — заявка снова уйдёт в очередь")
	}
	if got != "free" {
		t.Fatalf("селектор обязан вернуть свободный узел, got %q", got)
	}
}

// scenarioR493 — живая раскладка второго стенда (2026-11-11) в миниатюре:
//
//	master: 27B загружена, 1 активный запрос при maxConcurrentReqs=1 (слота нет),
//	        но по GPU-показателям он ВЫГЛЯДИТ привлекательно (так и было: карта
//	        недогружена, потому что запрос ждал, а не считался);
//	free:   модель не загружена, 20490 МБ свободной VRAM, слот свободен.
//
// Такой расклад различает этапы: P4 по score выбрал бы master, а R493-этап 3b —
// свободный узел под загрузку. Без этой асимметрии тест «проходил» бы и без
// правки, потому что P4 сам предпочитает свободную карту.
func scenarioR493(t *testing.T) *Proxy {
	t.Helper()
	p := availProxyR493(t, map[string]int{"master": 1, "free": 1})
	setMetricsR493(p, "master", []string{"m27b"}, 2471)
	setMetricsR493(p, "free", nil, 20490)
	// master: 0% GPU, 20 ГБ «свободно» по GPU-счётчику → высокий score.
	setGpuR493(p, "master", 0, 23000, 20000, 0)
	// free: 100% GPU и занятая память по GPU-счётчику → низкий score.
	setGpuR493(p, "free", 100, 23000, 0, 100)
	p.backends["master"].ActiveReqs = 1
	return p
}

// TestR493_LoadableFreeBackendRespectsVram — «свободный под загрузку» отбрасывает
// узел, где модель не влезает: на A10-126 (20 ГБ свободных) модель 12 ГиБ влезает,
// на 8-гигабайтном узле (6 ГБ свободных) — нет, хотя слот у обоих свободен.
func TestR493_LoadableFreeBackendRespectsVram(t *testing.T) {
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == "m12b" {
			return int64(12) << 30 // 12 ГиБ: влезает в 20 ГБ, не влезает в 6 ГБ
		}
		return 0
	})
	defer restore()

	// Прогретая копия есть и занята (иначе «загрузка вместо очереди» неприменима —
	// см. TestR493_NoLoadedCopyNoFreeBackendHijack).
	p := availProxyR493(t, map[string]int{"master": 1, "small": 1, "big": 1})
	setMetricsR493(p, "master", []string{"m12b"}, 2471)
	p.backends["master"].ActiveReqs = 1
	setMetricsR493(p, "small", nil, 6000)
	setMetricsR493(p, "big", nil, 20490)

	got := p.pickLoadableFreeBackendR493("m12b", p.getAllowedTypesList(types.BackendTypeLlamaCpp))
	if got != "big" {
		t.Fatalf("под загрузку 12 ГиБ должен выбираться узел с 20 ГБ свободных, а не с 6 ГБ; got %q", got)
	}

	// Обратная сторона: модель, которая не влезает НИКУДА, не даёт бэкенда —
	// при этом прогретая копия на мастере остаётся (гейт открыт).
	restore2 := setModelSizeBytesForTest(func(name string) int64 {
		return int64(40) << 30
	})
	defer restore2()
	setMetricsR493(p, "master", []string{"m40b"}, 2471)
	if got := p.pickLoadableFreeBackendR493("m40b", p.getAllowedTypesList(types.BackendTypeLlamaCpp)); got != "" {
		t.Fatalf("модель 40 ГиБ не влезает ни в один узел, выбирать некого; got %q", got)
	}
}

// TestR493_LoadableFreeBackendSkipsSaturatedNode — свободных слотов нет ни у кого:
// выбирать нечего (дальше решает очередь, а не выдуманный бэкенд).
func TestR493_LoadableFreeBackendSkipsSaturatedNode(t *testing.T) {
	// Прогретая копия занята на мастере, а свободный узел один.
	p := availProxyR493(t, map[string]int{"master": 1, "only": 1})
	setMetricsR493(p, "master", []string{"m27b"}, 2471)
	setMetricsR493(p, "only", nil, 20490)
	p.backends["master"].ActiveReqs = 1
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "only" {
		t.Fatalf("контроль: свободный узел обязан выбираться, иначе тест ниже ничего не доказывает; got %q", got)
	}
	p.backends["only"].ActiveReqs = 1
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "" {
		t.Fatalf("при занятых узлах выбирать нечего, got %q", got)
	}
}

// TestR493_NoLoadedCopyNoFreeBackendHijack — этап «загрузка вместо очереди» НЕ
// работает, когда прогретой копии нет ни на одном узле.
//
// Живой регресс этого правила (пойман на CI): TestReplication_Scenario_
// LiveRequestReplicated — два узла БЕЗ метрик, 10 запросов; ранняя версия R493
// выбирала «свободный узел под загрузку» по score, при равных score всегда
// выигрывал первый из map, и round-robin переставал действовать: один узел
// получал 10 запросов, второй 0. Модель не загружена нигде → решает обычный
// алгоритм, а не этот этап.
func TestR493_NoLoadedCopyNoFreeBackendHijack(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if p.modelLoadedSomewhereR493("m27b", types1) {
		t.Fatal("условие теста не выполнено: модель не должна считаться загруженной")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "" {
		t.Fatalf("без прогретой копии выбирать «узел под загрузку» нельзя, got %q", got)
	}

	// Контроль: как только модель появилась в памяти (пусть и на занятом узле),
	// этап снова имеет право выбирать.
	setMetricsR493(p, "a", []string{"m27b"}, 2471)
	p.backends["a"].ActiveReqs = 1
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "b" {
		t.Fatalf("при занятой прогретой копии должен выбираться свободный узел, got %q", got)
	}
}

// TestR493_NoAutoloadNoFreeNodeLoad — запрет автозагрузки соблюдается: если
// оператор выключил и async-автозагрузку, и SyncModelLoad, свободный узел НЕ
// получает заявку «под загрузку модели» — селектор уходит в очередь (P4).
//
// Без этого гейта R493 сам стал бы обходом выключателя: модель поднималась бы на
// свободном узле вопреки настройке.
func TestR493_NoAutoloadNoFreeNodeLoad(t *testing.T) {
	p := scenarioR493(t)
	p.lbAutoLoadAsync = false
	p.config.Balancing.SyncModelLoad.Enabled = false

	// Контроль: сама функция выбора от флага не зависит — гейт стоит на этапе 3b.
	if got := p.pickLoadableFreeBackendR493("m27b", p.getAllowedTypesList(types.BackendTypeLlamaCpp)); got != "free" {
		t.Fatalf("контроль: узел под загрузку должен находиться, got %q", got)
	}
	// При закрытом гейте решает P4 (score) — и это НЕ свободный узел.
	if got := p.selectBackend("m27b", types.BackendTypeLlamaCpp); got == "free" {
		t.Fatalf("при выключенной автозагрузке свободный узел не должен выбираться под загрузку модели, got %q", got)
	}

	// Включаем async-автозагрузку — та же раскладка отдаёт свободный узел.
	p.lbAutoLoadAsync = true
	if got := p.selectBackend("m27b", types.BackendTypeLlamaCpp); got != "free" {
		t.Fatalf("при включённой автозагрузке заявка должна уходить на свободный узел, got %q", got)
	}
}

// TestR493_StickyReleaseWiredInProxy — ПОДКЛЮЧЕНИЕ, а не только предикат.
//
// Юнит-тест предиката не ловит возврат к прежнему поведению в proxy.go: там
// привязка сессии раньше безусловно уезжала в ожидание слота. Проверяем по
// исходнику (тот же приём, что в TestR378_Steward_IdentitySourcesLiveHere):
// единственный источник бэкенда сессии для очереди — хелпер R493, а прямой
// GetSnapshot рядом с ним — регресс.
func TestR493_StickyReleaseWiredInProxy(t *testing.T) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать proxy.go: %v", err)
	}

	calls := 0
	direct := 0
	ast.Inspect(af, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "stickyBackendIfAvailableR493":
				calls++
			case "GetSnapshot":
				direct++
			}
		}
		return true
	})

	if calls != 1 {
		t.Fatalf("в proxy.go ожидается ровно один вызов stickyBackendIfAvailableR493, найдено %d: "+
			"привязка сессии снова может стать замком, и заявка будет ждать занятый узел", calls)
	}
	if direct != 0 {
		t.Fatalf("в proxy.go найден прямой GetSnapshot (%d): бэкенд сессии снова берётся без проверки "+
			"свободного слота", direct)
	}
}
