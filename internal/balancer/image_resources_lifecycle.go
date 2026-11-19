// image_resources_lifecycle.go — R633 (2026-11-19): жизненный цикл опроса бэкендов изображений: доступ к ресурсам, настройки и таймауты, Start/Stop/loop, опрос и кэш.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"sync"
	"sync/atomic"
	"time"
)

// imageResources — nil-safe доступ к состоянию Phase 6 (тип imageResources)
// у Proxy.
//
// nil-safe: тесты и внутренние вызовы иногда работают с Proxy, собранным
// вручную (&Proxy{}), где NewProxy не выполнялся. Тогда гейт просто не
// применяется (нет состояния — нет и решения), а не паникует.
func (p *Proxy) imageResources() *imageResources {
	if p == nil {
		return nil
	}
	return p.imageRes
}

// imageResources — состояние Phase 6 у Proxy: кэш метрик image-бэкендов,
// ресурсные локи GPU и счётчики наблюдаемости.
//
// Один экземпляр на Proxy (создаётся в NewProxy). Отдельная структура, а не
// поля Proxy, — чтобы не раздувать Proxy и держать всю логику Phase 6 в одном
// файле (см. договорённость о зонах правок).
type imageResources struct {
	proxy        *Proxy
	profiles     *config.ImageModelProfileStore
	lockWaitHist *histogramCollector
	stopCh       chan struct{}

	// imgReq — счётчики и лента image-запросов (Phase 8). Живут здесь же, а не
	// в Proxy: это та же зона ответственности (наблюдаемость image-пула), и
	// UI получает их тем же снимком, что и состояние воркеров.
	imgReq *imageRequestStore

	snapshots  map[string]*imageBackendMetrics
	locks      map[string]*imageHostLock
	gateDenied map[string]int64

	mu        sync.Mutex
	profileMu sync.Mutex
	stopOnce  sync.Once

	gateAllowed       atomic.Int64
	gateDeniedTotal   atomic.Int64
	lockAcquired      atomic.Int64
	lockRejected      atomic.Int64
	lockForced        atomic.Int64
	lockWaits         atomic.Int64
	lockWaitSumMS     atomic.Int64
	textSlotsWithheld atomic.Int64
	// textWaitCapped — сколько раз ожидание ТЕКСТОВОГО запроса было урезано до
	// balancing.image.queueWaitTimeoutSec, потому что единственная причина
	// отсутствия слота — активный image-лок (Phase 6 follow-up, п.2).
	textWaitCapped    atomic.Int64
	gateSkippedNoData atomic.Int64
	started           atomic.Bool

	profilesAt           time.Time
	queueWaitOverride    time.Duration
	lockFuseOverride     time.Duration
	probeTimeoutOverride time.Duration
	staleAfterOverride   time.Duration
	pollIntervalOverride time.Duration
}

// newImageResources — конструктор. Поллер НЕ стартует здесь: Start() вызывает
// NewProxy (как для llamaCppMetricsPoller).
func newImageResources(proxy *Proxy) *imageResources {
	return &imageResources{
		proxy:        proxy,
		imgReq:       newImageRequestStore(),
		snapshots:    make(map[string]*imageBackendMetrics),
		locks:        make(map[string]*imageHostLock),
		gateDenied:   make(map[string]int64),
		lockWaitHist: newHistogramCollector(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30),
		stopCh:       make(chan struct{}),
	}
}

// imageRequests — счётчики image-запросов (nil-safe).
func (r *imageResources) imageRequests() *imageRequestStore {
	if r == nil {
		return nil
	}
	return r.imgReq
}

// ImageMetricsSummary — блок `image` для GET /api/v1/metrics: политика
// сосуществования, состояние гейта и лока, счётчики запросов по пулу и по
// каждому image-бэкенду.
//
// ЗАЧЕМ ЭКСПОРТ. `internal/api` собирает /api/v1/metrics СВОИМ ответом
// (handlers_metrics.go: timestamp/backends/placement/autoContinue) и этот блок в
// него не попадал: страница «Image-бэкенды» и внешние потребители не могли
// узнать фактическую политику (coexistence_policy, vram_headroom_mb,
// queue_wait_timeout_sec) и поток запросов. Дублировать сборку в API-слое не
// нужно — отдаём готовый снимок.
func (p *Proxy) ImageMetricsSummary() map[string]interface{} {
	if p == nil {
		return nil
	}
	res := p.imageResources()
	if res == nil {
		return nil
	}
	return res.snapshotMetrics()
}

// settings — секция balancing.image из конфига (nil-safe).
func (r *imageResources) settings() types.ImageResourceSettings {
	if r == nil || r.proxy == nil || r.proxy.config == nil {
		return types.ImageResourceSettings{}
	}
	return r.proxy.config.Balancing.Image
}

// queueWait — сколько exclusive-режим ждёт освобождения GPU.
func (r *imageResources) queueWait() time.Duration {
	if r != nil && r.queueWaitOverride > 0 {
		return r.queueWaitOverride
	}
	return time.Duration(r.settings().EffectiveQueueWaitTimeout()) * time.Second
}

// lockFuseWarnOnce — WARN про взведённый предохранитель печатаем ОДИН раз за
// процесс: lockFuse() вызывается на каждую генерацию, а оператор должен увидеть
// причину «почему оборвалось» ровно один раз — без спама в логе.
var lockFuseWarnOnce sync.Once

// lockFuse — предохранитель «лок нельзя держать вечно».
//
// R88 (2026-10-08): по умолчанию СНЯТ (0 = без будильника). Доктрина
// (timeout_policy.go) запрещает duration-кап на работу: при 600 с лок снимался
// посреди легитимной генерации (2048x2048/40 шагов = 22m30s на стенде) и
// пускал текстовый трафик на занятую карту. Взведённый кап печатает WARN.
func (r *imageResources) lockFuse() time.Duration {
	if r != nil && r.lockFuseOverride > 0 {
		return r.lockFuseOverride
	}
	d := time.Duration(r.settings().EffectiveExclusiveLockTimeout()) * time.Second
	if d > 0 {
		lockFuseWarnOnce.Do(func() {
			if log := logger.Get(); log != nil {
				log.Warnw("image GPU lock fuse armed (operator opt-in)",
					"timeout_sec", int(d.Seconds()),
					"hint", i18n.T("balancer.image_resources.lock_fuse_hint"))
			}
		})
	}
	return d
}

// probeTimeout — таймаут одного HTTP-опроса воркера.
func (r *imageResources) probeTimeout() time.Duration {
	if r != nil && r.probeTimeoutOverride > 0 {
		return r.probeTimeoutOverride
	}
	return imageMetricsProbeTimeout
}

// staleAfter — возраст снимка, после которого гейт обновляет его синхронно.
func (r *imageResources) staleAfter() time.Duration {
	if r != nil && r.staleAfterOverride > 0 {
		return r.staleAfterOverride
	}
	return imageMetricsStaleAfter
}

// interval — текущий интервал поллера (быстрый, пока модель грузится).
// Настраивается тестами через pollIntervalOverride.
func (r *imageResources) interval() time.Duration {
	if r == nil {
		return imageMetricsInterval
	}
	if r.pollIntervalOverride > 0 {
		return r.pollIntervalOverride
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.snapshots {
		if s != nil && (s.loading() != nil || s.state == imageStateLoading) {
			return imageMetricsFastInterval
		}
	}
	return imageMetricsInterval
}

// ============================================================
// Поллер метрик
// ============================================================

// Start — запуск фонового поллера (идемпотентно).
func (r *imageResources) Start() {
	if r == nil || !r.started.CompareAndSwap(false, true) {
		return
	}
	go r.loop()
}

// Stop — остановка поллера (идемпотентно). NewProxy Shutdown не меняет —
// поллер сам завершается, увидев p.shuttingDown (см. loop).
func (r *imageResources) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
}

// stopped — запрошена ли остановка.
func (r *imageResources) stopped() bool {
	if r == nil {
		return true
	}
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// loop — цикл поллера.
//
// ПЕРВЫЙ ОПРОС — НЕ НЕМЕДЛЕННЫЙ, а через интервал (FIX-1 интеграции Phase 6).
// Почему: немедленный pollAll в Start() добавлял HTTP-запрос в image-воркер в
// момент старта балансера — это ломало тесты/скрипты, считающие обращения к
// /api/image/models (например internal/api TestImageBackendProxy_ModelsAlias
// ожидает ровно одно обращение), и дёргало сеть до того, как она реально нужна.
// Актуальность метрик К МОМЕНТУ ЗАПРОСА обеспечивает не поллер, а ленивая
// синхронная догрузка ensureFreshFor() перед гейтом — поэтому задержка первого
// тика на корректность решений не влияет.
//
// Бэкендов без типа image_cpp цикл не трогает вообще: pollAll выходит до любого
// сетевого вызова (см. проверку filterBackendsByType).
func (r *imageResources) loop() {
	for {
		if r.pollingDone() {
			return
		}
		// Спим «кусками»: Stop/shutdown замечаются в пределах imageMetricsPollSlice.
		deadline := time.Now().Add(r.interval())
		for time.Now().Before(deadline) {
			if r.pollingDone() {
				return
			}
			slice := time.Until(deadline)
			if slice > imageMetricsPollSlice {
				slice = imageMetricsPollSlice
			}
			timer := time.NewTimer(slice)
			select {
			case <-r.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if r.pollingDone() {
			return
		}
		r.pollAll(context.Background())
	}
}

// pollingDone — пора ли завершать цикл (Stop или Shutdown прокси).
func (r *imageResources) pollingDone() bool {
	if r.stopped() {
		return true
	}
	return r.proxy != nil && r.proxy.shuttingDown.Load()
}

// pollAll — параллельный опрос всех ЗДОРОВЫХ image-бэкендов.
func (r *imageResources) pollAll(ctx context.Context) {
	if r == nil || r.proxy == nil {
		return
	}
	backends := r.proxy.filterBackendsByType(types.BackendTypeImage)
	if len(backends) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, st := range backends {
		if st == nil || st.Backend == nil {
			continue
		}
		id := st.Backend.ID
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r.refresh(ctx, id)
		}(id)
	}
	wg.Wait()
}

// refresh — опросить бэкенд и опубликовать новый снимок.
//
// Ошибка опроса НЕ стирает последний снимок (иначе UI/логи потеряли бы модель),
// но помечает его lastErr: гейт по недостоверному снимку не судит.
func (r *imageResources) refresh(ctx context.Context, backendID string) *imageBackendMetrics {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snap, err := r.fetchModels(ctx, backendID)
	if err != nil {
		failed := r.metricsSnapshot(backendID)
		cp := imageBackendMetrics{backendID: backendID}
		if failed != nil {
			cp = *failed
		}
		cp.at = time.Now()
		cp.lastErr = err.Error()
		r.store(&cp)
		logger.Get().Debugw("image metrics poll failed",
			"backend", backendID, "error", err)
		return &cp
	}
	if snap != nil {
		r.store(snap)
	}
	return snap
}

// store — публикация снимка (под mu; снимок иммутабелен после этого).
func (r *imageResources) store(s *imageBackendMetrics) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	r.snapshots[s.backendID] = s
	r.mu.Unlock()
}

// metricsSnapshot — текущий снимок бэкенда (nil, если опроса ещё не было).
func (r *imageResources) metricsSnapshot(backendID string) *imageBackendMetrics {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshots[backendID]
}

// ensureFreshFor — снимок для решения гейта: свежий из кэша либо ЛЕНИВАЯ
// СИНХРОННАЯ догрузка с коротким таймаутом (imageMetricsProbeTimeout).
//
// ЗАЧЕМ ЛЕНИВАЯ ДОГРУЗКА, ЕСЛИ ЕСТЬ ПОЛЛЕР: поллер экономит сеть (первый тик —
// через интервал, дальше 30 с), но решение принимается per-request, и протухший
// (или отсутствующий) снимок стоит дорого в обе стороны — пропуск запроса к
// выгруженной модели или отказ рабочей генерации. Поэтому в момент запроса
// метрики при необходимости обновляются здесь; порог свежести — staleAfter
// (5 с), то есть подряд идущие генерации переиспользуют один опрос.
func (r *imageResources) ensureFreshFor(ctx context.Context, backendID string) *imageBackendMetrics {
	prev := r.metricsSnapshot(backendID)
	if prev != nil && prev.lastErr == "" && time.Since(prev.at) < r.staleAfter() {
		return prev
	}
	return r.refresh(ctx, backendID)
}
