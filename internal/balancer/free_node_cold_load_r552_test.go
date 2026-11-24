package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R552 (F8 плана docs/plans/2026-11-19-webui-and-balancer-fixes.md §2.8).
//
// ЖИВАЯ ЖАЛОБА: «балансер не грузит модель на свободный узел, а ждёт».
//
// Механизм «загрузка вместо очереди» (этап 3b) существовал, но был закрыт гейтом
// `modelLoadedSomewhereR493`: он требовал ПРОГРЕТУЮ копию в кластере. Типичный
// стенд после рестарта или выгрузки: в VRAM никого, ФАЙЛ модели на диске есть,
// свободный узел есть — а балансер загрузку не начинал и ставил клиента в очередь
// ждать копию, которую никто не создаст.
//
// Почему гейт нельзя было просто снять: тогда 3b «уводил» бы заявки при любом
// запросе (живой регресс R493: два узла без метрик, round-robin перестал
// действовать). Поэтому условие расширено ровно на ФАКТ из каталога (R93/R514):
// «известно и файл есть». Каталог не опрошен — поведение прежнее.

// catalogStoreR552 — записать снимок каталога узла.
func catalogStoreR552(p *Proxy, backendID string, files []string, dirErr string) {
	if p.modelCatalog == nil {
		if p.modelCatalog == nil { p.modelCatalog = newModelCatalog() } else { p.modelCatalog.reset() }
	}
	p.modelCatalog.store(backendID, files, dirErr, time.Now())
}

// TestR552_ColdModelOnDiskLoadsOnFreeNode — в VRAM никого, файл на диске
// свободного узла есть → узел выбран ПОД ЗАГРУЗКУ (было "" → очередь).
func TestR552_ColdModelOnDiskLoadsOnFreeNode(t *testing.T) {
	p := availProxyR493(t, map[string]int{"busy": 1, "free": 1})
	// Ни одной загруженной модели: стенд после рестарта/выгрузки.
	setMetricsR493(p, "busy", nil, 20490)
	setMetricsR493(p, "free", nil, 20490)
	p.backends["busy"].ActiveReqs = 1 // занят, слота нет
	// Каталог: файл модели лежит на свободном узле.
	catalogStoreR552(p, "free", []string{"m27b.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if p.modelLoadedSomewhereR493("m27b", types1) {
		t.Fatal("условие теста не выполнено: модели не должно быть в VRAM")
	}
	if !p.modelHasOnDiskSomewhereR493("m27b", types1) {
		t.Fatal("каталог сообщает файл на диске — предикат обязан это видеть")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "free" {
		t.Fatalf("при пустой VRAM и файле на диске загрузка должна идти на свободный узел, got %q — "+
			"это и есть жалоба «не грузит, а ждёт»", got)
	}
}

// TestR552_FileAbsentEverywhereNoHijack — каталог опрошен, файла нет ни на одном
// узле: выбор НЕ подменяется (иначе получили бы 503 `model_not_found` вместо
// честного распределения).
func TestR552_FileAbsentEverywhereNoHijack(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", []string{"other-model.gguf"}, "")
	catalogStoreR552(p, "b", []string{"other-model.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if p.modelHasOnDiskSomewhereR493("m27b", types1) {
		t.Fatal("файла нет ни у кого — предикат обязан ответить false")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "" {
		t.Fatalf("без файла на диске этап 3b не должен выбирать узел, got %q", got)
	}
}

// TestR552_CatalogUnknownKeepsOldBehaviour — каталог не опрошен: поведение
// прежнее (защита от живого регресса R493 с round-robin).
func TestR552_CatalogUnknownKeepsOldBehaviour(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if p.modelHasOnDiskSomewhereR493("m27b", types1) {
		t.Fatal("каталог не опрошен — «наверное, файл есть» не считается фактом")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "" {
		t.Fatalf("при непрочитанном каталоге этап 3b не должен подменять распределение, got %q", got)
	}
}

// TestR552_DirErrorIsNotAFile — чтение каталога упало: это не «файл есть» и не
// «файла нет», поэтому подмены выбора быть не должно.
func TestR552_DirErrorIsNotAFile(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", nil, "permission denied")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if p.modelHasOnDiskSomewhereR493("m27b", types1) {
		t.Fatal("ошибка чтения каталога не является доказательством наличия файла")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "" {
		t.Fatalf("при ошибке каталога выбор подменять нельзя, got %q", got)
	}
}

// TestR552_WarmCopyOnBusyNodeStillPrefersFreeNode — прежнее поведение сохранено:
// прогретая копия занятого узла + файл на свободном → свободный узел грузит.
func TestR552_WarmCopyOnBusyNodeStillPrefersFreeNode(t *testing.T) {
	p := scenarioR493(t) // master занят с прогретой копией, free свободен
	catalogStoreR552(p, "free", []string{"m27b.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if got := p.pickLoadableFreeBackendR493("m27b", types1); got != "free" {
		t.Fatalf("regress: при занятой прогретой копии должен выбираться свободный узел, got %q", got)
	}
}

// TestR552_SelectBackendColdLoadNotQueue — конец цепочки: на стенде без прогретой
// копии, но с файлом на диске, селектор отдаёт узел, а не пустоту (пустота = уход
// в очередь ожидания модели).
//
// ВАЖНОЕ ИЗМЕРЕНИЕ. Первая версия этого теста шла с включённым `SyncModelLoad` и
// заняла **120 секунд**: селектор сначала заходит в P3 (синхронное ожидание
// прогрева), и только потом доходит до этапа 3b. То есть «загрузка на свободный
// узел» работает, но клиент ждёт 120 с на пути, где ничего не греется — это
// ВТОРАЯ половина F8 (§2.8 «порядок этапов»), она вынесена в отдельный шаг и в
// этом тесте P3 выключен, чтобы проверять именно маршрут.
func TestR552_SelectBackendColdLoadNotQueue(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 2})
	setMetricsR493(p, "only", nil, 20490)
	catalogStoreR552(p, "only", []string{"m27b.gguf"}, "")
	p.config.Balancing.SyncModelLoad.Enabled = false // P3 выключен: см. измерение выше
	p.lbAutoLoadAsync = true

	got := p.selectBackend("m27b", types.BackendTypeLlamaCpp)
	if got != "only" {
		t.Fatalf("cold-load: селектор вернул %q, ожидался узел с файлом на диске — "+
			"пустой ответ означает постановку в очередь к несуществующей копии", got)
	}
}
