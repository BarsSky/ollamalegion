// rpc_mode_r115_test.go — R115: при выключенном флаге раскладки нет НИГДЕ.
//
// Проверяется не «флаг прочитан», а отсутствие следов: узлы не возвращаются
// потребителям (мост, memfit, сторож), а снимок устройств не берётся — то есть
// dial'ов к узлам не происходит вовсе. Это и есть требование «выключение по
// флагу»: поведение как у сборки без RPC.
package cppbackend

import (
	"testing"
	"time"
)

func TestR115_DisabledRPCExposesNoServers(t *testing.T) {
	b := &Backend{cfg: Config{
		RPCEnabled: false,
		RPCServers: []string{"192.0.2.10:50052", "192.0.2.11:50052"},
	}}

	if b.RPCEnabled() {
		t.Errorf("R115: режим выключен — RPCEnabled() обязан быть false")
	}
	if got := b.activeRPCServers(); len(got) != 0 {
		t.Errorf("R115: при выключенном режиме узлы не отдаются потребителям, получено %v", got)
	}
	if got := b.ActiveRPCServers(); len(got) != 0 {
		t.Errorf("R115: отчёт обязан показывать пустой список, получено %v", got)
	}
	// Снимок устройств — единственный путь, который реально дозванивается до
	// узлов. При выключенном режиме он обязан вернуться сразу.
	if snap := b.rpcDeviceSnapshot(); snap != nil {
		t.Errorf("R115: при выключенном режиме снимок устройств не собирается, получено %v", snap)
	}
}

func TestR115_EnabledRPCExposesServers(t *testing.T) {
	servers := []string{"192.0.2.10:50052"}
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: servers}}

	if !b.RPCEnabled() {
		t.Errorf("R115: режим включён — RPCEnabled() обязан быть true")
	}
	got := b.activeRPCServers()
	if len(got) != 1 || got[0] != servers[0] {
		t.Errorf("R115: узлы = %v, ожидался %v", got, servers)
	}
	// Отчёт не должен отдавать внутренний срез: его правка изменила бы конфиг
	// работающего бэкенда.
	report := b.ActiveRPCServers()
	report[0] = "подменено"
	if b.cfg.RPCServers[0] != servers[0] {
		t.Errorf("R115: отчётный список — копия, а не внутренний срез конфига")
	}
}

func TestR115_EnabledWithoutNodesIsStillOff(t *testing.T) {
	// «Включено», но узлов нет: раскладки нет, и это не ошибка — узлы могут
	// появиться позже (WebUI настраивает их на живом стенде).
	b := &Backend{cfg: Config{RPCEnabled: true}}
	if got := b.activeRPCServers(); len(got) != 0 {
		t.Errorf("R115: без узлов раскладка не строится, получено %v", got)
	}
}

func TestR115_NilBackendIsSafe(t *testing.T) {
	var b *Backend
	if b.RPCEnabled() {
		t.Errorf("R115: nil-бэкенд не может быть включён")
	}
	if got := b.activeRPCServers(); len(got) != 0 {
		t.Errorf("R115: nil-бэкенд не отдаёт узлов, получено %v", got)
	}
	if got := b.ActiveRPCServers(); len(got) != 0 {
		t.Errorf("R115: nil-бэкенд отдаёт пустой список, получено %v", got)
	}
	if got := b.LayoutBreakdown(); got.RPCEnabled || got.Nodes != 0 || len(got.Servers) != 0 {
		t.Errorf("R115: nil-бэкенд не имеет раскладки, получено %+v", got)
	}
}

func TestR115_DisabledLayoutHasNoRemoteCapacity(t *testing.T) {	// Ключевое требование «выключено по флагу»: вклад узлов равен нулю, и он не
	// может появиться из списка узлов, оставшегося в конфиге.
	b := &Backend{cfg: Config{
		RPCEnabled: false,
		RPCServers: []string{"192.0.2.10:50052"},
	}}
	lb := b.LayoutBreakdown()

	if lb.RPCEnabled {
		t.Errorf("R115: режим выключен")
	}
	if lb.Nodes != 0 || lb.RemoteFree != 0 || lb.RemoteTotal != 0 {
		t.Errorf("R115: при выключенном режиме узлы не учитываются: %+v", lb)
	}
	if len(lb.Servers) != 0 {
		t.Errorf("R115: при выключенном режиме список узлов пуст, получено %v", lb.Servers)
	}
	if lb.LayoutFree != lb.LocalFree || lb.LayoutTotal != lb.LocalTotal {
		t.Errorf("R115: без узлов итог равен локальной карте: %+v", lb)
	}
}

// TestR115_SetRPCConfigReachesLiveBackend — R115: настройка раскладки, пришедшая
// через API, обязана менять ЖИВОЙ конфиг бэкенда, а не только объект конфигурации
// пакета cppworker.
//
// Живой дефект (2026-10-13): `PUT config/update {"rpcEnabled":false}` отвечал
// `{"applied":["rpcEnabled","rpcServers(cleared)"]}`, а /api/models продолжал
// отдавать rpc_enabled=true, узел и 7009 MB удалённой памяти — настройка
// «сохранялась» без эффекта.
func TestR115_SetRPCConfigReachesLiveBackend(t *testing.T) {
	b := &Backend{cfg: Config{
		RPCEnabled: true,
		RPCServers: []string{"192.0.2.10:50052"},
	}}
	// Снимок устройств считается свежим: после смены режима он обязан сброситься,
	// иначе новый список узлов не начал бы действовать до истечения TTL (30 с).
	b.rpcDevAt = time.Now()

	b.SetRPCConfig(false, nil)

	if b.RPCEnabled() {
		t.Errorf("R115: выключение через API обязано доходить до живого конфига")
	}
	if got := b.ActiveRPCServers(); len(got) != 0 {
		t.Errorf("R115: при выключении узлы не отдаются, получено %v", got)
	}
	if !b.rpcDevAt.IsZero() {
		t.Errorf("R115: кэш снимка устройств обязан сброситься")
	}

	// Выключение очищает список даже если узлы передали вместе с false.
	b.SetRPCConfig(true, []string{"192.0.2.10:50052"})
	b.SetRPCConfig(false, []string{"192.0.2.11:50052"})
	if b.RPCEnabled() || len(b.cfg.RPCServers) != 0 {
		t.Errorf("R115: false сильнее списка узлов: enabled=%v servers=%v",
			b.RPCEnabled(), b.cfg.RPCServers)
	}

	b.SetRPCConfig(true, []string{"192.0.2.10:50052", "192.0.2.11:50052"})
	if !b.RPCEnabled() || len(b.ActiveRPCServers()) != 2 {
		t.Errorf("R115: включение с двумя узлами: enabled=%v servers=%v",
			b.RPCEnabled(), b.ActiveRPCServers())
	}
}

func TestR115_SetRPCConfigNilBackendIsSafe(t *testing.T) {
	var b *Backend
	b.SetRPCConfig(true, []string{"192.0.2.10:50052"}) // не должно паниковать
}
