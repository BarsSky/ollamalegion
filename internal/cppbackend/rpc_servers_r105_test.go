package cppbackend

// R105 (S1 плана 2026-10-07): RPC-узлы доезжают из конфига до параметров моста.
//
// Проверяем именно проводку и КОПИЮ среза: параметры моста живут дольше вызова, а
// конфиг перезаписывается reload'ом — общий срез дал бы гонку и «внезапно
// изменившиеся» адреса узлов на живой загрузке.

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

func TestApplyRPCServersR105_CopiesIntoBridgeParams(t *testing.T) {
	configured := []string{"192.0.2.11:50052", "192.0.2.12:50052"}
	cfg := bridge.ModelConfig{}

	got := applyRPCServersR105(&cfg, configured)
	if len(got) != 2 {
		t.Fatalf("получено %v, ожидались оба узла", got)
	}
	if len(cfg.RPCServers) != 2 || cfg.RPCServers[0] != configured[0] {
		t.Fatalf("узлы не попали в параметры моста: %v", cfg.RPCServers)
	}

	// Копия, а не общий срез.
	configured[0] = "изменено-снаружи"
	if cfg.RPCServers[0] == "изменено-снаружи" {
		t.Error("параметры моста делят срез с конфигом: reload конфигурации менял бы адреса узлов")
	}
}

func TestApplyRPCServersR105_NoServersNoChange(t *testing.T) {
	cfg := bridge.ModelConfig{}
	if got := applyRPCServersR105(&cfg, nil); got != nil {
		t.Errorf("без настроенных узлов возвращать нечего: %v", got)
	}
	if len(cfg.RPCServers) != 0 {
		t.Errorf("без настроенных узлов параметры моста не меняются: %v", cfg.RPCServers)
	}

	if got := applyRPCServersR105(nil, []string{"a:1"}); got != nil {
		t.Errorf("nil-параметры не паникуют и ничего не возвращают: %v", got)
	}
}
