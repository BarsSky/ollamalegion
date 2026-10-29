//go:build llama_stub

// rpc_server_config_r123_test.go — R123 (2026-10-16, пункт 2).
//
// Проверяется ПРОВОДКА переключателя узла раскладки: WebUI/оператор меняет
// `rpcServerEnabled` через PUT /api/v1/cppworker/config/update, и
//
//  1. состояние дочернего процесса меняется НА ХОДУ (пункт 2 требует
//     «переключение из WebUI»);
//  2. в ответе едет ФАКТ (rpcServer.status: enabled/running/listening/причина),
//     а не отправленное значение — иначе UI покажет «включено» при упавшем
//     процессе;
//  3. значение сохраняется как операторское переопределение и в этом качестве
//     имеет приоритет над env (проверяется на следующем старте конфига).
//
// Сам супервизор (реальный дочерний процесс, порт, падение) проверяется в
// internal/cppbackend/rpc_server_child_r123_test.go — здесь именно проводка.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// putConfigUpdate — PUT /config/update с телом.
func putConfigUpdate(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/config/update",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)
	return w
}

// rpcTestSetup — изоляция: свой каталог переопределений и свой супервизор.
func rpcTestSetup(t *testing.T, bin string) *cppbackend.Config {
	t.Helper()
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())

	prevChild := rpcServerChild
	prevConfig := currentConfig
	// R304 (2026-10-29): приказ об узле доносится и до ЖИВОГО бэкенда — иначе
	// правило «поднятый узел = донор» не действовало бы до пересоздания
	// контейнера (см. backend.SetRPCServerEnabled).
	//
	// ЗАЧЕМ ЗДЕСЬ ГАСИТЬ backend. Соседние тесты этого пакета оставляют пакетный
	// backend поднятым и не восстанавливают его (это их приём, а не недосмотр:
	// большинству обработчиков хватает «какой-то» бэкенд). Приказ, отправленный
	// этим тестом, начал бы менять СОСТОЯНИЕ чужого бэкенда — и следующие тесты
	// получали бы 409 «донор» на загрузке вместо своих кодов. Тест обязан
	// изолировать всё, что его обработчик меняет.
	prevBackend := backend
	backend = nil
	t.Cleanup(func() {
		if rpcServerChild != nil {
			_ = rpcServerChild.Stop()
		}
		rpcServerChild = prevChild
		currentConfig = prevConfig
		backend = prevBackend
	})

	cfg := &cppbackend.Config{
		Host:             "127.0.0.1",
		Port:             18091,
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   8192,
		DefaultBatchSize: 512,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    freeRPCPort(t),
		RPCServerBin:     bin,
	}
	currentConfig = cfg
	rpcServerChild = cppbackend.NewRPCServerChild(*cfg)
	return cfg
}

func freeRPCPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// altRPCPort — ДРУГОЙ валидный порт рядом с port.
//
// R221: раньше тесты брали `port+1000` и на Windows это регулярно выходило за
// 65535 (свободные порты выдаются из диапазона 49k-65k), порт становился
// недопустимым, обработчик его не применял — и тест падал с «смена порта НЕ
// пересоздала супервизор», хотя код был исправен. Тест обязан падать только на
// настоящей регрессии.
func altRPCPort(port int) int {
	if port+1000 <= 65535 {
		return port + 1000
	}
	return port - 1000
}

// TestR123_ConfigUpdate_TogglesRPCNodeOnTheFly — выключение и включение узла из
// API меняет состояние супервизора на ходу.
func TestR123_ConfigUpdate_TogglesRPCNodeOnTheFly(t *testing.T) {
	// Бинарь-заглушка: включаем/выключаем СОСТОЯНИЕ, не поднимая процесс.
	cfg := rpcTestSetup(t, "/nonexistent/rpc-server-r123")

	// Выключено (по умолчанию) — выключаем ещё раз: идемпотентно.
	w := putConfigUpdate(t, `{"rpcServerEnabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"applied"`) ||
		!strings.Contains(w.Body.String(), "rpcServerEnabled") {
		t.Errorf("в applied нет rpcServerEnabled: %s", w.Body.String())
	}

	// Включаем: процесс поднять нельзя (бинаря нет), но ЖЕЛАНИЕ и ПРИЧИНА видны.
	w = putConfigUpdate(t, `{"rpcServerEnabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		RPC *cppbackend.RPCServerStatus `json:"rpcServer"`
		Err string                      `json:"rpcServerError"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, w.Body.String())
	}
	if resp.RPC == nil {
		t.Fatalf("в ответе нет rpcServer — WebUI не покажет состояние узла: %s", w.Body.String())
	}
	if !resp.RPC.Enabled {
		t.Error("rpcServer.enabled = false после включения: желаемое состояние потеряно")
	}
	if resp.RPC.Running {
		t.Error("rpcServer.running = true при несуществующем бинаре — статус врёт")
	}
	if resp.RPC.LastError == "" && resp.Err == "" {
		t.Error("нет причины отказа: оператор не поймёт, почему узел не поднялся")
	}
	if cfg.RPCServerEnabled != true {
		t.Error("конфиг не обновлён: следующая загрузка узла не поднимется")
	}
}

// TestR123_ConfigUpdate_PersistsOverrideWithPriorityOverEnv — правка из WebUI
// переживает пересоздание контейнера и побеждает env.
func TestR123_ConfigUpdate_PersistsOverrideWithPriorityOverEnv(t *testing.T) {
	rpcTestSetup(t, "/nonexistent/rpc-server-r123")

	if w := putConfigUpdate(t, `{"rpcServerEnabled":true,"rpcServerPort":51052}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	// Имитируем старт контейнера: env говорит «выключено», файл оператора — «включено».
	t.Setenv("CPPWORKER_RPC_SERVER_ENABLED", "false")
	t.Setenv("CPPWORKER_RPC_SERVER_PORT", "50052")

	cfg := cppbackend.LoadConfigFromEnv()
	if cfg.RPCServerEnabled {
		t.Fatal("подготовка теста: env должен был выставить false")
	}
	applied := applyRuntimeOverrides(&cfg)

	if !appliedListContains(applied, "rpcServerEnabled") {
		t.Errorf("rpcServerEnabled не применён из файла переопределений (applied=%v)", applied)
	}
	if !cfg.RPCServerEnabled {
		t.Error("после пересоздания узел раскладки выключен, хотя оператор включал его из WebUI — " +
			"это тот же дефект «настройка не сохраняется», что был у idleUnloadMinutes")
	}
	if cfg.RPCServerPort != 51052 {
		t.Errorf("порт = %d, want 51052 (значение оператора, а не env 50052)", cfg.RPCServerPort)
	}
}

// TestR123_GetConfig_ReportsRPCNodeState — состояние узла видно и при обычном
// чтении конфига (его опрашивают балансер и WebUI), а не только в ответе на
// переключение: иначе после перезагрузки страницы оператор не увидел бы ничего.
func TestR123_GetConfig_ReportsRPCNodeState(t *testing.T) {
	rpcTestSetup(t, "/nonexistent/rpc-server-r123")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config", nil)
	w := httptest.NewRecorder()
	handleCppWorkerGetConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Config *cppbackend.Config          `json:"config"`
		RPC    *cppbackend.RPCServerStatus `json:"rpcServer"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.RPC == nil {
		t.Fatalf("в GET /config нет rpcServer — WebUI не покажет состояние узла: %s", w.Body.String())
	}
	if resp.RPC.Port == 0 || resp.RPC.Bin == "" {
		t.Errorf("в состоянии узла нет порта/бинарника: %+v", resp.RPC)
	}
	if resp.Config == nil || resp.Config.RPCServerBind == "" {
		t.Error("в конфиге нет настроек узла раскладки")
	}
}

// TestR123_ConfigUpdate_StopOnDisableReleasesPort — выключение обязано гасить
// процесс: иначе он останется держать порт и следующий старт не поднимется.
func TestR123_ConfigUpdate_StopOnDisableReleasesPort(t *testing.T) {
	cfg := rpcTestSetup(t, "/nonexistent/rpc-server-r123")
	// Супервизор «уже включён» — проверяем именно путь выключения.
	rpcServerChild = cppbackend.NewRPCServerChild(cppbackend.Config{
		RPCServerEnabled: false,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    cfg.RPCServerPort,
		RPCServerBin:     "/nonexistent/rpc-server-r123",
	})

	w := putConfigUpdate(t, `{"rpcServerEnabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	st := rpcServerChild.Status()
	if st.Running || st.Enabled {
		t.Errorf("после выключения: enabled=%v running=%v, want false/false", st.Enabled, st.Running)
	}
}
