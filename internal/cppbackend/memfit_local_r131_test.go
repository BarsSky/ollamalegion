//go:build llama_stub

// memfit_local_r131_test.go — R131 (2026-10-16): справочный путь НЕ ходит к RPC.
//
// ЖИВОЙ ДЕФЕКТ. `GET /api/models` считал бюджет с RPC-узлами, отдавал недоступный
// узел в C-мост, и llama.cpp вызывал abort() — мастер падал от чтения списка:
//
//	SIGABRT: abort (signal arrived during cgo execution)
//	  c/bridge.CollectDevices                 bridge.c:1094 (add_server)
//	  cppbackend.(*Backend).rpcDeviceSnapshot memfit_adapter.go:249
//	  cppbackend.(*Backend).MemfitBudget      memfit_adapter.go:141
//	  main.handleListModels                   ← GET /api/models
//
// Правка гейта (R130: «TCP-connect != живой узел») этого не вылечила, и это
// доказано живой проверкой: для УДАЛЁННОГО опубликованного порта прокси принимает
// соединение и молчит — ровно как живой rpc-server, поэтому по connect+тишине
// dead-end неотличим. Единственная надёжная защита read-only пути — не обращаться
// к узлам вовсе, что и проверяется здесь.
package cppbackend

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// withRPCSnapshotCounter — подменить шов снимка и посчитать обращения.
func withRPCSnapshotCounter(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := rpcSnapshotForTest
	rpcSnapshotForTest = func(b *Backend) []bridge.DeviceInfo {
		calls++
		return nil
	}
	t.Cleanup(func() { rpcSnapshotForTest = prev })
	return &calls
}

// TestR131_LocalBudgetNeverTouchesRPC — ГЛАВНОЕ свойство: справочный бюджет не
// обращается к RPC-узлам ни разу, даже когда раскладка настроена.
func TestR131_LocalBudgetNeverTouchesRPC(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}

	_ = b.MemfitBudgetLocal()
	if *calls != 0 {
		t.Errorf("MemfitBudgetLocal обратился к RPC-узлам %d раз: именно это и роняло "+
			"мастер через abort() в llama.cpp на GET /api/models", *calls)
	}

	_ = b.CalculateResourceLimitsLocal("m")
	if *calls != 0 {
		t.Errorf("CalculateResourceLimitsLocal обратился к RPC-узлам %d раз — "+
			"справочная ручка снова может уронить процесс", *calls)
	}
}

// TestR131_LoadBudgetStillUsesRPC — регресс-защита: путь загрузки обязан
// по-прежнему учитывать ёмкость донора, иначе раскладка планируется только по
// локальной карте (это отдельный живой дефект S1).
func TestR131_LoadBudgetStillUsesRPC(t *testing.T) {
	calls := withRPCSnapshotCounter(t)
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}

	_ = b.MemfitBudget()
	if *calls == 0 {
		t.Error("MemfitBudget (путь загрузки) перестал спрашивать RPC-узлы: планировщик " +
			"потеряет ёмкость донора и урежет план слоёв")
	}
}

// TestR131_ListHandlerUsesNoDialVariants — защита от отката: справочная ручка
// обязана использовать варианты расчёта БЕЗ дозвона до узлов.
//
// R132 (2026-10-16): вариант переименован из *Local в *Cached — справочный расчёт
// по-прежнему не дозванивается, но теперь учитывает уже собранный снимок узлов
// (иначе мастер в ответе выглядел беднее, чем он есть).
func TestR131_ListHandlerUsesNoDialVariants(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/cppworker/handlers_model.go")
	if err != nil {
		t.Fatalf("read handlers_model.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "MemfitBudgetCached()") {
		t.Error("handleListModels снова зовёт бюджет с дозвоном — чтение списка " +
			"моделей опять может уронить мастер через RPC и abort() в llama.cpp")
	}
	if !strings.Contains(src, "CalculateResourceLimitsCached(") {
		t.Error("handleListModels снова зовёт CalculateResourceLimits() (с дозвоном)")
	}
}
