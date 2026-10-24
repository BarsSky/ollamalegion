// rpc_mode_r115_test.go — R115: «выключено флагом» обязано быть выключено.
//
// Живой дефект, который эти тесты закрывают: `CPPWORKER_RPC_ENABLE=false` не
// выключал раскладку, если был задан `CPPWORKER_RPC_SERVERS` — список узлов
// безусловно ставил `RPCEnabled = true`. Оператор выключал режим, а мастер
// продолжал дозваниваться до узлов и планировать по их памяти.
package main

import "testing"

func TestR115_ExplicitDisableBeatsServerList(t *testing.T) {
	m := resolveRPCMode("false", "192.0.2.10:50052")

	if m.Enabled {
		t.Errorf("R115: явное false обязано выключать раскладку, получено Enabled=true")
	}
	if len(m.Servers) != 0 {
		t.Errorf("R115: при выключенном режиме список узлов обязан быть очищен, осталось %v", m.Servers)
	}
	if !m.Explicit {
		t.Errorf("R115: разбор обязан помнить, что флаг был задан явно")
	}
	if len(m.Notes) == 0 {
		t.Errorf("R115: решение «выключено флагом, список проигнорирован» должно быть в логе")
	}
}

func TestR115_ServerListStillEnablesWithoutFlag(t *testing.T) {
	// Совместимость: до R115 непустой список узлов включал раскладку, и это
	// поведение сохраняется, когда флаг не задан вовсе.
	m := resolveRPCMode("", "192.0.2.10:50052")

	if !m.Enabled {
		t.Errorf("R115: без флага непустой список узлов обязан включать раскладку")
	}
	if len(m.Servers) != 1 || m.Servers[0] != "192.0.2.10:50052" {
		t.Errorf("R115: узлы = %v, ожидался один 192.0.2.10:50052", m.Servers)
	}
	if m.Explicit {
		t.Errorf("R115: флаг не задавали — Explicit обязан быть false")
	}
}

func TestR115_ExplicitEnable(t *testing.T) {
	m := resolveRPCMode("true", "192.0.2.10:50052,192.0.2.11:50052")
	if !m.Enabled || len(m.Servers) != 2 {
		t.Errorf("R115: включено с двумя узлами, получено enabled=%v servers=%v", m.Enabled, m.Servers)
	}
}

func TestR115_EnabledWithoutNodesWarns(t *testing.T) {
	m := resolveRPCMode("true", "")
	if !m.Enabled {
		t.Errorf("R115: включённый режим без узлов остаётся включённым (узлы появятся позже)")
	}
	if len(m.Warnings) == 0 {
		t.Errorf("R115: «включено без узлов» — это предупреждение, а не тишина")
	}
}

func TestR115_DisabledByDefault(t *testing.T) {
	m := resolveRPCMode("", "")
	if m.Enabled {
		t.Errorf("R115: без флага и без узлов раскладка выключена (поведение прежнее)")
	}
	if m.Explicit {
		t.Errorf("R115: Explicit обязан быть false")
	}
}

func TestR115_InvalidInputsAreReportedNotSwallowed(t *testing.T) {
	// Битая переменная не должна выглядеть как рабочая настройка.
	if m := resolveRPCMode("", "not-a-host-port"); len(m.Warnings) == 0 {
		t.Errorf("R115: битый список узлов обязан давать предупреждение")
	}
	if m := resolveRPCMode("maybe", "192.0.2.10:50052"); len(m.Warnings) == 0 {
		t.Errorf("R115: битый bool обязан давать предупреждение")
	}
}

func TestR115_DuplicateNodeRejected(t *testing.T) {
	m := resolveRPCMode("true", "192.0.2.10:50052,192.0.2.10:50052")
	if len(m.Warnings) == 0 {
		t.Errorf("R115: дубль узла в списке обязан давать предупреждение")
	}
	if len(m.Servers) != 0 {
		t.Errorf("R115: при отказе разбора список не принимается частично: %v", m.Servers)
	}
}
