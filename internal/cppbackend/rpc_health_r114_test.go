//go:build llama_stub

// rpc_health_r114_test.go — R114 (2026-10-13): гейт и watchdog узлов раскладки.
//
// ЖИВОЙ ФАКТ, ради которого это появилось (docs/layer-sharding.md §4.6):
// пересоздание узла `rpc-server` под живым соединением мастера роняет мастера —
// `SIGABRT: abort` + `signal arrived during cgo execution` внутри
// `bridge_load_model`, контейнер перезапускается, модель теряется. Обработать это
// в llama.cpp нельзя; можно не доводить до него:
//
//   - гейт перед загрузкой — узел недоступен → понятная ошибка вместо abort;
//   - watchdog у загруженной модели — узел пропал → модель выгружается в простое,
//     и следующий запрос грузит её заново (гейт снова проверит узлы).
package cppbackend

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// listenerR114 — живой TCP-порт, который можно закрыть.
func listenerR114(t *testing.T) (string, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось занять порт: %v", err)
	}
	return ln.Addr().String(), ln
}

// TestR114_CheckRPCServers_EmptyIsOK — раскладка не настроена: гейт не мешает.
func TestR114_CheckRPCServers_EmptyIsOK(t *testing.T) {
	for _, servers := range [][]string{nil, {}, {"", "   "}} {
		if err := CheckRPCServers(servers, 50*time.Millisecond); err != nil {
			t.Errorf("пустой список узлов дал ошибку: %v", err)
		}
	}
}

// TestR114_CheckRPCServers_ReachablePasses — живой порт проходит гейт.
func TestR114_CheckRPCServers_ReachablePasses(t *testing.T) {
	addr, ln := listenerR114(t)
	defer ln.Close()

	if err := CheckRPCServers([]string{addr}, 200*time.Millisecond); err != nil {
		t.Fatalf("живой узел %s не прошёл гейт: %v", addr, err)
	}
}

// TestR114_CheckRPCServers_UnreachableIsTyped — недоступный узел: типизированная
// ошибка с адресом и объяснением про abort (оператор должен понять, что делать).
func TestR114_CheckRPCServers_UnreachableIsTyped(t *testing.T) {
	err := CheckRPCServers([]string{"127.0.0.1:1"}, 200*time.Millisecond)
	if err == nil {
		t.Fatal("недоступный узел прошёл гейт — загрузка упала бы в abort внутри llama.cpp")
	}
	var unreachable *RPCNodeUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("ошибка не типизирована как RPCNodeUnreachableError: %T %v", err, err)
	}
	if unreachable.Endpoint != "127.0.0.1:1" {
		t.Errorf("в ошибке не тот адрес: %q", unreachable.Endpoint)
	}
	msg := err.Error()
	for _, want := range []string{"127.0.0.1:1", "abort", "50052"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в тексте ошибки нет %q: %s", want, msg)
		}
	}
}

// TestR114_CheckRPCServers_NamesBadNodeAmongGood — из двух узлов называем плохой.
func TestR114_CheckRPCServers_NamesBadNodeAmongGood(t *testing.T) {
	good, ln := listenerR114(t)
	defer ln.Close()

	err := CheckRPCServers([]string{good, "127.0.0.1:1"}, 200*time.Millisecond)
	if err == nil {
		t.Fatal("набор с недоступным узлом прошёл гейт")
	}
	var unreachable *RPCNodeUnreachableError
	if !errors.As(err, &unreachable) || unreachable.Endpoint != "127.0.0.1:1" {
		t.Fatalf("назван не тот узел: %v", err)
	}
}

// TestR114_LoadModel_GateBlocksBeforeHeavyWork — R229: недоступный узел
// раскладки ИСКЛЮЧАЕТСЯ, а загрузка продолжается без него (не отменяется целиком).
//
// ПРЕЖНИЙ КОНТРАКТ (R114) — «загрузка отменена с typed-ошибкой» — защищал от
// abort() в llama.cpp, но оставлял оператора без модели, хотя донор необязателен.
// Теперь гейт фильтрует узлы: мёртвый адрес НЕ попадает в мост (безопасность
// сохранена), а модель грузится на живых узлах/локально. Контракт проверяем по
// двум фактам: типизированной ошибки НЕТ, узел записан в ExcludedRPCServers.
func TestR114_LoadModel_GateBlocksBeforeHeavyWork(t *testing.T) {
	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{"127.0.0.1:1"},
	})
	defer b.Close()

	err := b.LoadModelWithOpts(context.Background(), "net-model", "net-model.gguf", LoadModelOpts{})
	var unreachable *RPCNodeUnreachableError
	if errors.As(err, &unreachable) {
		t.Fatalf("загрузка по-прежнему отменяется целиком из-за донора: %v "+
			"(R229: узел исключается, загрузка идёт без него)", err)
	}
	got := b.ExcludedRPCServers()
	if len(got) != 1 || got[0] != "127.0.0.1:1" {
		t.Fatalf("недоступный узел не исключён: %v", got)
	}
}

// TestR114_GateInertWithoutRPC — без раскладки ни один гейт не вмешивается.
//
// Проверяем именно гейты (инференс и снимок устройств), а не саму загрузку: в
// stub-сборке мост не проверяет файл, и «успешная загрузка» упиралась бы в
// уборку TempDir фоновым писателем истории имён (шумный тест).
func TestR114_GateInertWithoutRPC(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	defer b.Close()

	if err := b.checkRPCNodesForInference(); err != nil {
		t.Fatalf("гейт инференса сработал без RPC: %v", err)
	}
	if snap := b.rpcDeviceSnapshot(); snap != nil {
		t.Fatalf("снимок устройств пошёл в RPC-часть без RPC-настройки: %+v", snap)
	}
}

// TestR114_UnreachableDetectionSeesNodeLoss — условие, по которому watchdog
// принимает решение: узел был жив, затем пропал.
func TestR114_UnreachableDetectionSeesNodeLoss(t *testing.T) {
	addr, ln := listenerR114(t)

	if bad := unreachableRPCServers([]string{addr}, 200*time.Millisecond); len(bad) != 0 {
		t.Fatalf("живой узел признан недоступным: %v", bad)
	}
	_ = ln.Close()

	// Ждём, пока ОС закроет порт (на Windows закрытие сокета асинхронно).
	deadline := time.Now().Add(3 * time.Second)
	for {
		bad := unreachableRPCServers([]string{addr}, 200*time.Millisecond)
		if len(bad) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("потеря узла %s не обнаружена — watchdog не выгрузит модель", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestR114_Watchdog_UnloadsModelAfterNodeLoss — watchdog выгружает модель, когда
// узел пропал (это и защищает от abort внутри C-моста на следующем запросе).
func TestR114_Watchdog_UnloadsModelAfterNodeLoss(t *testing.T) {
	old := rpcWatchdogInterval
	rpcWatchdogInterval = 20 * time.Millisecond
	defer func() { rpcWatchdogInterval = old }()

	addr, ln := listenerR114(t)
	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{addr},
	})

	const name = "sharded-model"
	// handle не нужен: UnloadModel терпит nil (проверяет if inst.handle != nil).
	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	b.startRPCWatchdog(name, b.cfg.RPCServers)
	_ = ln.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, exists := b.ModelState(name); !exists {
			return // модель выгружена — цель достигнута
		}
		if time.Now().After(deadline) {
			t.Fatal("watchdog не выгрузил модель после потери узла — следующий запрос уронил бы мастер")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestR114_Watchdog_DoesNotUnloadBusyModel — под активным запросом выгрузка
// откладывается (иначе use-after-free в C-мосте).
func TestR114_Watchdog_DoesNotUnloadBusyModel(t *testing.T) {
	old := rpcWatchdogInterval
	rpcWatchdogInterval = 20 * time.Millisecond
	defer func() { rpcWatchdogInterval = old }()

	addr, ln := listenerR114(t)
	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{addr},
	})

	const name = "busy-sharded-model"
	// handle не нужен: UnloadModel терпит nil (проверяет if inst.handle != nil).
	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	inst.incQueries() // имитация активного запроса (как в nonblocking_status_r83_test.go)
	defer inst.decQueries()

	b.startRPCWatchdog(name, b.cfg.RPCServers)
	_ = ln.Close()

	time.Sleep(300 * time.Millisecond)
	if _, exists := b.ModelState(name); !exists {
		t.Fatal("модель выгружена под активным запросом — это use-after-free в C-мосте")
	}

	b.stopRPCWatchdog(name)
}

// TestR114_DeviceSnapshot_SkipsDeadNode — снимок устройств НЕ трогает мёртвый узел.
//
// Живой стек падения мастера: GET /api/models → MemfitBudget → rpcDeviceSnapshot →
// bridge.CollectDevices → ggml_backend_rpc_add_server → abort(). То есть мастер
// падал от ЧТЕНИЯ СПИСКА МОДЕЛЕЙ. После правки снимок собирается без RPC-части
// (и об этом пишется в лог), планировщик считает локальную карту.
func TestR114_DeviceSnapshot_SkipsDeadNode(t *testing.T) {
	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{"127.0.0.1:1"},
	})

	b.rpcDeviceSnapshot() // не должно падать: к мёртвому узлу не обращаемся

	if b.rpcWarnAt.IsZero() {
		t.Fatal("предупреждение «узел раскладки недоступен» не залогировано — " +
			"значит снимок всё-таки пошёл в RPC-часть (там abort)")
	}
	free, total, devices := b.remoteVRAMBytes()
	if devices != 0 || free != 0 || total != 0 {
		t.Fatalf("удалённая память учтена при мёртвом узле: free=%d total=%d devices=%d", free, total, devices)
	}
}

// TestR114_DeviceSnapshot_UsesLiveNode — живой узел идёт по обычному пути.
func TestR114_DeviceSnapshot_UsesLiveNode(t *testing.T) {
	addr, ln := listenerR114(t)
	defer ln.Close()

	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{addr},
	})
	b.rpcDeviceSnapshot()

	if !b.rpcWarnAt.IsZero() {
		t.Fatal("живой узел помечен как недоступный")
	}
}

// TestR114_Generate_RefusesWhenNodeDead — генерация не начинается, если узел пропал.
//
// Иначе llama.cpp обращается к мёртвому RPC-устройству и вызывает abort(): клиент
// получает не ошибку, а перезапуск мастера. Ожидаем понятную типизированную ошибку.
func TestR114_Generate_RefusesWhenNodeDead(t *testing.T) {
	b := NewBackend(Config{
		ModelsDir:  t.TempDir(),
		RPCEnabled: true,
		RPCServers: []string{"127.0.0.1:1"},
	})
	const name = "rpc-model"
	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	inst.handle = &bridge.ModelHandle{}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	_, err := b.Generate(name, "hello", bridge.GenerationParams{})
	if err == nil {
		t.Fatal("генерация начата при мёртвом узле раскладки — это abort внутри llama.cpp")
	}
	var unreachable *RPCNodeUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("ожидалась ошибка узла раскладки, получено: %T %v", err, err)
	}

	_, err = b.GenerateStream(name, "hello", bridge.GenerationParams{}, func(string) bool { return true })
	if err == nil {
		t.Fatal("стриминг начат при мёртвом узле раскладки")
	}
	if !errors.As(err, &unreachable) {
		t.Fatalf("ожидалась ошибка узла раскладки (stream), получено: %T %v", err, err)
	}
}

// TestR114_Generate_NoGateWithoutRPC — без раскладки гейт генерации не вмешивается.
func TestR114_Generate_NoGateWithoutRPC(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "plain-model"
	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	inst.handle = &bridge.ModelHandle{}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	_, err := b.Generate(name, "hello", bridge.GenerationParams{})
	var unreachable *RPCNodeUnreachableError
	if errors.As(err, &unreachable) {
		t.Fatalf("без RPC гейт сработал: %v", err)
	}
}

// TestR114_Watchdog_StopIsSafe — остановка несуществующего наблюдения не паникует.
func TestR114_Watchdog_StopIsSafe(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	b.stopRPCWatchdog("never-started")

	stop := b.startRPCWatchdog("no-servers", nil)
	stop()
	b.stopRPCWatchdog("no-servers")
}
