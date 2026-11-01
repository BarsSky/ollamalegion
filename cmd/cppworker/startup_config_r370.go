// startup_config_r370.go — R370 (2026-11-01): раскладка памяти на старте:
// сначала окружение, потом операторские переопределения.
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд). Балансер назначил роли (main + donor) и
// прислал мастеру раскладку — всё работает: модель грузится со слоями на RPC0.
// Но после пересоздания контейнера воркера (`docker compose up -d` после сборки
// нового образа) раскладка МОЛЧА пропадала:
//
//	overrides: {"backendRole":"main","rpcEnabled":true,"rpcServers":["100.64.0.7:50052"]}
//	лог:       "applied runtime overrides (operator values win over env)"
//	/api/models: rpc_enabled=false, rpc_servers_active=[]
//	в логе загрузки: НИ ОДНОЙ RPC-строки — модель поднялась только на локальной карте
//
// Причина — порядок. Файл переопределений применялся ПЕРВЫМ (это правильно), но
// затем старт заново читал `CPPWORKER_RPC_ENABLE` / `CPPWORKER_RPC_SERVERS`
// напрямую из окружения и перезаписывал `cfg.RPCEnabled`/`cfg.RPCServers` их
// значениями. В compose обе переменные пустые (`CPPWORKER_RPC_ENABLE=`), то есть
// решение — «выключено, узлов нет», и оно затирало приказ балансера. Ровно тот
// класс, от которого защищались R119/R133: «настройка сохраняется без эффекта».
//
// ЧТО ДЕЛАЕМ. Решение по окружению — это НАМЕРЕНИЕ (база), операторские
// переопределения и приказ балансера — ФАКТ (последний слой). Поэтому
// `resolveRPCMode(env)` считается до `applyRuntimeOverrides`, а не после.
package main

import "ollama-loadbalancer/internal/cppbackend"

// applyStartupLayoutConfigR370 — собрать раскладку на старте в правильном
// порядке и вернуть решение по env, применённые override-ключи.
//
// getenv передаётся параметром, чтобы правило можно было проверить тестом без
// подмены окружения процесса.
func applyStartupLayoutConfigR370(cfg *cppbackend.Config, getenv func(string) string) (rpcMode, []string) {
	if cfg == nil {
		return rpcMode{}, nil
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	// Слой 1: окружение.
	mode := resolveRPCMode(getenv("CPPWORKER_RPC_ENABLE"), getenv("CPPWORKER_RPC_SERVERS"))
	cfg.RPCEnabled = mode.Enabled
	cfg.RPCServers = mode.Servers
	// Слой 2: оператор/балансер (файл переопределений) — поверх env.
	applied := applyRuntimeOverrides(cfg)
	return mode, applied
}
