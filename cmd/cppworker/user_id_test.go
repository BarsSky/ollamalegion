package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetUserID_XUserId — header X-User-Id приоритетнее RemoteAddr.
func TestGetUserID_XUserId(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.Header.Set("X-User-Id", "alice")
	r.RemoteAddr = "192.168.1.1:54321"
	got := getUserID(r)
	if got != "alice" {
		t.Fatalf("expected alice, got %q", got)
	}
}

// TestGetUserID_RemoteAddr — header нет, RemoteAddr есть → IP без порта.
func TestGetUserID_RemoteAddr(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = "192.168.1.1:54321"
	got := getUserID(r)
	if got != "192.168.1.1" {
		t.Fatalf("expected 192.168.1.1, got %q", got)
	}
}

// TestGetUserID_RemoteAddrIPv6 — IPv6 адрес должен корректно отделяться от порта.
func TestGetUserID_RemoteAddrIPv6(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = "[::1]:54321"
	got := getUserID(r)
	if got != "::1" {
		t.Fatalf("expected ::1, got %q", got)
	}
}

// TestGetUserID_Anonymous — ничего нет → "anonymous".
func TestGetUserID_Anonymous(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.RemoteAddr = ""
	got := getUserID(r)
	if got != "anonymous" {
		t.Fatalf("expected anonymous, got %q", got)
	}
}

// TestGetUserID_Sanitize — control chars и длинные строки → sanitized.
func TestGetUserID_Sanitize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"control char", "alice\nbob", "alice_bob"},
		{"tab", "alice\tbob", "alice_bob"},
		{"NUL", "alice\x00bob", "alice_bob"},
		{"punctuation", "user!@#name", "user___name"}, // ! # заменяются
		{"truncate long", strings.Repeat("a", 200), strings.Repeat("a", 128)},
		{"all bad", "!@#", "anonymous"},       // всё trimmed → anonymous
		{"multi-byte", "user\u00e9", "user_"}, // é → _ (non-ASCII)
		{"keep allowed", "user-name.v2@host:1234 ok", "user-name.v2@host:1234 ok"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeUserID(c.input)
			if got != c.expect {
				t.Errorf("sanitize(%q) = %q, want %q", c.input, got, c.expect)
			}
		})
	}
}
