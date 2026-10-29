package balancer

// lock_deadlock_r284_test.go — R284 (2026-10-29): воспроизведение и защита от
// дедлока p.mu, который останавливал балансер под нагрузкой агентских метрик.
//
// ЖИВОЙ ДЕФЕКТ (стенд r284). Через ~5 минут нагрузки на /api/v1/agents/metrics:
//   - /api/v1/health перестаёт отвечать (клиент видит 000 после 40 с ожидания);
//   - контейнер при этом healthy, потому что docker healthcheck ходит на
//     /api/v1/ping, который не берёт p.mu;
//   - SIGQUIT-дамп: ~20 горутин ждут p.mu (BackendExists, GetClusterState),
//     ~15 ждут p.mu.Lock внутри AdoptCapacityFromNode, а один писатель не
//     получает лок минутами.
//
// ПРИЧИНА. sync.RWMutex не рекурсивен. `selectBackendExcluding` брал
// `p.mu.RLock()` и ВНУТРИ вызывал `findBackendWithModelExcluding`, который берёт
// `p.mu.RLock()` ещё раз. Пока писателей нет, вложенный RLock выдают — поэтому
// дефект не виден ни в обычных тестах, ни на холостом ходу. Как только в
// очередь на запись встаёт писатель (а на каждый POST агентских метрик
// `UpdateBackendAgentStatus` берёт p.mu.Lock), ВТОРОЙ RLock не выдают никогда:
// писатель ждёт первого читателя, а читатель ждёт писателя. Дедлок навсегда,
// при живом контейнере.
//
// ТЕСТ. Читатель крутит `selectBackendExcluding` (путь ретрая запроса), писатель
// крутит `UpdateBackendAgentStatus` (путь агентских метрик). На исправном коде
// оба укладываются в отведённое время; на рекурсивном RLock тест встаёт и падает
// по таймауту — то есть ровно то, что видел оператор.

import (
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR284_RecursiveRLockDoesNotDeadlock — reader (ретрай запроса) не должен
// вставать навсегда, когда writer (агентские метрики) ждёт p.mu.
func TestR284_RecursiveRLockDoesNotDeadlock(t *testing.T) {
	cfg := createTestConfig()
	// ModelAffinity включает ветку, в которой selectBackendExcluding держал
	// RLock и вызывал функцию, берущую RLock повторно.
	cfg.Balancing.ModelAffinity = true
	p := newProxyWithCleanup(t, cfg)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Писатель: ровно то, что делает обработчик агентских метрик.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.UpdateBackendAgentStatus("backend-1", true)
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			_ = p.selectBackendExcluding("llama3.1:8b", map[string]bool{"backend-2": true}, types.BackendTypeOllama)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatal("ДЕДЛОК: рекурсивный p.mu.RLock под ожидающим писателем — " +
			"балансер перестал бы отвечать, оставаясь «healthy»")
	}
	close(stop)
	wg.Wait()
}

// TestR284_ReadersDoNotStarveWriter — обратная сторона: пока идут чтения,
// писатель обязан получать лок (иначе «health зелёный, а состояние не
// обновляется»). Проверяем прогресс писателя, а не только отсутствие дедлока.
func TestR284_ReadersDoNotStarveWriter(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.ModelAffinity = true
	p := newProxyWithCleanup(t, cfg)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 8 читателей (имитация параллельных запросов)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = p.selectBackendExcluding("llama3.1:8b", map[string]bool{"backend-2": true}, types.BackendTypeOllama)
				}
			}
		}()
	}

	// Писатель должен успеть много раз за 3 секунды.
	writes := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.UpdateBackendAgentStatus("backend-1", true)
		writes++
	}
	close(stop)
	wg.Wait()

	if writes < 100 {
		t.Fatalf("писатель сделал только %d записей за 3 с — чтения голодают его (рекурсивный RLock)", writes)
	}
}
