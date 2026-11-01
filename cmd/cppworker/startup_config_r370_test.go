// startup_config_r370_test.go — R370 (2026-11-01).
//
// Правило: раскладка памяти на старте собирается в порядке
//
//	окружение (намерение) → файл переопределений (факт: приказ балансера/WebUI)
//
// и переопределения ПОБЕЖДАЮТ. Живой дефект, из которого правило выросло:
// балансер назначил роли main+donor и прислал мастеру раскладку, но после
// пересоздания контейнера старт заново читал CPPWORKER_RPC_ENABLE/
// CPPWORKER_RPC_SERVERS (в compose они пустые) и затирал приказ —
// `/api/models` показывал rpc_enabled=false, в логе загрузки не было ни одной
// RPC-строки, то есть двухузловой стенд молча становился однопузловым.
package main

import (
	"go/ast"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// writeOverridesR370 — файл переопределений оператора в изолированном каталоге.
func writeOverridesR370(t *testing.T, overrides map[string]interface{}) {
	t.Helper()
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())
	encoded := map[string]interface{}{}
	for k, v := range overrides {
		encoded[k] = v
	}
	for k, v := range encoded {
		recordRuntimeOverride(k, v)
	}
}

// envMapR370 — getenv из карты (без подмены окружения процесса).
func envMapR370(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestR370_OverridesWinOverEmptyEnv — боевой случай стенда: env пустой,
// переопределения включают раскладку.
func TestR370_OverridesWinOverEmptyEnv(t *testing.T) {
	writeOverridesR370(t, map[string]interface{}{
		"backendRole": "main",
		"rpcEnabled":  true,
		"rpcServers":  []string{"100.64.0.7:50052"},
	})

	cfg := &cppbackend.Config{}
	_, applied := applyStartupLayoutConfigR370(cfg, envMapR370(map[string]string{}))

	if !cfg.RPCEnabled {
		t.Fatalf("rpc_enabled=false после старта, хотя переопределения его включают "+
			"(applied=%v) — это и есть живой дефект R370: приказ балансера затирался пустым env", applied)
	}
	if len(cfg.RPCServers) != 1 || cfg.RPCServers[0] != "100.64.0.7:50052" {
		t.Fatalf("rpc_servers=%v, want [100.64.0.7:50052]", cfg.RPCServers)
	}
}

// TestR370_OverridesOffWinOverEnvOn — обратная сторона: оператор выключил
// раскладку, хотя в env она включена. Факт главнее намерения.
func TestR370_OverridesOffWinOverEnvOn(t *testing.T) {
	writeOverridesR370(t, map[string]interface{}{
		"rpcEnabled": false,
		"rpcServers": []string{},
	})

	cfg := &cppbackend.Config{}
	applyStartupLayoutConfigR370(cfg, envMapR370(map[string]string{
		"CPPWORKER_RPC_ENABLE":  "true",
		"CPPWORKER_RPC_SERVERS": "192.0.2.10:50052",
	}))

	if cfg.RPCEnabled {
		t.Error("rpc_enabled=true, хотя оператор выключил раскладку")
	}
	if len(cfg.RPCServers) != 0 {
		t.Errorf("rpc_servers=%v, want пусто: при выключении список не должен всплывать", cfg.RPCServers)
	}
}

// TestR370_EnvWorksWithoutOverrides — без файла переопределений поведение
// прежнее: env включает раскладку и задаёт узлы.
func TestR370_EnvWorksWithoutOverrides(t *testing.T) {
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir()) // пусто: файла нет

	cfg := &cppbackend.Config{}
	mode, applied := applyStartupLayoutConfigR370(cfg, envMapR370(map[string]string{
		"CPPWORKER_RPC_ENABLE":  "true",
		"CPPWORKER_RPC_SERVERS": "192.0.2.11:50052,192.0.2.12:50052",
	}))

	if !cfg.RPCEnabled || len(cfg.RPCServers) != 2 {
		t.Fatalf("env-раскладка не применилась: enabled=%v servers=%v", cfg.RPCEnabled, cfg.RPCServers)
	}
	if !mode.Enabled || len(applied) != 0 {
		t.Errorf("решение по env=%v, применённые overrides=%v (файла нет)", mode.Enabled, applied)
	}
}

// TestR370_EnvExplicitOffStillWinsWithoutOverrides — CPPWORKER_RPC_ENABLE=false
// обязан выключать раскладку, даже если список узлов задан (R115).
func TestR370_EnvExplicitOffStillWinsWithoutOverrides(t *testing.T) {
	t.Setenv("CPPWORKER_CONFIG_DIR", t.TempDir())

	cfg := &cppbackend.Config{}
	applyStartupLayoutConfigR370(cfg, envMapR370(map[string]string{
		"CPPWORKER_RPC_ENABLE":  "false",
		"CPPWORKER_RPC_SERVERS": "192.0.2.10:50052",
	}))

	if cfg.RPCEnabled {
		t.Error("CPPWORKER_RPC_ENABLE=false не выключил раскладку")
	}
	if len(cfg.RPCServers) != 0 {
		t.Errorf("rpc_servers=%v, want пусто при явном выключении", cfg.RPCServers)
	}
}

// TestR370_OtherOverridesStillApply — R370 не должен сломать остальные
// переопределения (idleUnloadMinutes — исходный повод R119).
func TestR370_OtherOverridesStillApply(t *testing.T) {
	writeOverridesR370(t, map[string]interface{}{
		"idleUnloadMinutes": 3,
		"rpcEnabled":        true,
		"rpcServers":        []string{"100.64.0.7:50052"},
	})

	cfg := &cppbackend.Config{IdleUnloadMinutes: 10}
	applyStartupLayoutConfigR370(cfg, envMapR370(map[string]string{}))

	if cfg.IdleUnloadMinutes != 3 {
		t.Errorf("idleUnloadMinutes=%d, want 3 (операторское значение)", cfg.IdleUnloadMinutes)
	}
}

// TestR370STEWARD_MainUsesStartupHelper — стюард: main обязан собирать
// раскладку общим помощником и НЕ звать resolveRPCMode напрямую (иначе решение
// по env снова окажется после переопределений и затрёт приказ балансера).
func TestR370STEWARD_MainUsesStartupHelper(t *testing.T) {
	fn, pos := findFuncR365(t, parsePackageFilesR365(t), "main")
	calls := callNamesR364(fn)

	if !calls["applyStartupLayoutConfigR370"] {
		t.Errorf("%s: main не зовёт applyStartupLayoutConfigR370 — порядок «env → переопределения» "+
			"перестал быть общим правилом (R370)", pos)
	}
	if calls["resolveRPCMode"] {
		t.Errorf("%s: main зовёт resolveRPCMode напрямую.\n"+
			"    Именно так приказ балансера (файл переопределений) затирался пустым окружением:\n"+
			"    двухузловой стенд после пересоздания контейнера молча становился однопузловым.\n"+
			"    Решение по env должно приниматься внутри applyStartupLayoutConfigR370.", pos)
	}
}

// TestR370STEWARD_HelperOrder — стюард читает сам помощник: сначала env-решение,
// потом переопределения. Порядок строк в AST — единственное, что защищает от
// возврата дефекта «переопределения применил, env перечитал».
func TestR370STEWARD_HelperOrder(t *testing.T) {
	fn, pos := findFuncR365(t, parsePackageFilesR365(t), "applyStartupLayoutConfigR370")

	var posResolve, posOverride ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "resolveRPCMode":
				if posResolve == nil {
					posResolve = call
				}
			case "applyRuntimeOverrides":
				if posOverride == nil {
					posOverride = call
				}
			}
		}
		return true
	})
	if posResolve == nil || posOverride == nil {
		t.Fatalf("%s: в помощнике нет resolveRPCMode/applyRuntimeOverrides — тест проверяет не то", pos)
	}
	if posResolve.Pos() > posOverride.Pos() {
		t.Errorf("%s: resolveRPCMode вызывается ПОСЛЕ applyRuntimeOverrides — "+
			"решение по env снова затрёт приказ балансера (живой дефект R370)", pos)
	}
}
