//go:build llama_stub

// tokenize_not_blocked_d2_test.go — D2 (2026-11-25): подсчёт токенов не должен
// зависеть от занятости слота.
//
// ЖИВОЙ ДЕФЕКТ (реестр дефектов 2026-11-24, D2):
//
//	06:24:24 POST /api/tokenize duration=46.155s
//
// — при 45–47 мс на соседнем узле. Следствие в балансере: `exact prompt tokens
// unavailable … timeout` (1500/6000 мс) и работа «по оценке», завышающей окно
// примерно вдвое (`estimated 48121 → actual 33839`).
//
// ПРИЧИНА — НЕ токенизатор. Прежний путь /api/tokenize шёл через
// getModelInstance, который берёт inst.mu, а этот мьютекс удерживается ВСЮ
// генерацию (см. Generate: «держим mu на всём инференсе»). Значит запрос
// токенизации ждал, пока закончится ЧУЖОЙ запрос: 46 с — это время до конца
// генерации, а не стоимость подсчёта.
//
// Тест ниже воспроизводит это без живой модели: «занятый слот» — это
// inst.mu.Lock() (ровно то, что делает Generate). На состоянии ДО правки тест
// красный (таймаут ожидания), после правки — зелёный: токенизация идёт по
// отдельному замку жизни словаря (tokenMu) и inst.mu не берёт.
package cppbackend

import (
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/tokencount"
)

// newTokenizeTestBackendD2 — бэкенд с одной «загруженной» моделью.
func newTokenizeTestBackendD2(t *testing.T) (*Backend, *modelInstance, string) {
	t.Helper()
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "d2-tokenize-model"
	inst := &modelInstance{
		info:   ModelInfo{Name: name, State: StateLoaded},
		handle: &bridge.ModelHandle{},
	}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()
	return b, inst, name
}

// TestD2_TokenizeDoesNotBlockOnBusySlot — главный тест дефекта.
//
// «Занятый слот» здесь — удержанный inst.mu: именно его держит Generate на всё
// время инференса. До правки CountTokensExact стоял в очереди за этим замком
// (живой замер: 46.155 s при 45–47 мс на свободном слоте).
func TestD2_TokenizeDoesNotBlockOnBusySlot(t *testing.T) {
	b, inst, name := newTokenizeTestBackendD2(t)
	text := strings.Repeat("промпт ", 2000) // ~14k символов: как в живом замере

	// Занятый слот: ровно то, что делает Generate (inst.mu на всю генерацию).
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.incQueries() // и счётчик активных запросов, как у настоящей генерации
	defer inst.decQueries()

	type result struct {
		n     int
		exact bool
	}
	done := make(chan result, 1)
	go func() {
		n, exact := b.CountTokensExact(name, text)
		done <- result{n: n, exact: exact}
	}()

	select {
	case got := <-done:
		if got.n <= 0 {
			t.Errorf("токенов = %d, ожидалось > 0", got.n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CountTokensExact заблокировался на inst.mu, который удерживает " +
			"генерация: это и есть D2 — подсчёт токенов ждёт чужой запрос " +
			"(живой замер: 46.155 s против 45–47 мс на соседнем узле)")
	}
}

// TestD2_TokenizeValueMatchesFreeSlot — число не зависит от того, занят слот или
// нет: тот же текст, тот же подсчёт. Иначе «починили скорость» ценой другого
// ответа.
func TestD2_TokenizeValueMatchesFreeSlot(t *testing.T) {
	b, inst, name := newTokenizeTestBackendD2(t)
	text := strings.Repeat("hello world ", 500)

	freeN, freeExact := b.CountTokensExact(name, text)

	inst.mu.Lock()
	busyN, busyExact := b.CountTokensExact(name, text)
	inst.mu.Unlock()

	if freeN != busyN || freeExact != busyExact {
		t.Errorf("подсчёт зависит от занятости слота: свободный (%d, exact=%v) != "+
			"занятый (%d, exact=%v)", freeN, freeExact, busyN, busyExact)
	}
	// stub-сборка честно говорит «это оценка» (exact=false) — проверяем, что
	// значение именно оценщика pkg/tokencount, а не мусор.
	if want := tokencount.Estimate(text); freeN != want {
		t.Errorf("токенов = %d, ожидалось %d (pkg/tokencount.Estimate в stub-сборке)",
			freeN, want)
	}
}

// TestD2_TokenizeAfterUnloadFallsBackToEstimate — выгруженная модель: оценка и
// exact=false, без паники. Проверяет, что «лёгкий путь» не читает освобождённый
// словарь: unload берёт tokenMu на запись, токенизация — на чтение.
func TestD2_TokenizeAfterUnloadFallsBackToEstimate(t *testing.T) {
	b, _, name := newTokenizeTestBackendD2(t)
	text := strings.Repeat("x", 4000)

	if err := b.UnloadModel(name); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	n, exact := b.CountTokensExact(name, text)
	if exact {
		t.Error("exact=true для выгруженной модели: реального токенизатора нет")
	}
	if n != tokencount.Estimate(text) {
		t.Errorf("токенов = %d, ожидалось %d (оценка)", n, tokencount.Estimate(text))
	}
	// Модели нет вовсе — та же безопасная деградация.
	if n := b.CountTokens("нет-такой-модели", text); n <= 0 {
		t.Errorf("CountTokens(нет модели) = %d, ожидалась оценка > 0", n)
	}
}

// TestD2_TokenizeConcurrentWithUnload — гонок быть не должно (тест гоняется под
// -race): токенизация и выгрузка сериализуются замком жизни словаря, а не
// «на глазок».
func TestD2_TokenizeConcurrentWithUnload(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "d2-race-model"
	text := strings.Repeat("token ", 200)

	insert := func() {
		inst := &modelInstance{
			info:   ModelInfo{Name: name, State: StateLoaded},
			handle: &bridge.ModelHandle{},
		}
		b.mu.Lock()
		b.models[name] = inst
		b.mu.Unlock()
	}
	insert()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = b.CountTokensExact(name, text)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := b.UnloadModel(name); err != nil {
			t.Fatalf("UnloadModel: %v", err)
		}
		insert()
	}
	close(stop)
	wg.Wait()
}
