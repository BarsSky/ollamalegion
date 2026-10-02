package balancer

// image_lock_followup_test.go — R-Image follow-up (2026-10-02): два остатка
// Phase 6, зафиксированные в отчёте как известные ограничения.
//
//  1. Ключ лока сосуществования. ДО: лок брался по host целиком, и на
//     multi-GPU хосте генерация на одной карте блокировала текстовый бэкенд на
//     другой. ПОСЛЕ: ключ = host + индекс GPU, КОГДА индекс задан у ОБЕИХ
//     сторон; иначе — хост (консервативно: «неизвестно» не даёт права сузить
//     лок, иначе текстовый запрос мог бы уехать на карту генерации → OOM).
//
//  2. Лимит ожидания текстового запроса. ДО: если все текстовые кандидаты
//     отсеяны image-локом, запрос ждал LB_ADMISSION_WAIT_SEC (по умолчанию
//     300 с), а не balancing.image.queueWaitTimeoutSec. ПОСЛЕ: когда
//     единственная причина отсутствия кандидата — активный image-лок, ожидание
//     сужается до image-лимита; при любой другой причине (нет здоровых
//     бэкендов, нет модели, занятые слоты) поведение прежнее.

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Хелперы
// ============================================================

// setGPUIndex — проставить существующему бэкенду стенда ЯВНЫЙ индекс GPU.
//
// 0 — валидный индекс ПЕРВОЙ карты, а не «не задано» (R-Image follow-up:
// GPUIndex стал указателем именно для этого). «Неизвестно» — clearGPUIndex.
func setGPUIndex(t *testing.T, p *Proxy, backendID string, gpuIndex int) {
	t.Helper()
	b := p.GetBackend(backendID)
	if b == nil {
		t.Fatalf("бэкенд %q не найден на стенде", backendID)
	}
	cp := *b
	cp.GPUIndex = types.GPUIndexPtr(gpuIndex)
	if err := p.UpdateBackend(backendID, cp); err != nil {
		t.Fatalf("UpdateBackend(%s): %v", backendID, err)
	}
}

// clearGPUIndex — сбросить индекс в «неизвестно»: лок снова хостовый.
func clearGPUIndex(t *testing.T, p *Proxy, backendID string) {
	t.Helper()
	b := p.GetBackend(backendID)
	if b == nil {
		t.Fatalf("бэкенд %q не найден на стенде", backendID)
	}
	cp := *b
	cp.GPUIndex = nil
	if err := p.UpdateBackend(backendID, cp); err != nil {
		t.Fatalf("UpdateBackend(%s): %v", backendID, err)
	}
}

// acquireImageLock — взять лок как это делает image-запрос (beforeGeneration).
func acquireImageLock(t *testing.T, p *Proxy, backendID, host string, gpu gpuRef) *imageLockHolder {
	t.Helper()
	h, ok, _ := p.imageRes.acquireLock(context.Background(), backendID, host, gpu, "test-model")
	if !ok || h == nil {
		t.Fatalf("не удалось взять лок (backend=%s host=%s gpu=%+v)", backendID, host, gpu)
	}
	return h
}

// acquireImageLockForBackend — взять лок по индексу, ВЫВЕДЕННОМУ ИЗ БЭКЕНДА
// (EffectiveGPUIndex) — ровно как beforeGeneration. Так тест проверяет всю
// цепочку «поле бэкенда → known/индекс → ключ лока», а не только сам лок.
func acquireImageLockForBackend(t *testing.T, p *Proxy, backendID string) *imageLockHolder {
	t.Helper()
	b := p.GetBackend(backendID)
	if b == nil {
		t.Fatalf("бэкенд %q не найден на стенде", backendID)
	}
	idx, known := b.EffectiveGPUIndex()
	return acquireImageLock(t, p, backendID, b.Host, gpuRef{index: idx, known: known})
}

// secondHostBackend — второй текстовый бэкенд (для проверок «другой хост/карта»).
func secondHostBackend(id, host string, gpu gpuRef) types.Backend {
	b := types.Backend{
		ID:                id,
		Name:              "text " + id,
		Host:              host,
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 4,
	}
	if gpu.known {
		b.GPUIndex = types.GPUIndexPtr(gpu.index)
	}
	return b
}

// ============================================================
// 1. Ключ лока: host + индекс GPU
// ============================================================

// Индекс известен у ОБЕИХ сторон → лок блокирует только свою карту.
func TestGPUIndexLock_SeparatesCards(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{},
		secondHostBackend("llm-2", "127.0.0.1", knownGPU(2)))
	setGPUIndex(t, p, "img-1", 1)
	setGPUIndex(t, p, "llm-1", 1)
	setGPUIndex(t, p, "llm-2", 2)

	holder := acquireImageLockForBackend(t, p, "img-1")
	defer holder.Release("test_done")

	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Error("текстовый бэкенд на ТОЙ ЖЕ карте обязан блокироваться (exclusive)")
	}
	if p.imageGateBlocksTextBackend(p.GetBackendState("llm-2")) {
		t.Error("текстовый бэкенд на ДРУГОЙ карте того же хоста блокироваться не должен — " +
			"иначе multi-GPU хост простаивает")
	}
	if !p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(1)) {
		t.Error("gpuLockHeldFor(host, GPU1) = false, want true")
	}
	if p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(2)) {
		t.Error("gpuLockHeldFor(host, GPU2) = true, want false (лок взят на карте 1)")
	}

	// Освобождение не должно «залипать» на индексе: после Release карта свободна.
	holder.Release("test_done")
	if p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(1)) {
		t.Error("после Release лок на карте 1 всё ещё удерживается")
	}
}

// ЯВНЫЙ GPU 0 разводит бэкенды так же, как любой другой индекс (главное
// требование R-Image follow-up: первая карта объявляема, и на ней нагрузку
// можно разводить с картой 1).
func TestGPUIndexLock_ExplicitGPUZeroSeparatesCards(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{},
		secondHostBackend("llm-2", "127.0.0.1", knownGPU(1)))
	setGPUIndex(t, p, "img-1", 0) // явно ПЕРВАЯ карта
	setGPUIndex(t, p, "llm-1", 0)
	setGPUIndex(t, p, "llm-2", 1)

	holder := acquireImageLockForBackend(t, p, "img-1")
	defer holder.Release("test_done")

	if holder.key != "127.0.0.1#gpu0" {
		t.Errorf("ключ лока = %q, want %q (явный GPU 0 обязан давать индексный ключ, "+
			"иначе первая карта снова блокирует весь хост)", holder.key, "127.0.0.1#gpu0")
	}
	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Error("текстовый бэкенд на GPU 0 обязан блокироваться генерацией на GPU 0")
	}
	if p.imageGateBlocksTextBackend(p.GetBackendState("llm-2")) {
		t.Error("текстовый бэкенд на GPU 1 НЕ должен блокироваться генерацией на GPU 0 — " +
			"ровно ради этого случая индекс 0 стал объявляемым")
	}
	if !p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(0)) {
		t.Error("gpuLockHeldFor(host, GPU0) = false, want true")
	}
	if p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(1)) {
		t.Error("gpuLockHeldFor(host, GPU1) = true, want false (лок взят на карте 0)")
	}
}

// Два запроса к одной и той же ЯВНО объявленной карте 0 конфликтуют между
// собой (0 — не «пустой» индекс, а полноценная карта).
func TestGPUIndexLock_ExplicitGPUZeroConflictsWithItself(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.imageRes.queueWaitOverride = 100 * time.Millisecond
	setGPUIndex(t, p, "img-1", 0)

	first := acquireImageLockForBackend(t, p, "img-1")
	if _, ok, _ := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", knownGPU(0), "m"); ok {
		t.Fatal("повторный захват явного GPU 0 прошёл молча — exclusive не работает на первой карте")
	}
	first.Release("test_done")

	// И текстовый бэкенд, объявивший карту 0, тоже обязан считаться занятым.
	llm := *p.GetBackend("llm-1")
	llm.GPUIndex = types.GPUIndexPtr(0)
	if err := p.UpdateBackend("llm-1", llm); err != nil {
		t.Fatalf("UpdateBackend: %v", err)
	}
	second := acquireImageLockForBackend(t, p, "img-1")
	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Error("текстовый бэкенд на GPU 0 не заблокирован локом на GPU 0")
	}
	second.Release("test_done")
}

// Две image-генерации на разных картах одного хоста не мешают друг другу.
func TestGPUIndexLock_TwoCardsAcquireIndependently(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{},
		types.Backend{
			ID: "img-2", Name: "image worker 2", Host: "127.0.0.1", ImagePort: 18094,
			Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 2,
		})
	setGPUIndex(t, p, "img-1", 1)
	setGPUIndex(t, p, "img-2", 2)

	first := acquireImageLockForBackend(t, p, "img-1")
	defer first.Release("test_done")

	start := time.Now()
	second := acquireImageLockForBackend(t, p, "img-2")
	second.Release("test_done")
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Errorf("лок на другой карте ждал %v — карты не разделены", waited)
	}
}

// Индекс не задан хотя бы у одной стороны → блокируется весь хост (текущее
// поведение сохраняется: ослаблять защиту на «неизвестно» нельзя).
func TestGPUIndexLock_NoIndexBlocksWholeHost(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{},
		secondHostBackend("llm-2", "127.0.0.1", knownGPU(2))) // у llm-2 индекс есть, у img-1 и llm-1 — нет

	holder := acquireImageLockForBackend(t, p, "img-1") // индекс неизвестен
	defer holder.Release("test_done")

	if holder.key != "127.0.0.1" {
		t.Errorf("ключ лока = %q, want %q (при неизвестном индексе лок хостовый)", holder.key, "127.0.0.1")
	}
	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Error("без индекса у image-бэкенда весь хост обязан блокироваться")
	}
	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-2")) {
		t.Error("лок с неизвестным индексом обязан блокировать и индексированный бэкенд " +
			"(какая карта занята — неизвестно)")
	}
	// Сброшенный индекс (nil) ведёт себя как «неизвестно» и после того, как
	// индекс был задан: PUT с null снимает сужение лока.
	clearGPUIndex(t, p, "img-1")
	if got := p.GetBackend("img-1").GPUIndex; got != nil {
		t.Errorf("после clearGPUIndex GPUIndex = %d, want nil", *got)
	}
	if !p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(2)) {
		t.Error("хостовый лок (индекс сброшен) обязан блокировать и карту 2")
	}
}

// Обратный случай: индекс есть у image, а у текстового — нет → тоже весь хост.
func TestGPUIndexLock_UnknownOnTextSideBlocksHost(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{},
		secondHostBackend("llm-2", "127.0.0.1", unknownGPU()))
	setGPUIndex(t, p, "img-1", 1)

	holder := acquireImageLockForBackend(t, p, "img-1")
	defer holder.Release("test_done")

	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-2")) {
		t.Error("текстовый бэкенд без индекса обязан считаться «занимающим весь хост»")
	}
}

// Хостовый лок и индексированный конфликтуют между собой в обе стороны.
func TestGPUIndexLock_HostWideAndIndexedConflict(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.imageRes.queueWaitOverride = 100 * time.Millisecond // не ждать 30 с в тесте

	// Хостовый лок держит img-1 (индекс не задан).
	hostWide := acquireImageLock(t, p, "img-1", "127.0.0.1", unknownGPU())
	if _, ok, _ := p.imageRes.acquireLock(context.Background(), "img-2", "127.0.0.1", knownGPU(2), "m"); ok {
		t.Fatal("индексированный лок взят при удерживаемом хостовом — карты не пересекаются")
	}
	// Явный GPU 0 — тот же случай: неизвестный лок блокирует и первую карту.
	if _, ok, _ := p.imageRes.acquireLock(context.Background(), "img-2", "127.0.0.1", knownGPU(0), "m"); ok {
		t.Fatal("лок на явный GPU 0 взят при удерживаемом хостовом — часть хоста занята")
	}
	hostWide.Release("test_done")

	// Теперь наоборот: держим карту 2, хостовый лок обязан ждать/отказать.
	indexed := acquireImageLock(t, p, "img-2", "127.0.0.1", knownGPU(2))
	if _, ok, _ := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", unknownGPU(), "m"); ok {
		t.Fatal("хостовый лок взят при удерживаемом локе карты — часть хоста занята")
	}
	indexed.Release("test_done")
}

// Повторный захват ТОГО ЖЕ ключа обязан конфликтовать (самый частый случай:
// один хост, индексы не заданы — регрессия, поймана
// TestImageLock_ExclusiveSecondRequestRejected).
func TestGPUIndexLock_SameKeyConflicts(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.imageRes.queueWaitOverride = 100 * time.Millisecond

	first := acquireImageLock(t, p, "img-1", "127.0.0.1", unknownGPU())
	if _, ok, waited := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", unknownGPU(), "m"); ok {
		t.Fatal("повторный захват того же (host, gpu) прошёл молча — exclusive не работает")
	} else if waited < 50*time.Millisecond {
		t.Errorf("второй захват не ждал освобождения (waited=%v)", waited)
	}
	first.Release("test_done")
	if p.imageRes.gpuLockHeldFor("127.0.0.1", unknownGPU()) {
		t.Error("после Release карта всё ещё занята")
	}

	// И с явным индексом — тоже.
	indexed := acquireImageLock(t, p, "img-1", "127.0.0.1", knownGPU(2))
	if _, ok, _ := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", knownGPU(2), "m"); ok {
		t.Fatal("повторный захват того же индекса прошёл молча")
	}
	indexed.Release("test_done")
}

// Параллельные захваты (цель для -race): один ключ — не больше одного
// владельца, две карты — не больше двух одновременных владельцев.
func TestGPUIndexLock_ConcurrentAcquire(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.imageRes.queueWaitOverride = 50 * time.Millisecond

	const workers = 12
	var held, maxHeld int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gpu := knownGPU(1 + i%2) // чередуем карты 1 и 2 одного хоста
			h, ok, _ := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", gpu, "m")
			if !ok {
				return // не дождался за queueWait — нормальный исход для теста
			}
			cur := atomic.AddInt32(&held, 1)
			for {
				old := atomic.LoadInt32(&maxHeld)
				if cur <= old || atomic.CompareAndSwapInt32(&maxHeld, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&held, -1)
			h.Release("test_done")
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxHeld); got > 2 {
		t.Errorf("одновременно лок держали %d запросов, а карт всего 2 — ключ не разделяет GPU", got)
	}
	if p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(1)) || p.imageRes.gpuLockHeldFor("127.0.0.1", knownGPU(2)) {
		t.Error("после завершения всех запросов лок остался удерживаемым")
	}
}

// ============================================================
// 2. Лимит ожидания текстового запроса
// ============================================================

// newWaitCapProxy — стенд: image-лок 1 с (из настроек), admission-лимит задаётся
// тестом. queueWaitOverride хелпера сбрасывается, иначе настройка не проверялась бы.
func newWaitCapProxy(t *testing.T, admissionWait time.Duration, extra ...types.Backend) *Proxy {
	t.Helper()
	p, _ := newImgResProxy(t, types.ImageResourceSettings{QueueWaitTimeoutSec: 1}, extra...)
	p.imageRes.queueWaitOverride = 0
	p.setAdmissionWait(admissionWait)
	return p
}

// Единственная причина отсутствия кандидата — image-лок → ждём image-лимит, а не
// LB_ADMISSION_WAIT_SEC, и получаем штатный 503 очереди с причиной.
func TestImageLockWaitCap_SoleBlockerCapsWait(t *testing.T) {
	p := newWaitCapProxy(t, 3*time.Second)
	// Модель нигде не загружена (у llm-1 нет метрик) → P1/P2 пусты, и
	// единственный кандидат отсеян image-локом.
	holder := acquireImageLock(t, p, "img-1", "127.0.0.1", unknownGPU())
	defer holder.Release("test_done")

	if capped, ok := p.imageLockWaitCap("m1", types.BackendTypeLlamaCpp); !ok || capped != time.Second {
		t.Fatalf("imageLockWaitCap = %v,%v; want 1s,true (image-лок — единственная причина)", capped, ok)
	}

	start := time.Now()
	_, release, waited, position, err := p.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeLlamaCpp, "X-User-Id:cap")
	elapsed := time.Since(start)

	if release != nil {
		t.Error("при таймауте release обязан быть nil")
	}
	if !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("ожидался штатный 503 очереди (errAdmissionTimeout), получено %v", err)
	}
	if position == 0 {
		t.Error("position = 0 — запрос не встал в очередь")
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("ожидание %v: лимит НЕ сужен до balancing.image.queueWaitTimeoutSec (1 c), "+
			"admission-лимит был 3 c", elapsed)
	}
	if waited < 900*time.Millisecond {
		t.Errorf("waited = %v, ожидалось ≈ image-лимит (1 c)", waited)
	}
	if got := p.imageRes.textWaitCapped.Load(); got != 1 {
		t.Errorf("text_wait_capped_total = %d, want 1 (метрика причины)", got)
	}
}

// 503 обязан отчитаться ФАКТИЧЕСКИ применённым лимитом и назвать причину.
func TestWriteAdmissionUnavailable_ReportsAppliedLimitAndReason(t *testing.T) {
	capped := httptest.NewRecorder()
	writeAdmissionUnavailable(capped, "m1", "sess", 3, time.Second, 5*time.Second)
	if got := capped.Header().Get("X-Queue-Wait-Max-Sec"); got != "1" {
		t.Errorf("X-Queue-Wait-Max-Sec = %q, want 1 (применённый лимит, а не настроенные 5 c)", got)
	}
	if got := capped.Header().Get("X-Queue-Wait-Reason"); got != imageLockWaitReason {
		t.Errorf("X-Queue-Wait-Reason = %q, want %q", got, imageLockWaitReason)
	}
	body := capped.Body.String()
	for _, want := range []string{`"waitReason":"image_gpu_lock"`, `"waitMaxSec":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("тело 503 не содержит %s: %s", want, body)
		}
	}
	if got := capped.Header().Get("Retry-After"); got == "" {
		t.Error("503 без Retry-After — клиент не поймёт, когда повторять")
	}

	// Обычный случай (waited == предел): причина не выдумывается.
	plain := httptest.NewRecorder()
	writeAdmissionUnavailable(plain, "m1", "sess", 3, 5*time.Second, 5*time.Second)
	if got := plain.Header().Get("X-Queue-Wait-Reason"); got != "" {
		t.Errorf("X-Queue-Wait-Reason = %q, want пусто (лимит не сужался)", got)
	}
	if got := plain.Header().Get("X-Queue-Wait-Max-Sec"); got != "5" {
		t.Errorf("X-Queue-Wait-Max-Sec = %q, want 5", got)
	}
	if strings.Contains(plain.Body.String(), "waitReason") {
		t.Errorf("в обычном 503 появился waitReason: %s", plain.Body.String())
	}
}

// Причина иная (нет здоровых текстовых бэкендов) → лимит НЕ сужается.
func TestImageLockWaitCap_NoHealthyCandidatesUnchanged(t *testing.T) {
	p := newWaitCapProxy(t, 2*time.Second)
	holder := acquireImageLock(t, p, "img-1", "127.0.0.1", unknownGPU())
	defer holder.Release("test_done")
	p.UpdateBackendStatus("llm-1", types.StatusUnhealthy)

	if capped, ok := p.imageLockWaitCap("m1", types.BackendTypeLlamaCpp); ok {
		t.Fatalf("imageLockWaitCap = %v при отсутствии здоровых кандидатов; want «не применять»", capped)
	}

	start := time.Now()
	_, _, _, _, err := p.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeLlamaCpp, "X-User-Id:nohealth")
	elapsed := time.Since(start)

	if !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("ожидался errAdmissionTimeout, получено %v", err)
	}
	if elapsed < 1800*time.Millisecond {
		t.Errorf("ожидание %v: лимит сужен там, где причина иная (нет здоровых бэкендов)", elapsed)
	}
	if got := p.imageRes.textWaitCapped.Load(); got != 0 {
		t.Errorf("text_wait_capped_total = %d, want 0", got)
	}
}

// Слоты кандидата заняты → причина ожидания ёмкость (обычная перегрузка), а не
// лок: лимит не сужается, даже если кандидат ещё и заблокирован локом.
func TestImageLockWaitCap_BusySlotsIsNotCapped(t *testing.T) {
	p := newWaitCapProxy(t, 2*time.Second)
	// Метрики есть (иначе бэкенд выпал бы из кандидатов по checkResourceLimits),
	// модель загружена, но ёмкость = 1 и она занята.
	p.SetBackendMetrics("llm-1", &types.BackendMetrics{
		ID:        "llm-1",
		Timestamp: time.Now(),
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: "m1"}},
		},
	})
	one := *p.GetBackend("llm-1")
	one.MaxConcurrentReqs = 1
	if err := p.UpdateBackend("llm-1", one); err != nil {
		t.Fatalf("UpdateBackend: %v", err)
	}

	holder := acquireImageLock(t, p, "img-1", "127.0.0.1", unknownGPU())
	defer holder.Release("test_done")

	if !p.tryAcquireSlot("llm-1") {
		t.Fatal("не удалось занять слот llm-1")
	}
	defer p.releaseSlot("llm-1")

	if capped, ok := p.imageLockWaitCap("m1", types.BackendTypeLlamaCpp); ok {
		t.Fatalf("imageLockWaitCap = %v при занятых слотах; want «не применять» "+
			"(причина ожидания — ёмкость, а не image-лок)", capped)
	}

	start := time.Now()
	_, _, _, _, err := p.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeLlamaCpp, "X-User-Id:loaded")
	elapsed := time.Since(start)

	if !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("ожидался errAdmissionTimeout, получено %v", err)
	}
	if elapsed < 1800*time.Millisecond {
		t.Errorf("ожидание %v: обычная перегрузка слотов сужена, хотя причина не в image-локе", elapsed)
	}
	if got := p.imageRes.textWaitCapped.Load(); got != 0 {
		t.Errorf("text_wait_capped_total = %d, want 0", got)
	}
}

// tryAcquireAnyBackendSlotDetailed обязан различать «кандидатов нет» и «слот
// занят» — на этом различии и построено сужение лимита.
func TestTryAcquireAnyBackendSlotDetailed_NoCandidateFlag(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	// Метрики нужны, чтобы checkResourceLimits не выбросил бэкенд из кандидатов
	// при занятом слоте (без метрик он отвечает false при active >= max).
	proxy.SetBackendMetrics("ollama-1", &types.BackendMetrics{ID: "ollama-1", Timestamp: time.Now()})

	id, noCandidate := proxy.tryAcquireAnyBackendSlotDetailed("m1", types.BackendTypeOllama)
	if id != "ollama-1" || noCandidate {
		t.Fatalf("свободный слот: id=%q noCandidate=%v, want ollama-1,false", id, noCandidate)
	}
	id, noCandidate = proxy.tryAcquireAnyBackendSlotDetailed("m1", types.BackendTypeOllama)
	if id != "" || noCandidate {
		t.Fatalf("занятый слот: id=%q noCandidate=%v — занятость слота НЕ должна выглядеть "+
			"как «нет кандидатов» (иначе сузили бы лимит при обычной перегрузке)", id, noCandidate)
	}
	proxy.releaseSlot("ollama-1")

	proxy.UpdateBackendStatus("ollama-1", types.StatusOffline)
	id, noCandidate = proxy.tryAcquireAnyBackendSlotDetailed("m1", types.BackendTypeOllama)
	if id != "" || !noCandidate {
		t.Fatalf("нет здоровых кандидатов: id=%q noCandidate=%v, want \"\",true", id, noCandidate)
	}
}
