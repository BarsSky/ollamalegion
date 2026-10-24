//go:build llama_stub

// rpc_server_idempotent_r173_test.go — R173 (2026-10-18).
//
// ЗАЧЕМ. Живая авария на доноре: приказ балансера (PUT /config/update с
// rpcServerEnabled/rpcServerPort) приходил на каждом согласовании роли, и
// обработчик ГАСИЛ живой дочерний rpc-server просто потому, что поле было в
// запросе. Узел убивался прямо во время передачи весов мастером — мастер терял
// сессию и умирал (код 141/SIGPIPE), контейнер перезапускался. Оператор видел
// «встроенный узел падает», хотя узел был исправен.
//
// Здесь фиксируется контракт: повторный приказ с ТЕМИ ЖЕ значениями не трогает
// живой процесс; пересоздание допускается только при смене идентичности
// (адрес/порт/бинарь/внешний адрес/отладка), потому что они зашиты в argv/env.
package cppbackend

import (
	"testing"
	"time"
)

// TestR173_RequiresRecreate_Matrix — что считается сменой идентичности.
func TestR173_RequiresRecreate_Matrix(t *testing.T) {
	base := Config{
		RPCServerBind: "127.0.0.1",
		RPCServerPort: 50052,
		RPCServerBin:  "/app/rpc-server",
	}
	c := NewRPCServerChild(base)

	same := base
	if c.RequiresRecreate(same) {
		t.Fatal("те же настройки требуют пересоздания: повторный приказ будет убивать живой узел " +
			"(это и была авария R173)")
	}

	// Желание (включён/выключен) пересоздания НЕ требует: SetEnabled идемпотентен.
	withEnabled := base
	withEnabled.RPCServerEnabled = true
	if c.RequiresRecreate(withEnabled) {
		t.Error("смена только «включённости» требует пересоздания — живой узел будет убит зря")
	}

	cases := map[string]Config{
		"порт":          {RPCServerBind: "127.0.0.1", RPCServerPort: 50055, RPCServerBin: "/app/rpc-server"},
		"адрес":         {RPCServerBind: "0.0.0.0", RPCServerPort: 50052, RPCServerBin: "/app/rpc-server"},
		"бинарь":        {RPCServerBind: "127.0.0.1", RPCServerPort: 50052, RPCServerBin: "/opt/rpc-server"},
		"отладка":       {RPCServerBind: "127.0.0.1", RPCServerPort: 50052, RPCServerBin: "/app/rpc-server", RPCServerDebug: true},
		"внешний адрес": {RPCServerBind: "127.0.0.1", RPCServerPort: 50052, RPCServerBin: "/app/rpc-server", RPCServerExternalAddr: "10.0.0.2:50055"},
	}
	for name, cfg := range cases {
		if !c.RequiresRecreate(cfg) {
			t.Errorf("%s: смена идентичности НЕ требует пересоздания — настройка не применится", name)
		}
	}
}

// TestR173_RepeatOrderKeepsLiveChild — главный контрфакт: повторный приказ с теми
// же значениями не должен ни гасить, ни перезапускать живой дочерний процесс.
//
// До R173 это было ровно наоборот: обработчик вызывал Stop() + NewRPCServerChild
// на КАЖДОЕ применение rpcServerEnabled/rpcServerPort, то есть узел умирал под
// работающей сессией раскладки.
func TestR173_RepeatOrderKeepsLiveChild(t *testing.T) {
	shortListenWait(t)
	port := freePort(t)
	c := newHelperChild(t, port)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Listening })
	if first.PID <= 0 {
		t.Fatalf("узел не поднялся: %+v", first)
	}

	// «Повторный приказ»: те же настройки. Проверяем решение и результат.
	cfg := Config{
		RPCServerEnabled: true,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    port,
		RPCServerBin:     c.bin,
	}
	if c.RequiresRecreate(cfg) {
		t.Fatal("повторный приказ требует пересоздания — живой узел будет убит")
	}
	if err := c.SetEnabled(true); err != nil {
		t.Fatalf("повторный SetEnabled(true): %v", err)
	}
	after := c.Status()
	if after.PID != first.PID {
		t.Errorf("PID сменился %d → %d: повторный приказ перезапустил живой узел",
			first.PID, after.PID)
	}
	if !after.Listening {
		t.Error("после повторного приказа узел не слушает порт")
	}

	// А смена порта обязана пересоздать — иначе настройка не применится.
	changed := cfg
	changed.RPCServerPort = port + 1000
	if !c.RequiresRecreate(changed) {
		t.Error("смена порта не требует пересоздания — новый порт не будет слушаться")
	}

	// Готовимся к завершению: тест не должен оставлять процесс.
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	dead := waitStatus(t, c, func(s RPCServerStatus) bool { return !s.Running })
	if dead.Running {
		t.Errorf("после Stop узел всё ещё работает (pid %d)", dead.PID)
	}
	// Штатная остановка не должна выглядеть падением (иначе автоперезапуск и
	// предупреждения в логе будут врать).
	if dead.LastExit != "" {
		t.Errorf("LastExit = %q после штатного Stop — остановка выглядит как падение", dead.LastExit)
	}
	if el := time.Since(c.startedAt); el < 0 {
		t.Fatalf("время старта в будущем: %v", el)
	}
}
