//go:build llama_stub

// load_claims_r303_test.go — R303: семантика заявок на загрузку и возраста
// in-flight в пакете cppbackend.
//
// Отчёт по двухузловому стенду (D3) описал состояние, в котором узел живёт до
// перезапуска: `POST /api/models/load` висит, `unload` отвечает 409 «model is
// currently loading», `in_flight_requests` не пустеет. Причина — заявка на
// загрузку без владельца и ожидание канала БЕЗ таймаута. Здесь проверяются
// ровно те примитивы, которыми это закрыто.
package cppbackend

import (
	"testing"
	"time"
)

func r303TestBackend(t *testing.T) *Backend {
	t.Helper()
	return NewBackend(Config{Host: "127.0.0.1", Port: 18091})
}

// TestR303_WaitForLoadTimeout_TimesOut — ожидание чужой загрузки ограничено и
// сообщает о таймауте, а не висит (живой стенд: 260 c без ответа).
func TestR303_WaitForLoadTimeout_TimesOut(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-wait-timeout"
	if ok, err := b.TryLockLoadR303(name, "test"); !ok || err != nil {
		t.Fatalf("TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer b.UnlockLoad(name)

	start := time.Now()
	loaded, timedOut := b.WaitForLoadTimeoutR303(name, 80*time.Millisecond)
	elapsed := time.Since(start)

	if loaded {
		t.Fatal("loaded=true, хотя модель не загружена")
	}
	if !timedOut {
		t.Fatal("timedOut=false — ожидание вернулось без таймаута и без загрузки")
	}
	if elapsed < 60*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("ожидание длилось %s, ожидалось ~80 мс", elapsed)
	}
}

// TestR303_WaitForLoadTimeout_WakesOnUnlock — освобождение заявки будит
// ожидающих (не «спит до таймаута», иначе падает пропускная способность).
func TestR303_WaitForLoadTimeout_WakesOnUnlock(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-wait-wake"
	if ok, err := b.TryLockLoadR303(name, "test"); !ok || err != nil {
		t.Fatalf("TryLockLoadR303 = (%v, %v)", ok, err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		b.UnlockLoad(name)
	}()

	start := time.Now()
	loaded, timedOut := b.WaitForLoadTimeoutR303(name, 5*time.Second)
	elapsed := time.Since(start)

	if timedOut {
		t.Fatal("timedOut=true при освобождённой заявке")
	}
	if loaded {
		t.Fatal("loaded=true: загрузка не выполнялась, модель не в реестре")
	}
	if elapsed > time.Second {
		t.Fatalf("ожидание не проснулось по UnlockLoad: %s", elapsed)
	}
}

// TestR303_ReclaimStaleClaim_ReclaimsDeadClaim — заявка без живого инстанса
// снимается, и IsLoading становится false (это и есть «лечение без перезапуска»).
func TestR303_ReclaimStaleClaim_ReclaimsDeadClaim(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-reclaim"
	if ok, err := b.TryLockLoadR303(name, "dead-owner"); !ok || err != nil {
		t.Fatalf("TryLockLoadR303 = (%v, %v)", ok, err)
	}
	if !b.IsLoading(name) {
		t.Fatal("заявка не видна")
	}

	claim, reclaimed := b.ReclaimStaleLoadClaimR303(name, 0)
	if !reclaimed {
		t.Fatal("мёртвая заявка не снята")
	}
	if claim.Requester != "dead-owner" {
		t.Errorf("Requester=%q, ожидался владелец заявки", claim.Requester)
	}
	if b.IsLoading(name) {
		t.Fatal("IsLoading=true после снятия заявки — unload снова получит вечный 409")
	}
	// Повторное снятие — no-op (никаких паник на закрытом канале).
	if _, again := b.ReclaimStaleLoadClaimR303(name, 0); again {
		t.Fatal("повторное снятие сработало — заявки уже нет")
	}
	// После снятия заявку можно взять заново (второй load не блокируется).
	if ok, err := b.TryLockLoadR303(name, "new-owner"); !ok || err != nil {
		t.Fatalf("после снятия заявки новый load не взял лок: (%v, %v)", ok, err)
	}
	b.UnlockLoad(name)
}

// TestR303_ReclaimStaleClaim_RespectsMinAge — порог возраста соблюдается:
// свежую заявку не трогаем (иначе можно запустить ВТОРУЮ загрузку того же файла).
func TestR303_ReclaimStaleClaim_RespectsMinAge(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-reclaim-age"
	if ok, err := b.TryLockLoadR303(name, "owner"); !ok || err != nil {
		t.Fatalf("TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer b.UnlockLoad(name)

	if _, reclaimed := b.ReclaimStaleLoadClaimR303(name, time.Hour); reclaimed {
		t.Fatal("свежая заявка снята — это разрешило бы вторую загрузку того же файла")
	}
	if !b.IsLoading(name) {
		t.Fatal("заявка исчезла, хотя снимать её было нельзя")
	}
}

// TestR303_LoadClaims_SnapshotHasOwnerAndAge — диагностика: владелец и возраст.
func TestR303_LoadClaims_SnapshotHasOwnerAndAge(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-snapshot"
	if ok, err := b.TryLockReloadR303(name, "reload-owner"); !ok || err != nil {
		t.Fatalf("TryLockReloadR303 = (%v, %v)", ok, err)
	}
	defer b.UnlockLoad(name)

	time.Sleep(25 * time.Millisecond)

	claims := b.LoadClaimsR303()
	if len(claims) != 1 {
		t.Fatalf("LoadClaimsR303=%d записей, ожидалась 1", len(claims))
	}
	c := claims[0]
	if c.Model != name || c.Requester != "reload-owner" {
		t.Errorf("снимок заявки: %+v", c)
	}
	if c.AgeSec <= 0 {
		t.Errorf("AgeSec=%v, ожидалось > 0", c.AgeSec)
	}
	if c.LoadingInstance || c.ModelLoaded {
		t.Errorf("инстанс не должен считаться живым: %+v", c)
	}

	b.UnlockLoad(name)
	if got := b.LoadClaimsR303(); len(got) != 0 {
		t.Errorf("после UnlockLoad остались заявки: %+v", got)
	}
}

// TestR303_InFlight_AgesTrackedAndCleared — возраст in-flight появляется при
// первом активном запросе и снимается при последнем.
//
// Без этого «мусор в in_flight_requests» (D3: {"gemma": 3} минутами) неотличим
// от нормальной длинной генерации ни оператору, ни балансеру.
func TestR303_InFlight_AgesTrackedAndCleared(t *testing.T) {
	c := NewInFlightCounter()

	if ages := c.AgesR303(); len(ages) != 0 {
		t.Fatalf("пустой счётчик отдаёт возрасты: %v", ages)
	}

	c.Inc("m")
	c.Inc("m") // второй запрос не должен сбрасывать начало первого
	time.Sleep(30 * time.Millisecond)

	ages := c.AgesR303()
	age, ok := ages["m"]
	if !ok || age <= 0 {
		t.Fatalf("возраст не отслеживается: %v", ages)
	}

	model, oldest := c.OldestAgeR303()
	if model != "m" || oldest <= 0 {
		t.Fatalf("OldestAgeR303 = (%q, %v)", model, oldest)
	}

	c.Dec("m")
	if got := c.AgesR303(); len(got) != 1 {
		t.Fatalf("возраст снят до последнего Dec: %v", got)
	}
	c.Dec("m")
	if got := c.AgesR303(); len(got) != 0 {
		t.Fatalf("возраст остался после последнего Dec: %v", got)
	}

	// Аварийный сброс тоже снимает возраст.
	c.Inc("m")
	c.Reset("m")
	if got := c.AgesR303(); len(got) != 0 {
		t.Fatalf("Reset не снял возраст: %v", got)
	}
}

// TestR303_InFlight_NilSafe — счётчик и заявки должны быть nil-safe: cppworker
// переинициализирует backend в тестах и на старте.
func TestR303_InFlight_NilSafe(t *testing.T) {
	var c *InFlightCounter
	if got := c.AgesR303(); len(got) != 0 {
		t.Errorf("nil-счётчик отдаёт возрасты: %v", got)
	}
	if model, age := c.OldestAgeR303(); model != "" || age != 0 {
		t.Errorf("OldestAgeR303 на nil = (%q, %v)", model, age)
	}
	c.Inc("m")
	c.Dec("m")

	var b *Backend
	if _, ok := b.LoadClaimR303("m"); ok {
		t.Error("nil-backend отдаёт заявку")
	}
	if got := b.LoadClaimsR303(); got != nil {
		t.Errorf("nil-backend отдаёт заявки: %v", got)
	}
	if loaded, timedOut := b.WaitForLoadTimeoutR303("m", time.Millisecond); loaded || timedOut {
		t.Errorf("nil-backend: (%v, %v)", loaded, timedOut)
	}
	if _, reclaimed := b.ReclaimStaleLoadClaimR303("m", 0); reclaimed {
		t.Error("nil-backend снял заявку")
	}
}

// TestR303_WaitForLoadTimeout_LoadingRecordIsNotSuccess — R303.1: запись модели в
// состоянии loading — это НЕ завершённая загрузка.
//
// ЖИВОЙ ДЕФЕКТ (проверка на стенде после обновления образа). Проверка успеха
// была «GetModel вернул запись», а GetModel отдаёт запись и для loading. Поэтому
// sync-загрузка (`?wait=true`) с истёкшим пределом ожидания отвечала
//
//	200 {"status":"loaded_by_other","model":{"state":"loading"}}
//
// Клиент считал модель готовой и начинал слать запросы в ещё не поднятую модель.
// Здесь фиксируется контракт: при истечении срока и незавершённой загрузке
// результат обязан быть (loaded=false, timedOut=true) — вызывающий ответит
// типизированным 503, а не 200.
func TestR303_WaitForLoadTimeout_LoadingRecordIsNotSuccess(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-loading-record"
	if ok, err := b.TryLockLoadR303(name, "test"); !ok || err != nil {
		t.Fatalf("TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer b.UnlockLoad(name)

	// Запись есть, но веса ещё читаются.
	b.mu.Lock()
	b.models[name] = &modelInstance{info: ModelInfo{Name: name, State: StateLoading}}
	b.mu.Unlock()

	loaded, timedOut := b.WaitForLoadTimeoutR303(name, 80*time.Millisecond)
	if loaded {
		t.Fatal("loaded=true при state=loading: клиент получит 200 на незавершённую загрузку")
	}
	if !timedOut {
		t.Fatal("timedOut=false: ожидание обязано сообщить, что предел истёк")
	}
}

// TestR303_WaitForLoadTimeout_LoadedRecordIsSuccess — обратный случай: модель
// ДЕЙСТВИТЕЛЬНО загружена — ожидание возвращает успех даже без таймаута.
func TestR303_WaitForLoadTimeout_LoadedRecordIsSuccess(t *testing.T) {
	b := r303TestBackend(t)
	name := "r303-loaded-record"

	b.mu.Lock()
	b.models[name] = &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	b.mu.Unlock()

	loaded, timedOut := b.WaitForLoadTimeoutR303(name, 50*time.Millisecond)
	if !loaded || timedOut {
		t.Fatalf("loaded=%v timedOut=%v, ожидалось (true,false) при загруженной модели", loaded, timedOut)
	}
}

// TestR303_WaitForLoadTimeout_NoWaitAsksFact — режим «не ждать вовсе» (d<0)
// обязан отвечать фактом готовности, а не наличием записи.
func TestR303_WaitForLoadTimeout_NoWaitAsksFact(t *testing.T) {
	b := r303TestBackend(t)
	loading := "r303-nowait-loading"
	loaded := "r303-nowait-loaded"
	b.mu.Lock()
	b.models[loading] = &modelInstance{info: ModelInfo{Name: loading, State: StateLoading}}
	b.models[loaded] = &modelInstance{info: ModelInfo{Name: loaded, State: StateLoaded}}
	b.mu.Unlock()

	if ok, timedOut := b.WaitForLoadTimeoutR303(loading, -1); ok || !timedOut {
		t.Errorf("loading: ok=%v timedOut=%v, ожидалось (false,true)", ok, timedOut)
	}
	if ok, timedOut := b.WaitForLoadTimeoutR303(loaded, -1); !ok || timedOut {
		t.Errorf("loaded: ok=%v timedOut=%v, ожидалось (true,false)", ok, timedOut)
	}
}
