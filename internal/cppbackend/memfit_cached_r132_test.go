//go:build llama_stub

// memfit_cached_r132_test.go — R132 (2026-10-16): справочные ручки не ходят к узлам
// раскладки НИ ОДНИМ вызовом.
//
// ЧТО ОТКРЫЛОСЬ ПОСЛЕ R131. R131 убрал RPC из двух вызовов `handleListModels`
// (vram_known и per-model лимиты), но в ТОЙ ЖЕ ручке остался третий:
//
//	layout := backend.LayoutBreakdown()   // -> MemfitBudget() + RPCCapacityBytes()
//
// то есть `GET /api/models` продолжал дозваниваться до узла раскладки, и на узле,
// который принимает TCP и молчит (дублёр: контейнер-слушатель в сети стека),
// чтение списка моделей уходило в C-мост. Плюс отдельная справочная ручка
// `GET /api/models/effective` звала `MemfitBudget()` напрямую.
//
// ЖИВОЙ ДУБЛЁР МЁРТВОГО УЗЛА (замер изнутри ol-stack-cppworker-gpu):
//
//	опубликованный порт без слушателя : принимает и ЗАКРЫВАЕТ (12-18 мс)
//	контейнер-слушатель в ol-net      : принимает и МОЛЧИТ (5 с и больше)
//	хост донора выключен              : SYN дропается, таймаут 9 с
//
// Молчащий случай гейт R130 пропускает (тишина = «ждёт хэндшейка» = живой узел),
// поэтому от него защищает только одно: не дозваниваться из справочных путей.
package cppbackend

import (
	"os"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// readSource — исходник файла репозитория (путь от internal/cppbackend).
func readSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// dialingCalls — вызовы, которые могут дозвониться до узлов раскладки. Список
// ЯВНЫЙ: если появится новый способ спросить узлы, его нужно добавить сюда и в
// тест — иначе справочная ручка сможет снова уйти в C-мост незаметно.
//
// Суффиксные варианты (MemfitBudgetCached, LayoutBreakdownCached,
// feasibilityFromMemfitCached) сюда НЕ входят: скобка стоит после «Cached».
var dialingCalls = []string{
	"backend.MemfitBudget()",
	"backend.MemfitBudgetFor(",
	"backend.CalculateResourceLimits(",
	"backend.LayoutBreakdown()",
	"backend.RPCCapacityBytes(",
	"feasibilityFromMemfit(",
	"CheckRPCServers(",
	"ProbeRPCServers(",
	"MeasureLinkSpeed(",
}

// funcBody — исходник одной функции верхнего уровня (до следующей «\nfunc »).
func funcBody(t *testing.T, path, name string) string {
	t.Helper()
	src := readSource(t, path)
	marker := "func " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("в %s нет функции %s — тест устарел", path, name)
	}
	rest := src[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

// TestR132_ReadOnlyHandlersDoNotDialNodes — защита КЛАССА, а не двух мест:
// тела всех справочных ручек проверяются на любой из известных вызовов с
// дозвоном. Первая версия правки (R132) закрыла только вызов в самом обработчике,
// а дозвон оставался в общем помощнике feasibilityFromMemfit — живая проверка
// поймала это на стенде (`GET /api/models/effective` не отвечал 30 с).
func TestR132_ReadOnlyHandlersDoNotDialNodes(t *testing.T) {
	handlers := []struct{ path, fn string }{
		{"../../cmd/cppworker/handlers_model.go", "handleListModels"},
		{"../../cmd/cppworker/handlers_model.go", "handleGetModel"},
		{"../../cmd/cppworker/handlers_effective_r116.go", "handleModelEffective"},
	}
	for _, h := range handlers {
		body := funcBody(t, h.path, h.fn)
		for _, call := range dialingCalls {
			if strings.Contains(body, call) {
				t.Errorf("%s (%s) содержит %s — справочная ручка дозванивается до узла "+
					"раскладки и на молчащем узле вешает/роняет мастер",
					h.fn, h.path, call)
			}
		}
	}
}

// TestR132_DecisionPathsStillDial — обратная сторона: пути РЕШЕНИЯ (гейт n_ctx
// перед загрузкой, планировщик) обязаны по-прежнему учитывать ёмкость узлов.
// Если этот тест упадёт, значит «безопасный» вариант просочился в решение, и
// раскладка молча потеряет донора (живой дефект S1).
func TestR132_DecisionPathsStillDial(t *testing.T) {
	gate := readSource(t, "../../cmd/cppworker/nctx_feasibility.go")
	if !strings.Contains(gate, "feasibilityFromMemfit(") {
		t.Error("гейт n_ctx перестал звать вердикт с опросом узлов (feasibilityFromMemfit)")
	}
	loader := readSource(t, "../../cmd/cppworker/adaptive_loader.go")
	if !strings.Contains(loader, "RPCCapacityBytes(") {
		t.Error("адаптивный загрузчик перестал спрашивать ёмкость узлов")
	}
}

// TestR132_ListHandlerDoesNotDialNodes — ГЛАВНОЕ: в справочной ручке списка
// моделей не должно остаться НИ ОДНОГО вызова, который ходит к узлам.
func TestR132_ListHandlerDoesNotDialNodes(t *testing.T) {
	src := readSource(t, "../../cmd/cppworker/handlers_model.go")
	for _, bad := range []string{
		"backend.LayoutBreakdown()",
		"backend.MemfitBudget()",
		"backend.CalculateResourceLimits(",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("handleListModels всё ещё зовёт %s — GET /api/models дозванивается "+
				"до узла раскладки и на молчащем узле уходит в C-мост", bad)
		}
	}
	if !strings.Contains(src, "LayoutBreakdownCached()") {
		t.Error("handleListModels обязан брать раскладку через LayoutBreakdownCached()")
	}
}

// TestR132_EffectiveHandlerDoesNotDialNodes — вторая справочная ручка:
// GET /api/models/effective считает вердикт памяти и звала MemfitBudget() (с RPC).
func TestR132_EffectiveHandlerDoesNotDialNodes(t *testing.T) {
	src := readSource(t, "../../cmd/cppworker/handlers_effective_r116.go")
	if strings.Contains(src, "backend.MemfitBudget()") {
		t.Error("handleModelEffective всё ещё зовёт MemfitBudget() (с RPC) — " +
			"открытие карточки модели могло уронить мастер")
	}
	if !strings.Contains(src, "MemfitBudgetCached()") {
		t.Error("handleModelEffective обязан считать бюджет через MemfitBudgetCached()")
	}
}

// TestR132_CachedDeviceCountNeverDials — счётчик узлов для «раскладки решения» из
// кэша: справочная ручка показывает, сколько узлов учтено, не опрашивая их.
func TestR132_CachedDeviceCountNeverDials(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"probe-silent-rpc:50059"}}}
	b.rpcDevices = []bridge.DeviceInfo{remoteDevice()}
	b.rpcDevAt = time.Now()

	if n := b.CachedRemoteDeviceCount(); n != 1 {
		t.Errorf("CachedRemoteDeviceCount=%d, ожидалось 1 (узел из свежего снимка)", n)
	}
	if *calls != 0 {
		t.Fatalf("CachedRemoteDeviceCount дозвонился до узлов %d раз", *calls)
	}

	b.rpcDevAt = time.Now().Add(-2 * rpcDeviceTTL)
	if n := b.CachedRemoteDeviceCount(); n != 0 {
		t.Errorf("CachedRemoteDeviceCount=%d на протухшем снимке, ожидалось 0", n)
	}
	if *calls != 0 {
		t.Fatalf("разбор протухшего снимка дозвонился до узлов %d раз", *calls)
	}
}

// remoteDevice — удалённое устройство раскладки в снимке (4 ГиБ свободно из 6).
func remoteDevice() bridge.DeviceInfo {
	return bridge.DeviceInfo{
		Name:        "RPC0",
		IsRemote:    true,
		MemoryFree:  4 << 30,
		MemoryTotal: 6 << 30,
	}
}

// TestR132_CachedPathSeesSnapshotWithoutDialing — обе половины свойства сразу:
// вклад узлов ВИДЕН (значит кэш реально используется, а не игнорируется) и при
// этом НИ ОДНОГО обращения к узлам не сделано (значит мастер не может упасть).
func TestR132_CachedPathSeesSnapshotWithoutDialing(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"probe-silent-rpc:50059"}}}
	// Путь загрузки уже собрал снимок (или это сделал предыдущий расчёт).
	b.rpcDevices = []bridge.DeviceInfo{remoteDevice()}
	b.rpcDevAt = time.Now()

	local := b.MemfitBudgetLocal()
	cached := b.MemfitBudgetCached()

	if *calls != 0 {
		t.Fatalf("справочный расчёт дозвонился до узлов %d раз — именно это уходит в "+
			"C-мост и роняет мастер на молчащем узле", *calls)
	}
	if got, want := uint64(cached.VRAMFree), uint64(local.VRAMFree)+(4<<30); got != want {
		t.Errorf("свежий снимок узла не учтён: VRAMFree=%d, ожидалось %d (локально %d + 4 ГиБ узла)",
			got, want, uint64(local.VRAMFree))
	}
	if !strings.Contains(cached.Source, "+rpc-cached") {
		t.Errorf("источник бюджета %q не помечен как кэш узлов — оператор не отличит "+
			"измеренную ёмкость узла от её отсутствия", cached.Source)
	}

	layout := b.LayoutBreakdownCached()
	if *calls != 0 {
		t.Fatalf("LayoutBreakdownCached дозвонился до узлов %d раз", *calls)
	}
	if layout.Nodes != 1 || layout.RemoteFree != 4<<30 {
		t.Errorf("раскладка не увидела узел из снимка: nodes=%d remoteFree=%d",
			layout.Nodes, layout.RemoteFree)
	}
	if layout.RemoteSource != RemoteSourceCache {
		t.Errorf("источник вклада узлов %q, ожидался %q", layout.RemoteSource, RemoteSourceCache)
	}
	if layout.LayoutFree != layout.LocalFree+layout.RemoteFree {
		t.Errorf("итог раскладки %d != локально %d + узлы %d",
			layout.LayoutFree, layout.LocalFree, layout.RemoteFree)
	}

	_ = b.CalculateResourceLimitsCached("m")
	if *calls != 0 {
		t.Fatalf("CalculateResourceLimitsCached дозвонился до узлов %d раз", *calls)
	}
}

// TestR132_StaleSnapshotIsNotPresentedAsCurrent — протухший снимок не выдаётся за
// сегодняшнюю ёмкость: иначе оператор и балансер видели бы гигабайты донора,
// которого уже нет.
func TestR132_StaleSnapshotIsNotPresentedAsCurrent(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"probe-silent-rpc:50059"}}}
	b.rpcDevices = []bridge.DeviceInfo{remoteDevice()}
	b.rpcDevAt = time.Now().Add(-2 * rpcDeviceTTL)

	local := b.MemfitBudgetLocal()
	cached := b.MemfitBudgetCached()
	if uint64(cached.VRAMFree) != uint64(local.VRAMFree) {
		t.Errorf("протухший снимок попал в бюджет: %d против локальных %d",
			uint64(cached.VRAMFree), uint64(local.VRAMFree))
	}
	if strings.Contains(cached.Source, "+rpc-cached") {
		t.Errorf("источник %q помечен кэшем узлов, хотя снимок протух", cached.Source)
	}
	layout := b.LayoutBreakdownCached()
	if layout.Nodes != 0 || layout.RemoteSource != RemoteSourceNone {
		t.Errorf("протухший снимок показан как текущий: nodes=%d source=%q",
			layout.Nodes, layout.RemoteSource)
	}
	if *calls != 0 {
		t.Fatalf("разбор протухшего снимка дозвонился до узлов %d раз", *calls)
	}
}

// TestR132_DialingVariantIsDistinct — варианты не должны схлопнуться: путь,
// которому ёмкость узлов нужна как основание решения, по-прежнему её спрашивает.
// Если этот тест упадёт, значит «безопасный» вариант стал повсюду, и планировщик
// молча потеряет ёмкость донора (отдельный живой дефект S1).
func TestR132_DialingVariantIsDistinct(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"probe-silent-rpc:50059"}}}

	_ = b.LayoutBreakdown()
	if *calls == 0 {
		t.Error("LayoutBreakdown (путь решения) перестал спрашивать узлы")
	}
}
