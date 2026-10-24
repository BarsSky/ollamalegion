// first_byte_default_r189_test.go — R189 (2026-10-20).
//
// Живой дефект: /api/chat с tools[] не отдавал ни байта 300 с, потому что
// граница первого байта ставилась только при явной настройке профиля/окружения.
package balancer

import (
	"os"
	"testing"
	"time"
)

func TestR189_DefaultFirstByteBound(t *testing.T) {
	os.Unsetenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC")
	if got := defaultFirstByteTimeout(); got != defaultFirstByteTimeoutSec*time.Second {
		t.Fatalf("по умолчанию ожидалось %v, получено %v", defaultFirstByteTimeoutSec*time.Second, got)
	}
	if defaultFirstByteDisabled() {
		t.Fatal("граница не должна быть отключена по умолчанию")
	}
}

func TestR189_ExplicitDisableAndOverride(t *testing.T) {
	os.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "0")
	defer os.Unsetenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC")
	if !defaultFirstByteDisabled() {
		t.Fatal("0 должен отключать границу")
	}

	os.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "45")
	if got := defaultFirstByteTimeout(); got != 45*time.Second {
		t.Fatalf("ожидалось 45s, получено %v", got)
	}
}
