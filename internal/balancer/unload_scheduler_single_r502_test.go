//go:build llama_stub

// unload_scheduler_single_r502_test.go — R502 (2026-11-19), аудит S4, пункт X-1.
//
// ДЕФЕКТ. Планировщик выгрузки создавался ДВАЖДЫ: в конструкторе прокси (и там же
// запускался) и в `cmd/balancer/main.go` — второй объект подменял указатель
// (`SetUnloadScheduler`) и запускался ещё раз. Первый цикл продолжал жить, но его
// `lastUsed` никто не обновлял: `RecordModelUse` идёт через указатель прокси. Итог —
// лишний фоновый цикл, двойные записи «unloadScheduler» в логе и «мёртвая» выгрузка,
// которую нельзя было ни увидеть, ни выключить.
package balancer

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestR502_MainDoesNotCreateSecondScheduler — X-1: объект один, его создаёт прокси.
func TestR502_MainDoesNotCreateSecondScheduler(t *testing.T) {
	src, err := os.ReadFile("../../cmd/balancer/main.go")
	if err != nil {
		t.Fatalf("чтение main.go: %v", err)
	}
	text := string(src)
	if strings.Contains(text, "NewUnloadScheduler(") {
		t.Error("main.go создаёт свой планировщик выгрузки — это и есть двойной " +
			"объект аудита X-1: возьмите proxy.GetUnloadScheduler()")
	}
	if strings.Contains(text, "SetUnloadScheduler(") {
		t.Error("main.go подменяет указатель планировщика (SetUnloadScheduler удалён " +
			"как причина второго, «мёртвого» цикла — аудит X-1)")
	}
	if !strings.Contains(text, "GetUnloadScheduler()") {
		t.Error("main.go не берёт единственный планировщик через GetUnloadScheduler()")
	}
}

// TestR502_SchedulerStartIsIdempotent — X-1: повторный Start не поднимает второй цикл.
func TestR502_SchedulerStartIsIdempotent(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())
	p.config.Balancing.ModelInstances.IdleUnloadAfter = "15m"
	us := NewUnloadScheduler(p)
	if !us.enabled {
		t.Fatal("планировщик не включился при idleUnloadAfter=15m")
	}

	us.Start()
	us.Start()
	us.mu.RLock()
	started := us.started
	stopCh := us.stopCh
	us.mu.RUnlock()
	if !started || stopCh == nil {
		t.Fatalf("после Start: started=%v stopCh=%v — цикл не запущен", started, stopCh)
	}
	// Наблюдаемое свойство: реально поднят ОДИН цикл. Флаг started можно
	// выставить дважды по ошибке, а счётчик показывает то, что произошло.
	// Ждём, пока поднятый цикл успеет отметиться, затем убеждаемся, что второй
	// не появился (при мутации «убрать проверку started» их будет два).
	for i := 0; i < 100 && atomic.LoadInt32(&us.loopStarts) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&us.loopStarts); n != 1 {
		t.Fatalf("после двух Start поднято циклов: %d, ожидался 1 (аудит X-1: "+
			"второй цикл не обновляет lastUsed и «выгружает» вслепую)", n)
	}

	us.Stop()
	us.mu.RLock()
	started = us.started
	us.mu.RUnlock()
	if started {
		t.Fatal("после Stop признак started остался true — повторный Start не сработает")
	}

	// Запуск после остановки снова работает (иначе выгрузку нельзя вернуть,
	// не пересоздавая прокси).
	us.Start()
	us.mu.RLock()
	started = us.started
	us.mu.RUnlock()
	if !started {
		t.Fatal("после Stop+Start цикл не поднялся")
	}
	us.Stop()

	// Уборка: дать горутине цикла увидеть закрытый stopCh до конца теста.
	time.Sleep(10 * time.Millisecond)
}
