// cold_candidate_ttl_r480_test.go — R480 (P1-5): «холодный кандидат» — факт,
// а не догадка. Метрика окна имеет срок годности (LB_R459_COLD_TTL_SEC).
//
// Правило:
//   - perSeq > 0            → окно известно, сравниваем с need;
//   - perSeq = 0, свежая    → «точно нет модели» → переезд отклонён;
//   - perSeq = 0, устарела
//     или не опрашивали     → «неизвестно» → кандидат допущен (с WARN в лог).
package balancer

import (
	"strings"
	"testing"
	"time"
)

// TestR480_FreshColdCandidateRejected — свежая метрика без загруженных моделей:
// это факт «модели нет», переезд запрещён.
func TestR480_FreshColdCandidateRejected(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r459Proxy(65536, 0)
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now())

	ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 65536, "session")
	if ok {
		t.Fatal("свежая метрика без модели — переезд обязан быть запрещён")
	}
	if !strings.Contains(why, "не загружена") {
		t.Fatalf("причина не называет холодный кандидат: %s", why)
	}
}

// TestR480_StaleColdCandidateAllowed — метрика устарела: это «неизвестно».
// Раньше такой кандидат отклонялся, и сессия стояла на занятой копии до
// обновления метрики — «перестало работать распределение».
func TestR480_StaleColdCandidateAllowed(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r459Proxy(65536, 0)
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now().Add(-10*time.Minute))

	ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 65536, "session")
	if !ok {
		t.Fatalf("устаревшая метрика — это «неизвестно», а переезд отклонён: %s", why)
	}
}

// TestR480_NeverPolledCandidateAllowed — запись о бэкенде есть, но поллер по
// нему не отрабатывал: тоже «неизвестно», не «нет модели».
func TestR480_NeverPolledCandidateAllowed(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r459Proxy(65536, 0) // ни одной отметки опроса

	ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 65536, "session")
	if !ok {
		t.Fatalf("метрика ни разу не приходила — это «неизвестно», переезд отклонён: %s", why)
	}
}

// TestR480_ColdTTLZeroMeansStrict — LB_R459_COLD_TTL_SEC=0: TTL выключен,
// любая существующая запись считается точной → старый кандидат отклоняется.
func TestR480_ColdTTLZeroMeansStrict(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "0")
	p := r459Proxy(65536, 0)
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now().Add(-10*time.Minute))

	ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 65536, "session")
	if ok {
		t.Fatalf("при выключенном TTL старый кандидат обязан отклоняться, получено ok (%s)", why)
	}
}

// TestR480_KnownWindowStillCompared — известное окно сравнивается с need
// независимо от возраста метрики (пока perSeq > 0 он достоверен).
func TestR480_KnownWindowStillCompared(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r459Proxy(65536, 32768)
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now().Add(-2*time.Hour))

	if ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 65536, "session"); ok {
		t.Fatalf("32768 < need 65536, переезд обязан быть запрещён (%s)", why)
	}
	if ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, 20000, "session"); !ok {
		t.Fatalf("32768 >= need 20000, переезд разрешён (%s)", why)
	}
}

// TestR480_LoadedPerSeqKnown — сам предикат достоверности.
func TestR480_LoadedPerSeqKnown(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r459Proxy(65536, 0)

	if _, known := p.loadedPerSeqNCtxKnownR480("b2", r459Model); known {
		t.Fatal("без отметки опроса known=true, ожидалось false")
	}
	p.metricsMgr.MarkLlamaCppPolled("b2", time.Now())
	if w, known := p.loadedPerSeqNCtxKnownR480("b2", r459Model); !known || w != 0 {
		t.Fatalf("свежая пустая метрика: (w=%d known=%v), ожидалось (0,true)", w, known)
	}
	if w, known := p.loadedPerSeqNCtxKnownR480("b1", r459Model); !known || w != 65536 {
		t.Fatalf("известное окно: (w=%d known=%v), ожидалось (65536,true)", w, known)
	}
}
