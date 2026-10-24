//go:build llama_stub

// runtime_overrides_r133_test.go — R133 (2026-10-16): состав раскладки
// (кто донор) переживает пересоздание контейнера.
//
// ЖИВОЙ ДЕФЕКТ. В deployments/.env лежали CPPWORKER_RPC_ENABLE=true и
// CPPWORKER_RPC_SERVERS=192.0.2.10:50052. Балансер назначает доноров ролями и
// на своём проходе гасил раскладку у мастера (донор выключен) — но при КАЖДОМ
// пересоздании контейнера окружение снова включало раскладку на старый адрес.
// Оператор видел «настройка не сохраняется» и не понимал, какой параметр
// обязателен; хуже того, мастер снова начинал дозваниваться до выключенного узла.
//
// Тесты проверяют тот же контракт, что уже принят для idleUnloadMinutes и
// rpcServerEnabled: значение из файла операторских переопределений читается
// ПОСЛЕ окружения и потому побеждает его.
package main

import (
	"net/http"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestR133_ConfigUpdatePersistsLayoutState — PUT раскладки сохраняется в файл и
// после «пересоздания» (перечитывания env) берётся из файла, а не из env.
func TestR133_ConfigUpdatePersistsLayoutState(t *testing.T) {
	rpcTestSetup(t, "/nonexistent/rpc-server-r133")

	if w := putConfigUpdate(t, `{"rpcEnabled":true,"rpcServers":["192.0.2.10:50052"]}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	// Пересоздание контейнера: окружение снова указывает на СТАРЫЙ адрес.
	t.Setenv("CPPWORKER_RPC_ENABLE", "false")
	t.Setenv("CPPWORKER_RPC_SERVERS", "198.51.100.9:50052")

	cfg := cppbackend.LoadConfigFromEnv()
	applied := applyRuntimeOverrides(&cfg)

	if !appliedListContains(applied, "rpcEnabled") || !appliedListContains(applied, "rpcServers") {
		t.Fatalf("состояние раскладки не применено из файла (applied=%v)", applied)
	}
	if !cfg.RPCEnabled {
		t.Error("раскладка выключилась, хотя сохранённое состояние — включено")
	}
	if len(cfg.RPCServers) != 1 || cfg.RPCServers[0] != "192.0.2.10:50052" {
		t.Errorf("список доноров = %v, want [192.0.2.10:50052] (адрес из файла, а не из env)", cfg.RPCServers)
	}
}

// TestR133_DisabledLayoutStaysDisabledAfterRecreate — обратный и самый живой
// случай: балансер выключил раскладку (донор недоступен), и после пересоздания
// контейнера она НЕ включается обратно из env.
func TestR133_DisabledLayoutStaysDisabledAfterRecreate(t *testing.T) {
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())
	recordRuntimeOverride("rpcEnabled", false)
	recordRuntimeOverride("rpcServers", []string{})

	t.Setenv("CPPWORKER_RPC_ENABLE", "true")
	t.Setenv("CPPWORKER_RPC_SERVERS", "192.0.2.10:50052")

	cfg := cppbackend.LoadConfigFromEnv()
	if !cfg.RPCEnabled || len(cfg.RPCServers) == 0 {
		t.Fatal("подготовка теста: env обязан включить раскладку с донором")
	}

	applied := applyRuntimeOverrides(&cfg)
	if !appliedListContains(applied, "rpcEnabled") {
		t.Errorf("rpcEnabled не применён из файла (applied=%v)", applied)
	}
	if cfg.RPCEnabled {
		t.Error("выключенная раскладка снова включилась из env — это и есть дефект «каждое пересоздание " +
			"возвращает старого донора»")
	}
	if len(cfg.RPCServers) != 0 {
		t.Errorf("список доноров вернулся из env: %v (пустой список в файле — значимое состояние)", cfg.RPCServers)
	}
}

// TestR133_BrokenDonorListInFileIsIgnored — битый список в файле не должен
// подменять рабочий список из окружения.
func TestR133_BrokenDonorListInFileIsIgnored(t *testing.T) {
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())
	recordRuntimeOverride("rpcServers", []string{"не-адрес"})

	t.Setenv("CPPWORKER_RPC_ENABLE", "true")
	t.Setenv("CPPWORKER_RPC_SERVERS", "192.0.2.10:50052")

	cfg := cppbackend.LoadConfigFromEnv()
	applied := applyRuntimeOverrides(&cfg)

	if appliedListContains(applied, "rpcServers") {
		t.Errorf("битый список из файла принят за рабочий (applied=%v)", applied)
	}
	if len(cfg.RPCServers) != 1 || cfg.RPCServers[0] != "192.0.2.10:50052" {
		t.Errorf("рабочий список из env потерян: %v", cfg.RPCServers)
	}
}

// TestR133_NoOverrideLeavesEnvAlone — если файла нет, поведение прежнее: env
// решает (обратная совместимость для стендов без балансерских ролей).
func TestR133_NoOverrideLeavesEnvAlone(t *testing.T) {
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())
	t.Setenv("CPPWORKER_RPC_ENABLE", "true")
	t.Setenv("CPPWORKER_RPC_SERVERS", "192.0.2.10:50052")

	cfg := cppbackend.LoadConfigFromEnv()
	applied := applyRuntimeOverrides(&cfg)

	if appliedListContains(applied, "rpcEnabled") || appliedListContains(applied, "rpcServers") {
		t.Errorf("без файла переопределений раскладка не должна переопределяться (applied=%v)", applied)
	}
	if !cfg.RPCEnabled || len(cfg.RPCServers) != 1 {
		t.Errorf("env-значения раскладки потеряны: enabled=%v servers=%v", cfg.RPCEnabled, cfg.RPCServers)
	}
}
