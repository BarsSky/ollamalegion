// queue_move_gate_r480_test.go — R480 (P1-7): порог гейта очереди — окно
// ТЕКУЩЕЙ КОПИИ сессии, а не потребность запроса.
//
// Первый вариант сравнивал кандидата с потребностью запроса. Если она больше
// ВСЕХ окон в кластере (большой промпт на маленьких копиях), гейт отклонял
// всех, и запрос висел в очереди до 900 с вместо того, чтобы уйти на свою
// копию и дать preflight поднять окно. CI ловил это таймаутом пакета.
package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR480_QueueMoveUsesSessionWindowNotRequestNeed — сессия на b2 (окно
// 32768), b2 занят: очередь вправе взять b1 (65536), потому что он НЕ хуже.
func TestR480_QueueMoveUsesSessionWindowNotRequestNeed(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r480SelectorProxy(t)
	p.setAdmissionWait(2 * time.Second)
	// b2 занят: один слот, уже занят (иначе у него 10 свободных слотов).
	p.backends["b2"].Backend.MaxConcurrentReqs = 1
	p.backends["b2"].ActiveReqs = 1

	id, release, _, _, err := p.waitForInferenceBackendWithMoveR480(
		context.Background(), r459Model, types.BackendTypeLlamaCpp, "s1", "b2")
	if err != nil {
		t.Fatalf("переезд на b1 (окно больше) обязан быть разрешён, получено %v", err)
	}
	if id != "b1" {
		t.Fatalf("получен %q, ожидался b1", id)
	}
	if release != nil {
		release()
	}
}

// TestR480_QueueMoveRejectsNarrowerCopy — сессия на b1 (65536), b1 занят:
// b2 (32768) уже, переезд запрещён → ждём слот b1 до таймаута.
func TestR480_QueueMoveRejectsNarrowerCopy(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r480SelectorProxy(t)
	p.setAdmissionWait(200 * time.Millisecond)
	p.backends["b1"].Backend.MaxConcurrentReqs = 1
	p.backends["b1"].ActiveReqs = 1

	_, _, _, _, err := p.waitForInferenceBackendWithMoveR480(
		context.Background(), r459Model, types.BackendTypeLlamaCpp, "s1", "b1")
	if err != errAdmissionTimeout {
		t.Fatalf("ожидался errAdmissionTimeout (ждём свою копию), получено %v", err)
	}
}

// TestR480_QueueMoveNoGateWhenWindowUnknown — окно текущей копии неизвестно:
// сравнивать не с чем, гейт не ставится (запрос обслуживается как раньше).
func TestR480_QueueMoveNoGateWhenWindowUnknown(t *testing.T) {
	p := r480SelectorProxy(t)
	p.setAdmissionWait(2 * time.Second)
	// Окно "b9" неизвестно: записи в llamaMetrics нет вовсе.
	id, release, _, _, err := p.waitForInferenceBackendWithMoveR480(
		context.Background(), r459Model, types.BackendTypeLlamaCpp, "s1", "b9")
	if err != nil {
		t.Fatalf("без известного окна очередь обязана обслужить запрос, получено %v", err)
	}
	if id == "" {
		t.Fatal("пустой бэкенд при неизвестном окне")
	}
	if release != nil {
		release()
	}
}
