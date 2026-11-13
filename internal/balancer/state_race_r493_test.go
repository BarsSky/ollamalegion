// state_race_r493_test.go — R493 (2026-11-13): гонка, которую поймал CI.
//
// ЖИВОЙ ФАКТ. На коммите с golden-контрактом CI (self-hosted, -race) дал
// DATA RACE: фоновый autosave (`SaveState`) и чтение состава кластера
// (`compositionNodeR95`) читали поля `types.Backend` БЕЗ замка записи, а поллер
// 12i в это время обновлял идентичность воркера под замком. Гонка существовала и
// раньше (её давал любой путь обновления статуса), но постоянный фоновый писатель
// сделал окно регулярным.
//
// Тест держит оба конца одновременно: писатель (identity refresh), autosave и
// чтение состава кластера. Без замков `-race` падает — это и есть охраняемое
// поведение, отдельной «проверки по исходнику» здесь нет намеренно.
package balancer

import (
	"sync"
	"testing"
	"time"
)

// TestR493_SaveStateConcurrentWithIdentityRefresh — autosave, состав кластера и
// обновление идентичности не гоняются за поля бэкенда.
func TestR493_SaveStateConcurrentWithIdentityRefresh(t *testing.T) {
	p := identityProxyR493(t, "127.0.0.1", 1)
	p.statePath = t.TempDir() + "/state.json"

	ident := workerIdentityR493{
		Version:   "r261112-2146",
		Commit:    "32153c14",
		BuildDate: "2026-11-12T18:46:37Z",
		RPCProto:  "6.0.0",
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Писатель: так поллер обновляет идентичность раз в TTL.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				p.applyWorkerIdentityR493("b1", ident)
			}
		}
	}()

	// Читатели: autosave (SaveState) и состав кластера (compositionNodeR95).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			deadline := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(deadline) {
				if reader == 0 {
					_ = p.SaveState()
					continue
				}
				_ = p.ValidateCompositionR95(CompositionRequestR95{})
			}
		}(i)
	}

	time.Sleep(250 * time.Millisecond)
	close(done)
	wg.Wait()

	// Значение обязано быть на месте: писатель не потерялся в замках. Читаем тем
	// же способом, что продакшен — под замком записи (рядом ещё тикает фоновый
	// поллер прокси).
	if got := backendIdentitySnapshotR493(p, "b1"); got.commit != "32153c14" {
		t.Fatalf("идентичность не применилась: %q", got.commit)
	}
}
