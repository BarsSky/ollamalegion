// reload_dedup_r69_test.go — R69 (2026-09-23): единый gate для всех триггеров
// reload'а.
//
// ПРОБЛЕМА (логи живого стенда):
//
//	reload rollback failed (model is no longer loaded!)
//	  rollback_error: "load model gemma-4-E4B-it-Q4_K_M: load cancelled:
//	   model load aborted by user (R60.57 checkpoint A: after llama_model_load_from_file)"
//	POST /api/models/reload → 500 за 2m49s
//
// У балансера было ТРИ независимых лаунчера загрузки одной модели:
//  1. preflight async reload (reloadDedup по ключу backend+model+target);
//  2. R60.47-обработчик 413 от cppworker (раньше вообще без дедупликации);
//  3. R60.35/44 executeAsyncReload (прямой POST /api/models/load, тоже без неё).
//
// cppworker не умеет вести две загрузки одной модели: вторую он отменяет
// (BRIDGE_ERR_ABORTED), rollback после отмены падает, и модель остаётся
// ВЫГРУЖЕННОЙ — клиент получает 503 на каждый следующий запрос.
//
// ФИКС: StartModelReloadIfNotPending — ключ только (backend, model), target не
// важен; проверяет любой уже идущий reload этой модели (IsReloadPending) и не
// запускает второй.
package balancer

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestStartModelReload_R69_BlocksParallelTriggers — пока загрузка идёт, второй
// и третий триггеры (в т.ч. R60.47-путь) не запускают новую.
func TestStartModelReload_R69_BlocksParallelTriggers(t *testing.T) {
	p := admissionTestProxy(t, 4)
	const modelName = "gemma"

	started := make(chan struct{})
	release := make(chan struct{})
	if !p.nctxReload.StartModelReloadIfNotPending("llama_adm", modelName, func(_ *reloadEntry) {
		close(started)
		<-release
	}) {
		t.Fatal("первый reload должен запуститься")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("первый reload не стартовал")
	}

	// Загрузка «идёт» — любой второй триггер обязан получить false.
	if p.nctxReload.StartModelReloadIfNotPending("llama_adm", modelName, func(_ *reloadEntry) {
		t.Error("второй reload не должен запускаться, пока идёт первый")
	}) {
		t.Error("StartModelReloadIfNotPending вернул true при активном reload")
	}

	// R60.47-путь (обработчик 413 от cppworker) — тот же gate.
	plan := &ReloadPlan{Decision: DecisionReload, NewNCtx: 65536, Reason: "test"}
	rec := httptest.NewRecorder()
	p.handleNCtxReloadActualAsync("llama_adm", modelName, plan, "http://127.0.0.1:1", nil, rec)
	if rec.Code != 503 {
		t.Errorf("R60.47-путь должен ответить 503 клиенту, получено %d", rec.Code)
	}

	close(release)
	// Дожидаемся снятия записи из реестра (закрытие done происходит после delete).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !p.nctxReload.IsReloadPending("llama_adm", modelName) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.nctxReload.IsReloadPending("llama_adm", modelName) {
		t.Fatal("запись о reload не снята после завершения")
	}
	// Gate снова свободен.
	done := make(chan struct{})
	if !p.nctxReload.StartModelReloadIfNotPending("llama_adm", modelName, func(_ *reloadEntry) {
		close(done)
	}) {
		t.Fatal("после завершения reload gate должен быть свободен")
	}
	<-done
}

// TestStartModelReload_R69_DifferentTargetsStillDedup — ключ не зависит от
// target: reload на 32768 блокирует запуск reload'а на 65536 для той же модели
// (cppworker всё равно отменил бы первую загрузку).
func TestStartModelReload_R69_DifferentTargetsStillDedup(t *testing.T) {
	c := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	block := make(chan struct{})
	started := make(chan struct{})
	if _, startedNew := c.reloadDedup.StartReloadIfNotPending("b1", "m", 32768, func(_ *reloadEntry) {
		close(started)
		<-block
	}); !startedNew {
		t.Fatal("первый reload должен зарегистрироваться")
	}
	<-started
	// Другой target — но та же модель: второй запуск запрещён.
	if c.StartModelReloadIfNotPending("b1", "m", func(_ *reloadEntry) {
		t.Error("второй reload (другой target) не должен запускаться")
	}) {
		t.Error("StartModelReloadIfNotPending не увидел reload с другим target")
	}
	// Другая модель — разрешено.
	otherDone := make(chan struct{})
	if !c.StartModelReloadIfNotPending("b1", "other-model", func(_ *reloadEntry) {
		close(otherDone)
	}) {
		t.Error("reload другой модели должен запускаться")
	}
	<-otherDone
	close(block)
}
