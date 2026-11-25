//go:build llama_stub

// profile_reload_unset_params_r757_test.go — R757 (2026-11-25): «не задано» в
// профиле НЕ отправляется как ноль.
//
// ЖИВОЙ ДЕФЕКТ (архив второго стенда, 06:17:50). После ответа модель
// перезагружалась «по профилю», в котором BatchSize не был задан (0). Балансер
// отправлял `batchSize: 0`, cppworker отвечал
//
//	400 {"error":"batchSize must be >= 1"}
//
// поэтому перезагрузка не происходила вовсе:
//   - старый экземпляр оставался занят (`unload refused: model has active
//     requests active_queries=1`), клиент получал 502;
//   - модель уходила из памяти только полной остановкой контейнера — ровно то,
//     что оператор описал как «после ответа произошёл сбой по перезагрузке
//     такой модели и только полная остановка выгрузила модель из памяти».
//
// ЧТО ФИКСИРУЕТСЯ:
//  1. при BatchSize=0 и ContextLength=0 в теле reload этих ключей НЕТ (воркер
//     решает по своим дефолтам);
//  2. заданные значения по-прежнему доезжают (правка не должна выключить
//     применение профиля);
//  3. отрицательные значения тоже не отправляются: у cppworker это ошибка
//     валидации, а не «авто».
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR757_ReloadOmitsUnsetContextAndBatch — нули в профиле не уходят в тело.
func TestR757_ReloadOmitsUnsetContextAndBatch(t *testing.T) {
	srv, captured := mockReloadCapture(t)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{ID: "b1", Host: host, CppWorkerPort: port}

	// Профиль ровно как в живом архиве: размер батча не задан (0).
	if _, err := s.reloadModelOnCppWorker(backend, "Inkling-2-Mini-UD-Q4_K_M", types.LlamaCppModelProfile{
		NumGPULayers:  24,
		ContextLength: 0,
		BatchSize:     0,
	}); err != nil {
		t.Fatalf("reloadModelOnCppWorker: %v", err)
	}
	if *captured == nil {
		t.Fatal("reload не был вызван")
	}
	if v, ok := (*captured)["batchSize"]; ok {
		t.Fatalf("в теле reload есть batchSize=%v, want отсутствие ключа: cppworker "+
			"отвечает 400 «batchSize must be >= 1» и перезагрузка НЕ происходит "+
			"(живой дефект второго стенда: модель уходила из памяти только полной остановкой)", v)
	}
	if v, ok := (*captured)["contextSize"]; ok {
		t.Fatalf("в теле reload есть contextSize=%v, want отсутствие ключа: 0 в профиле "+
			"означает «не задано», воркер подставляет свой дефолт", v)
	}
	// gpuLayers — законный ноль (все слои на CPU), его отправлять можно.
	if v, ok := (*captured)["gpuLayers"]; !ok || int(v.(float64)) != 24 {
		t.Errorf("gpuLayers = %v, want 24: правка не должна ломать передачу слоёв", v)
	}
}

// TestR757_ReloadKeepsExplicitValues — заданные значения доезжают как раньше.
func TestR757_ReloadKeepsExplicitValues(t *testing.T) {
	srv, captured := mockReloadCapture(t)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{ID: "b1", Host: host, CppWorkerPort: port}

	if _, err := s.reloadModelOnCppWorker(backend, "Qwen3.8-27B-UD-Q4_K_M", types.LlamaCppModelProfile{
		ContextLength: 32768,
		BatchSize:     512,
		NumGPULayers:  48,
	}); err != nil {
		t.Fatalf("reloadModelOnCppWorker: %v", err)
	}
	if *captured == nil {
		t.Fatal("reload не был вызван")
	}
	if v, ok := (*captured)["batchSize"]; !ok || int(v.(float64)) != 512 {
		t.Errorf("batchSize = %v, want 512", v)
	}
	if v, ok := (*captured)["contextSize"]; !ok || int(v.(float64)) != 32768 {
		t.Errorf("contextSize = %v, want 32768", v)
	}
}

// TestR757_ReloadOmitsNegativeValues — отрицательное не «авто», а ошибка.
func TestR757_ReloadOmitsNegativeValues(t *testing.T) {
	srv, captured := mockReloadCapture(t)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{ID: "b1", Host: host, CppWorkerPort: port}

	if _, err := s.reloadModelOnCppWorker(backend, "m", types.LlamaCppModelProfile{
		ContextLength: -1,
		BatchSize:     -1,
	}); err != nil {
		t.Fatalf("reloadModelOnCppWorker: %v", err)
	}
	if v, ok := (*captured)["batchSize"]; ok {
		t.Errorf("в теле reload есть batchSize=%v, want отсутствие ключа", v)
	}
	if v, ok := (*captured)["contextSize"]; ok {
		t.Errorf("в теле reload есть contextSize=%v, want отсутствие ключа", v)
	}
}
