package balancer

import (
	"testing"
	"time"
)

// R546 (T-7 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// В аудите записано: «порог зомби 120 с удаляет сессию с живым стримом
// (LastRequestAt не обновляется в стриме)». Проверка кода показала, что это
// утверждение УСТАРЕЛО: R252 добавил startStreamSession → HeartbeatStream каждые
// getHeartbeatInterval() (дефолт 15 с), и Stop() снимает флаг.
//
// Осталась реальная ловушка: heartbeat и порог зомби задаются оператором
// НЕЗАВИСИМО, и при heartbeat ≥ порога живой стрим снова удаляется. Дешёво это
// видно только здесь — поэтому правило «heartbeat не реже порога/2» применяется
// при создании прокси, с Warn-ом и обоими числами.

// TestR546_LiveStreamSurvivesZombieThreshold — сердцебиение оживляет сессию со
// стримом, и cleanup() её НЕ удаляет. Без heartbeat — удаляет (это и есть
// исходный дефект, который R252 закрыл).
func TestR546_LiveStreamSurvivesZombieThreshold(t *testing.T) {
	const zombie = 120 * time.Second
	newMgr := func() *SessionManager {
		sm := NewSessionManagerWithTTL(15*time.Minute, 5*time.Minute)
		sm.SetZombieThreshold(zombie)
		sm.Set("s1", "backend-1", "qwen3-8b", "cline", "10.0.0.5", "test")
		sm.SetStreamActive("s1", true)
		// Сессия «молчит» 3 минуты — больше порога. Это ровно то состояние, в
		// котором живой стрим (префилл/tool-call) оказывается без heartbeat.
		sm.mu.Lock()
		sm.sessions["s1"].LastRequestAt = time.Now().Add(-3 * time.Minute)
		sm.mu.Unlock()
		return sm
	}

	// Контроль: без heartbeat сессия считается зомби и удаляется.
	dead := newMgr()
	if got := dead.DetectZombieSessions(zombie); len(got) != 1 {
		t.Fatalf("контроль: ожидалась одна зомби-сессия, получено %d", len(got))
	}
	dead.cleanup()
	if dead.Get("s1") != nil {
		t.Fatalf("контроль: «молчащая» стрим-сессия должна удаляться — иначе тест ничего не проверяет")
	}

	// Живой стрим: heartbeat пришёл перед cleanup — сессия обязана остаться.
	live := newMgr()
	live.HeartbeatStream("s1")
	if got := live.DetectZombieSessions(zombie); len(got) != 0 {
		t.Fatalf("после heartbeat сессия не может считаться зомби, получено %d", len(got))
	}
	live.cleanup()
	if live.Get("s1") == nil {
		t.Fatalf("R546: сессия с живым стримом удалена, хотя heartbeat обновил LastRequestAt — " +
			"именно это выглядело как «балансер потерял клиента посреди генерации»")
	}
}

// TestR546_HeartbeatClampedBelowZombieThreshold — heartbeat не может быть реже
// половины порога зомби: конфиг «heartbeat 300 с при пороге 120 с» применяется
// как 60 с, а расхождение печатается (не молча).
func TestR546_HeartbeatClampedBelowZombieThreshold(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.AdvancedTiming.HeartbeatIntervalSec = 300
	cfg.Balancing.AdvancedTiming.ZombieSessionThresholdSec = 120

	proxy := newProxyWithCleanup(t, cfg)

	got := proxy.getHeartbeatInterval()
	want := 60 * time.Second
	if got != want {
		t.Fatalf("heartbeat = %v, ожидалось %v (порог зомби %v, heartbeat не реже половины порога); "+
			"при heartbeat ≥ порога живой стрим снова попадает под cleanup", got, want, 120*time.Second)
	}
	if proxy.sessionMgr == nil {
		t.Fatal("нет sessionMgr")
	}
}

// TestR546_NormalConfigKeepsHeartbeat — обычный конфиг не трогаем: дефолт 15 с
// при пороге 120 с остаётся как есть (правило применяется только к нарушению).
func TestR546_NormalConfigKeepsHeartbeat(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.AdvancedTiming.HeartbeatIntervalSec = 15
	cfg.Balancing.AdvancedTiming.ZombieSessionThresholdSec = 120

	proxy := newProxyWithCleanup(t, cfg)
	if got := proxy.getHeartbeatInterval(); got != 15*time.Second {
		t.Fatalf("heartbeat = %v, ожидалось 15s: штатный конфиг менять нельзя", got)
	}

	// И дефолтный конфиг (поле не задано) тоже даёт 15 с.
	def := newProxyWithCleanup(t, createTestConfig())
	if got := def.getHeartbeatInterval(); got != 15*time.Second {
		t.Fatalf("heartbeat по умолчанию = %v, ожидалось 15s", got)
	}
}

