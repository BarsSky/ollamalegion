// layout_remote_last_known_r348_test.go — R348 (2026-10-31).
//
// ЖИВОЙ ДЕФЕКТ, ПОЙМАННЫЙ НА ДВУХУЗЛОВОМ СТЕНДЕ (см.
// docs/plans/2026-10-31-two-node-donor-check.md). Донор доступен, раскладка
// работает (23 слоя локально + 19 на доноре), но `vram_remote_*` в /api/models
// показывали 0/0: снимок узлов живёт rpcDeviceTTL = 30 с, и после этого справочный
// путь честно обнуляет вклад узлов.
//
// Почему «просто показывать старое» — неправильно: R132 уже решил, что протухший
// снимок нельзя выдавать за текущую ёмкость (иначе оператор видит гигабайты донора,
// которого в раскладке уже нет), и это закреплено тестом
// TestR132_StaleSnapshotIsNotPresentedAsCurrent.
//
// Поэтому R348 вводит ОТДЕЛЬНУЮ историю: ёмкость остаётся строго текущей, а
// последний замер живёт в LastKnown* с возрастом и в решениях не участвует.
//
// Проверяется:
//  1. протухший снимок: Nodes/RemoteFree по-прежнему 0 и source=none (контракт R132
//     не нарушен), но LastKnown* отдаёт замер и его возраст;
//  2. свежий снимок: LastKnown* = текущий вклад, возраст ~0;
//  3. снимок для ДРУГОГО списка узлов в историю не попадает (это память машины,
//     которой в раскладке нет);
//  4. до первого успешного замера истории нет вовсе (нули, а не выдуманные числа);
//  5. RPC выключен: история не показывается (узлы не используются нигде, R115).
package cppbackend

import (
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// r348RemoteDevice — одно удалённое устройство (4 ГиБ свободно).
func r348RemoteDevice() bridge.DeviceInfo {
	return bridge.DeviceInfo{Name: "RPC0", Type: 1, IsRemote: true, MemoryTotal: 8123 << 20, MemoryFree: 4 << 30}
}

// TestR348_StaleSnapshotKeepsHistoryButNotCapacity — главный сценарий: замер был,
// но протух. Ёмкость — ноль (R132), история — есть и с возрастом.
func TestR348_StaleSnapshotKeepsHistoryButNotCapacity(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}
	b.rpcDevices = []bridge.DeviceInfo{r348RemoteDevice()}
	b.rpcDevAt = time.Now().Add(-10 * time.Minute)

	layout := b.LayoutBreakdownCached()

	if layout.Nodes != 0 || layout.RemoteFree != 0 {
		t.Errorf("протухший снимок попал в ТЕКУЩУЮ ёмкость: nodes=%d remoteFree=%d — "+
			"это нарушает R132 (протухшее не выдаём за сегодняшнее)", layout.Nodes, layout.RemoteFree)
	}
	if layout.RemoteSource != RemoteSourceNone {
		t.Errorf("источник %q, want %q: протухший снимок не должен называться текущим",
			layout.RemoteSource, RemoteSourceNone)
	}
	if layout.RemoteLastKnownNodes != 1 || layout.RemoteLastKnownFree != 4<<30 {
		t.Errorf("история замера потеряна: nodes=%d free=%d, want 1 / 4 ГиБ — "+
			"без неё «0 МБ узлов» неотличимо от «7 ГБ донора, измеренные 10 минут назад»",
			layout.RemoteLastKnownNodes, layout.RemoteLastKnownFree)
	}
	if layout.RemoteLastKnownAge < 9*time.Minute {
		t.Errorf("возраст последнего замера %v, want ~10 минут: без возраста "+
			"оператор примет старые данные за свежие", layout.RemoteLastKnownAge)
	}
}

// TestR348_FreshSnapshotHistoryMatchesCapacity — пока снимок свежий, история и
// ёмкость совпадают, а возраст близок к нулю.
func TestR348_FreshSnapshotHistoryMatchesCapacity(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}
	b.rpcDevices = []bridge.DeviceInfo{r348RemoteDevice()}
	b.rpcDevAt = time.Now()

	layout := b.LayoutBreakdownCached()

	if layout.Nodes != 1 || layout.RemoteFree != 4<<30 {
		t.Fatalf("свежий снимок не попал в ёмкость: nodes=%d remoteFree=%d", layout.Nodes, layout.RemoteFree)
	}
	if layout.RemoteLastKnownNodes != 1 || layout.RemoteLastKnownFree != 4<<30 {
		t.Errorf("история расходится с текущей ёмкостью: nodes=%d free=%d",
			layout.RemoteLastKnownNodes, layout.RemoteLastKnownFree)
	}
	if layout.RemoteLastKnownAge > time.Minute {
		t.Errorf("возраст свежего замера %v — слишком большой", layout.RemoteLastKnownAge)
	}
}

// TestR348_HistoryIgnoresSnapshotOfAnotherNodeList — снимок для другого списка
// узлов в историю не попадает: это была бы память машины, которой в раскладке нет.
func TestR348_HistoryIgnoresSnapshotOfAnotherNodeList(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.20:50052"}}}
	b.rpcDevGood = []bridge.DeviceInfo{r348RemoteDevice()}
	b.rpcDevGoodAt = time.Now().Add(-2 * time.Minute)
	b.rpcDevGoodKey = "192.0.2.10:50052" // ДРУГОЙ узел

	layout := b.LayoutBreakdownCached()
	if layout.RemoteLastKnownNodes != 0 || layout.RemoteLastKnownFree != 0 {
		t.Errorf("в историю попал снимок узла, которого нет в раскладке: nodes=%d free=%d",
			layout.RemoteLastKnownNodes, layout.RemoteLastKnownFree)
	}
}

// TestR348_NoHistoryBeforeFirstMeasurement — до первого успешного замера истории
// нет: нули, а не выдуманное «сколько-нибудь».
func TestR348_NoHistoryBeforeFirstMeasurement(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}

	layout := b.LayoutBreakdownCached()
	if layout.RemoteLastKnownNodes != 0 || layout.RemoteLastKnownFree != 0 || layout.RemoteLastKnownAge != 0 {
		t.Errorf("история появилась без замера: nodes=%d free=%d age=%v",
			layout.RemoteLastKnownNodes, layout.RemoteLastKnownFree, layout.RemoteLastKnownAge)
	}
}

// TestR348_HistoryHiddenWhenRPCDisabled — при выключенной раскладке узлы не
// используются нигде (R115), поэтому и истории быть не должно.
func TestR348_HistoryHiddenWhenRPCDisabled(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: false, RPCServers: []string{"192.0.2.10:50052"}}}
	b.rpcDevGood = []bridge.DeviceInfo{r348RemoteDevice()}
	b.rpcDevGoodAt = time.Now()
	b.rpcDevGoodKey = "192.0.2.10:50052"

	layout := b.LayoutBreakdownCached()
	if layout.RemoteLastKnownNodes != 0 {
		t.Errorf("история показана при выключенной раскладке: nodes=%d", layout.RemoteLastKnownNodes)
	}
}

// TestR348_HistoryFromLastGoodSnapshot — если текущий снимок стал локальным
// (загрузка шла при занятости, снимок заменился), история берётся из последнего
// УСПЕШНОГО замера для того же списка узлов (R174).
func TestR348_HistoryFromLastGoodSnapshot(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"192.0.2.10:50052"}}}
	// Текущий снимок — только локальная карта (типичный результат busy-пути).
	b.rpcDevices = []bridge.DeviceInfo{{Name: "CUDA0", Type: 1, MemoryTotal: 8192 << 20, MemoryFree: 7000 << 20}}
	b.rpcDevAt = time.Now()
	// Последний успешный замер с узлом.
	b.rpcDevGood = []bridge.DeviceInfo{r348RemoteDevice()}
	b.rpcDevGoodAt = time.Now().Add(-3 * time.Minute)
	b.rpcDevGoodKey = "192.0.2.10:50052"

	layout := b.LayoutBreakdownCached()
	if layout.RemoteLastKnownNodes != 1 || layout.RemoteLastKnownFree != 4<<30 {
		t.Errorf("история не взята из последнего успешного замера: nodes=%d free=%d",
			layout.RemoteLastKnownNodes, layout.RemoteLastKnownFree)
	}
	if layout.RemoteLastKnownAge < 2*time.Minute {
		t.Errorf("возраст истории %v, want ~3 минуты (взят из lastGood, а не из свежего локального снимка)",
			layout.RemoteLastKnownAge)
	}
}
