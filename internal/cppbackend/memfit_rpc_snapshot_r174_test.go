//go:build llama_stub

// memfit_rpc_snapshot_r174_test.go — R174 (2026-10-18).
//
// НАЙДЕНО СЦЕНАРИЯМИ НАСТРОЕК (params-e2e --scenario reload + живой стенд):
// «Применить профиль» из WebUI перезагружает модель — и раскладка ИСЧЕЗАЕТ.
// Было CPU:1 CUDA0:23 RPC0:19, стало CPU:23 CUDA0:20: девятнадцать слоёв уехали
// в RAM, скорость упала, а UI не сказал ни слова.
//
// Причина не в приказе балансера: во время работы (reload держит loadMu) снимок
// устройств намеренно НЕ запрашивает узлы — запрос памяти влезает в единственное
// соединение узла и роняет клиента по GGML_ABORT (R115). Но если кэш снимка
// (TTL 30 с) к этому моменту истёк, снимок собирался БЕЗ узлов, и планировщик
// видел только локальную карту.
//
// Здесь фиксируется правило: занятость не стирает раскладку — планируем по
// последнему УСПЕШНОМУ замеру, если список узлов не менялся.
package cppbackend

import (
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// TestR174_LastGoodRPCSnapshot_UsedForSameServers — снимок с узлами отдаётся,
// пока список узлов тот же (это и восстанавливает раскладку при reload).
func TestR174_LastGoodRPCSnapshot_UsedForSameServers(t *testing.T) {
	b := &Backend{}
	good := []bridge.DeviceInfo{
		{Name: "CUDA0", Type: 1, MemoryTotal: 8192 << 20, MemoryFree: 7000 << 20},
		{Name: "RPC0", Type: 1, IsRemote: true, MemoryTotal: 8123 << 20, MemoryFree: 6800 << 20},
	}
	b.rpcDevGood = good
	b.rpcDevGoodAt = time.Now().Add(-5 * time.Minute) // кэш TTL давно истёк
	b.rpcDevGoodKey = "192.0.2.10:50052"

	got, age, ok := b.lastGoodRPCSnapshot([]string{"192.0.2.10:50052"})
	if !ok {
		t.Fatal("последний успешный снимок с узлами не найден — при reload раскладка " +
			"снова будет потеряна (слои уедут в RAM)")
	}
	if len(got) != 2 {
		t.Fatalf("в снимке %d устройств, want 2 (CUDA0 + RPC0)", len(got))
	}
	hasRPC := false
	for _, d := range got {
		if d.Name == "RPC0" {
			hasRPC = true
		}
	}
	if !hasRPC {
		t.Error("в снимке нет RPC0 — планировщик не увидит узел")
	}
	if age < 4*time.Minute {
		t.Errorf("возраст снимка %v, want ~5 минут (возраст нужен для честного лога)", age)
	}
}

// TestR174_LastGoodRPCSnapshot_RejectedAfterNodesChange — после смены списка
// узлов старый снимок НЕ годится: это план по машине, которой в раскладке нет.
func TestR174_LastGoodRPCSnapshot_RejectedAfterNodesChange(t *testing.T) {
	b := &Backend{}
	b.rpcDevGood = []bridge.DeviceInfo{{Name: "RPC0", Type: 1, IsRemote: true, MemoryTotal: 8123 << 20, MemoryFree: 6800 << 20}}
	b.rpcDevGoodAt = time.Now()
	b.rpcDevGoodKey = "192.0.2.10:50052"

	if _, _, ok := b.lastGoodRPCSnapshot([]string{"192.0.2.99:50052"}); ok {
		t.Error("снимок для ДРУГОГО узла принят: планировщик разложил бы слои на машину, " +
			"которой в раскладке нет")
	}
	if _, _, ok := b.lastGoodRPCSnapshot(nil); ok {
		t.Error("снимок отдан при пустом списке узлов")
	}
}

// TestR174_LastGoodRPCSnapshot_EmptyBeforeFirstSuccess — пока успешного замера не
// было, отдавать нечего: вызывающий обязан планировать по локальной карте.
func TestR174_LastGoodRPCSnapshot_EmptyBeforeFirstSuccess(t *testing.T) {
	b := &Backend{}
	if _, _, ok := b.lastGoodRPCSnapshot([]string{"192.0.2.10:50052"}); ok {
		t.Error("снимок отдан до первого успешного замера")
	}
}
