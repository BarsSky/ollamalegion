// model_catalog_reset_race_test.go — гонка «подмена каталога vs фоновое чтение»
// (R737, CI-дефект).
//
// ЖИВОЙ ДЕФЕКТ (прогон 45317255544, job «Test (self-hosted Windows)»):
//
//	WARNING: DATA RACE
//	Write at 0x00c0036fa660 by goroutine:
//	  balancer.planProxyR96()  internal/balancer/setup_plan_r96_test.go:54
//	Previous read at 0x00c0036fa660 by goroutine:
//	  balancer.(*Proxy).refreshBackendModelCatalog()  internal/balancer/model_catalog.go:397
//	    ← refreshStaleModelCatalogs ← llamaCppMetricsPoller.pollAll.func3
//	--- FAIL: TestPlacementPlanR96_SingleWhenOnlyOneNodeWouldFit
//
// Причина: тест подменял УКАЗАТЕЛЬ `p.modelCatalog = newModelCatalog()`, а это
// запись в общий указатель без синхронизации: фоновый поллер в этот момент
// читает его. Ожидание `WaitStopped(2s)` было без проверки результата — на
// медленном раннере оно истекало, и подмена происходила «под читателем».
//
// Лечение: (1) тесты очищают каталог НА МЕСТЕ (`reset()` под внутренним
// RWMutex), а не подменяют указатель; (2) `planProxyR96` проверяет результат
// ожидания поллера и падает с причиной. Этот тест закрепляет (1): сброс может
// идти одновременно с чтениями и не даёт гонки.
package balancer

import (
	"sync"
	"testing"
	"time"
)

// TestR737_CatalogResetIsRaceFreeUnderReaders — reset() и читатели каталога
// работают параллельно без гонки и без паники.
//
// Под `-race` тест краснел бы на варианте «подмена указателя» (проверено на
// старом коде: писатель `p.modelCatalog = newModelCatalog()` + читатель
// `catalog.lookup(...)` дают WARNING: DATA RACE).
func TestR737_CatalogResetIsRaceFreeUnderReaders(t *testing.T) {
	cat := newModelCatalog()
	cat.store("node-1", []string{"a.gguf", "b.gguf"}, "", time.Now())

	const readers = 8
	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(readers + 1)

	// Читатели: именно те методы, которыми каталог читает production.
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, _, _ = cat.lookup("node-1", "a.gguf")
				_ = cat.snapshot()
				_, _, _, _, _ = cat.presence("node-1", "a.gguf")
			}
		}()
	}

	// Писатель: сброс и повторное наполнение — как в тестах планирования.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cat.reset()
			cat.store("node-1", []string{"a.gguf"}, "", time.Now())
		}
	}()

	wg.Wait()

	// Смысл не потерян: после последнего сырого сброса каталог снова наполнен
	// (гонки записи «store после reset» нет — оба под одним мьютексом).
	if has, known, _, _, _ := cat.presence("node-1", "a.gguf"); !known || !has {
		t.Fatalf("после серии reset/store каталог не знает файл: has=%v known=%v", has, known)
	}
}

// TestR737_ResetClearsContents — reset() действительно очищает, а не только
// «делает вид»: иначе тесты планирования снова увидели бы модели соседнего стенда.
func TestR737_ResetClearsContents(t *testing.T) {
	cat := newModelCatalog()
	cat.store("node-1", []string{"a.gguf"}, "", time.Now())
	cat.reset()
	if _, known, _ := cat.lookup("node-1", "a.gguf"); known {
		t.Fatal("reset() не очистил каталог: запись о бэкенде осталась")
	}
	if snap := cat.snapshot(); len(snap) != 0 {
		t.Fatalf("после reset() снимок не пуст: %v", snap)
	}
}
