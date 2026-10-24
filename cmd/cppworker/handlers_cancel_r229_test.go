//go:build llama_stub

// handlers_cancel_r229_test.go — R229 (2026-10-22): область отмены и диагностика.
//
// ЖИВОЙ ДЕФЕКТ, который здесь закрыт. Балансер присылает отмену с ОДНИМ полем
// model (кнопка Stop в OpenWebUI). Прежний switch такой случай не разбирал и
// попадал в default «отменить ВСЕ»: Stop на модели A гасил и генерацию модели B на
// том же воркере. Тест фиксирует:
//   - model без userID отменяет только генерации этой модели;
//   - userID+model — только этого пользователя на этой модели;
//   - request_id, которого нет, — явный признак «не найдено» (404 в хендлере);
//   - пустое тело — админский сброс всех генераций.
package main

import (
	"context"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

type cancelReq = struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
}

func TestR229_CancelScopeModelOnlyDoesNotCancelOtherModel(t *testing.T) {
	tracker := cppbackend.NewActiveGenerations()
	cancelledA := false
	cancelledB := false
	tracker.Add("req-a", "alice", "modelA", "b1", func() { cancelledA = true })
	tracker.Add("req-b", "bob", "modelB", "b1", func() { cancelledB = true })

	n, by, missing := applyCancelScope(tracker, cancelReq{Model: "modelA"})
	if missing {
		t.Fatal("model-only не должен считаться «id не найден»")
	}
	if by != "model" || n != 1 {
		t.Fatalf("cancelled=%d by=%q, want 1/model", n, by)
	}
	if !cancelledA {
		t.Error("генерация запрошенной модели не отменена")
	}
	if cancelledB {
		t.Error("Stop на modelA погасил генерацию modelB — это и был живой дефект")
	}
}

func TestR229_CancelScopeUserAndModelFiltersUser(t *testing.T) {
	tracker := cppbackend.NewActiveGenerations()
	alice := false
	bob := false
	tracker.Add("r1", "alice", "m", "b1", func() { alice = true })
	tracker.Add("r2", "bob", "m", "b1", func() { bob = true })

	n, by, _ := applyCancelScope(tracker, cancelReq{UserID: "alice", Model: "m"})
	if by != "model" || n != 1 {
		t.Fatalf("cancelled=%d by=%q, want 1/model", n, by)
	}
	if !alice || bob {
		t.Errorf("фильтр по пользователю не сработал: alice=%v bob=%v", alice, bob)
	}
}

func TestR229_CancelScopeUnknownRequestID(t *testing.T) {
	tracker := cppbackend.NewActiveGenerations()
	n, by, missing := applyCancelScope(tracker, cancelReq{RequestID: "nope"})
	if !missing {
		t.Fatal("неизвестный request_id должен давать missing=true (хендлер ответит 404)")
	}
	if n != 0 || by != "id" {
		t.Errorf("cancelled=%d by=%q, want 0/id", n, by)
	}
}

func TestR229_CancelScopeEmptyBodyCancelsAll(t *testing.T) {
	tracker := cppbackend.NewActiveGenerations()
	a := false
	b := false
	tracker.Add("r1", "alice", "m1", "b1", func() { a = true })
	tracker.Add("r2", "bob", "m2", "b1", func() { b = true })

	n, by, _ := applyCancelScope(tracker, cancelReq{})
	if by != "all" || n != 2 {
		t.Fatalf("cancelled=%d by=%q, want 2/all", n, by)
	}
	if !a || !b {
		t.Errorf("админский сброс не отменил всё: a=%v b=%v", a, b)
	}
}

var _ = context.CancelFunc(nil)
