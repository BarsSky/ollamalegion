// log_buffer_r476_test.go — R476 (2026-11-07): псевдоним LB_DIAG_RING.
//
// Два имени на одну настройку — риск «задал одно, работает другое». Правило
// проверяется тестом: основная переменная главнее, псевдоним работает только
// когда основная не задана или задана мусором.
package main

import (
	"os"
	"testing"
)

// TestR476_LogBufferMainNameWins — LB_LOG_BUFFER_ENTRIES главнее LB_DIAG_RING.
func TestR476_LogBufferMainNameWins(t *testing.T) {
	t.Setenv("LB_LOG_BUFFER_ENTRIES", "123")
	t.Setenv("LB_DIAG_RING", "456")
	if got := logBufferEntriesR390(); got != 123 {
		t.Fatalf("размер буфера = %d, ожидалось 123 (главная переменная)", got)
	}
}

// TestR476_LogBufferAlias — псевдоним включается, когда основной нет.
func TestR476_LogBufferAlias(t *testing.T) {
	os.Unsetenv("LB_LOG_BUFFER_ENTRIES")
	t.Setenv("LB_DIAG_RING", "321")
	if got := logBufferEntriesR390(); got != 321 {
		t.Fatalf("размер буфера = %d, ожидалось 321 (псевдоним)", got)
	}
}

// TestR476_LogBufferGarbageFallsThrough — мусор в главной переменной не отменяет
// псевдоним и не роняет дефолт.
func TestR476_LogBufferGarbageFallsThrough(t *testing.T) {
	t.Setenv("LB_LOG_BUFFER_ENTRIES", "abc")
	t.Setenv("LB_DIAG_RING", "777")
	if got := logBufferEntriesR390(); got != 777 {
		t.Fatalf("размер буфера = %d, ожидалось 777 (псевдоним после мусора)", got)
	}
}

// TestR476_LogBufferDefault — обе не заданы: дефолт.
func TestR476_LogBufferDefault(t *testing.T) {
	os.Unsetenv("LB_LOG_BUFFER_ENTRIES")
	os.Unsetenv("LB_DIAG_RING")
	if got := logBufferEntriesR390(); got != logBufferDefaultR390 {
		t.Fatalf("размер буфера = %d, ожидался дефолт %d", got, logBufferDefaultR390)
	}
}
