//go:build llama_stub

// unload_loading_guard_r83_test.go — R83 §9.6 D-C (2026-09-26).
//
// Симптом: unload во время загрузки 16 GB модели отвечал 200 мгновенно
// (проверялись только активные inference-запросы), модель удалялась из реестра,
// а через минуты возвращалась туда фоновой загрузкой — «model loaded в логе,
// /api/models → count=0».
//
// Теперь unload во время загрузки возвращает 409 и подсказывает ?force=true.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestR83_UnloadDuringLoad_Conflict — пока модель грузится, unload обязан
// вернуть 409, а не 200/404.
func TestR83_UnloadDuringLoad_Conflict(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	name := "r83-loading-model"
	// Занимаем «загрузку» так же, как это делает LoadModelWithOpts.
	if ok, err := backend.TryLockLoad(name); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoad = (%v, %v)", ok, err)
	}
	defer backend.UnlockLoad(name)

	if !backend.IsLoading(name) {
		t.Fatal("IsLoading не видит активную загрузку — guard D-C не сработает")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/unload?name="+name, nil)
	handleUnloadModel(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("unload во время загрузки: HTTP %d, want %d (body=%s)",
			rec.Code, http.StatusConflict, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"currently loading", "force=true"} {
		if !strings.Contains(body, want) {
			t.Errorf("в ответе нет %q: %s", want, body)
		}
	}
}

// TestR83_UnloadNotLoading_NoGuard — когда загрузки нет, guard не мешает
// обычной логике (модель не найдена → 404, а не 409).
func TestR83_UnloadNotLoading_NoGuard(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	if backend.IsLoading("never-loading-model") {
		t.Fatal("IsLoading=true для модели, которую никто не грузит")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/unload?name=never-loading-model", nil)
	handleUnloadModel(rec, req)

	if rec.Code == http.StatusConflict {
		t.Errorf("без активной загрузки получен 409 — guard сработал ложно (body=%s)", rec.Body.String())
	}
}

// TestR83_IsLoading_TracksLockLifecycle — реестр загрузок ведётся корректно:
// TryLockLoad → true, UnlockLoad → false.
func TestR83_IsLoading_TracksLockLifecycle(t *testing.T) {
	b := cppbackend.NewBackend(cppbackend.Config{ModelsDir: t.TempDir()})
	name := "lifecycle-model"

	if b.IsLoading(name) {
		t.Fatal("IsLoading=true до TryLockLoad")
	}
	if ok, err := b.TryLockLoad(name); !ok || err != nil {
		t.Fatalf("TryLockLoad = (%v, %v)", ok, err)
	}
	if !b.IsLoading(name) {
		t.Error("IsLoading=false после TryLockLoad")
	}
	b.UnlockLoad(name)
	if b.IsLoading(name) {
		t.Error("IsLoading=true после UnlockLoad")
	}
}
