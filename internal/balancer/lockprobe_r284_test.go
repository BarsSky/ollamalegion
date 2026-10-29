package balancer

// lockprobe_r284_test.go — R284: проба лока должна честно говорить «не берётся».

import (
	"testing"
	"time"
)

// TestR284_ProbeLocksHealthy_DetectsStuck — если p.mu удерживается писателем
// дольше бюджета, проба обязана вернуть false (именно на этом строится 503
// lock_stuck вместо «healthy при мёртвом API»).
func TestR284_ProbeLocksHealthy_DetectsStuck(t *testing.T) {
	p := &Proxy{}

	p.mu.Lock()
	ok, waited := p.ProbeLocksHealthy(120 * time.Millisecond)
	if ok {
		p.mu.Unlock()
		t.Fatal("проба сказала «лок берётся», хотя он удерживается писателем")
	}
	if waited < 100*time.Millisecond {
		p.mu.Unlock()
		t.Fatalf("проба вернулась раньше бюджета: %v", waited)
	}
	p.mu.Unlock()

	// После разблокировки проба обязана позеленеть. Зависшая проба могла ещё
	// держать флаг in-flight — ждём, пока она завершится.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := p.ProbeLocksHealthy(300 * time.Millisecond); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("после разблокировки проба так и не позеленела")
}

// TestR284_ProbeLocksHealthy_FreeMutex — на свободном мьютексе проба быстрая.
func TestR284_ProbeLocksHealthy_FreeMutex(t *testing.T) {
	p := &Proxy{}
	ok, waited := p.ProbeLocksHealthy(time.Second)
	if !ok {
		t.Fatal("свободный мьютекс: проба не взяла лок")
	}
	if waited > 200*time.Millisecond {
		t.Fatalf("проба на свободном локе подозрительно медленная: %v", waited)
	}
}

// TestR284_ProbeLocksHealthy_NilProxy — nil-прокси не паникует и считается живым
// (нет лока, который мог бы залипнуть).
func TestR284_ProbeLocksHealthy_NilProxy(t *testing.T) {
	var p *Proxy
	ok, _ := p.ProbeLocksHealthy(time.Second)
	if !ok {
		t.Fatal("nil-прокси: проба должна быть нейтральной")
	}
}
