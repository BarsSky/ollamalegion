//go:build llama_stub

// phase6_hygiene_r65d_test.go — R65d (2026-09-20): гигиена P3.
//
// Покрывает два дефекта из аудита 2026-09-20:
//
//  1. MetricsBroker молча терял данные: оба `default:` в publish-путях
//     отбрасывали метрики при заполненном канале (медленный WebSocket-клиент,
//     всплеск метрик) — без счётчика, без лога. Оператор видел «застывший»
//     дашборд и не мог понять, что данные теряются на стороне балансера.
//
//  2. getUserID использовал RemoteAddr сразу после X-User-Id. cppworker стоит
//     ЗА балансером, поэтому RemoteAddr — это адрес балансера, одинаковый для
//     всех клиентов: при MaxParallelPerUser > 0 все клиенты без X-User-Id
//     попадали в один бакет и делили лимит.
package main

import (
	"net/http/httptest"
	"testing"
)

// TestR65d_GetUserID_PrefersXRealIPOverRemoteAddr — при отсутствии X-User-Id
// идентификатор берётся из X-Real-IP (его проставляет балансер), а НЕ из
// RemoteAddr (это адрес самого балансера).
func TestR65d_GetUserID_PrefersXRealIPOverRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/chat", nil)
	// RemoteAddr = адрес балансера (одинаковый для всех клиентов).
	r.RemoteAddr = "172.18.0.5:54321"
	// X-Real-IP = реальный IP клиента.
	r.Header.Set("X-Real-IP", "203.0.113.77")

	if got := getUserID(r); got != "203.0.113.77" {
		t.Errorf("getUserID = %q, want 203.0.113.77 "+
			"(RemoteAddr — это адрес балансера, все клиенты схлопывались в один бакет)", got)
	}
}

// TestR65d_GetUserID_UsesFirstXForwardedFor — если X-Real-IP нет, берём первого
// клиента из X-Forwarded-For, а не адрес балансера.
func TestR65d_GetUserID_UsesFirstXForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 172.18.0.5")

	if got := getUserID(r); got != "198.51.100.9" {
		t.Errorf("getUserID = %q, want 198.51.100.9 (первый адрес в XFF)", got)
	}
}

// TestR65d_GetUserID_UserIDHeaderWins — явный X-User-Id остаётся высшим
// приоритетом.
func TestR65d_GetUserID_UserIDHeaderWins(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Real-IP", "203.0.113.77")
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	r.Header.Set("X-User-Id", "alice")

	if got := getUserID(r); got != "alice" {
		t.Errorf("getUserID = %q, want alice (X-User-Id имеет высший приоритет)", got)
	}
}

// TestR65d_GetUserID_FallsBackToRemoteAddr — прямое подключение без заголовков
// по-прежнему работает через RemoteAddr.
func TestR65d_GetUserID_FallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = "192.0.2.11:40000"

	if got := getUserID(r); got != "192.0.2.11" {
		t.Errorf("getUserID = %q, want 192.0.2.11 (fallback на RemoteAddr без порта)", got)
	}
}

// TestR65d_GetUserID_DistinctClientsDistinctBuckets — главный смысл фикса:
// разные клиенты за одним балансером должны получать РАЗНЫЕ идентификаторы,
// иначе MaxParallelPerUser работает как глобальный лимит.
func TestR65d_GetUserID_DistinctClientsDistinctBuckets(t *testing.T) {
	mk := func(realIP string) string {
		r := httptest.NewRequest("POST", "/api/chat", nil)
		// Один и тот же балансер для всех запросов.
		r.RemoteAddr = "172.18.0.5:54321"
		r.Header.Set("X-Real-IP", realIP)
		return getUserID(r)
	}

	a := mk("203.0.113.1")
	b := mk("203.0.113.2")
	if a == b {
		t.Errorf("два разных клиента получили одинаковый user_id %q — "+
			"MaxParallelPerUser будет считать их одним пользователем", a)
	}
	if a == "172.18.0.5" || b == "172.18.0.5" {
		t.Errorf("user_id совпал с адресом балансера (%q/%q) — "+
			"RemoteAddr не должен использоваться при наличии X-Real-IP", a, b)
	}
}
