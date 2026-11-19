// image_resources_snapshot.go — R633 (2026-11-19): снимки для монитора: watchAsyncGeneration, clusterSnapshot, snapshotMetrics.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"time"
)

// watchAsyncGeneration — сторож асинхронной генерации.
//
// Зачем: нативный POST /api/image/generate отвечает 202 сразу, а генерация
// продолжается в воркере. Освободить лок в момент ответа (как для
// синхронных /v1/images/* и /sdapi/v1/*) означало бы пустить текст на GPU
// ВНУТРЬ генерации. Сторож опрашивает active_queries воркера и снимает лок,
// когда генерация закончилась (или по предохранителю/остановке).
//
// Phase 8: сторож же закрывает и запись в метриках (req). Исход — finished, а не
// ok: воркер отдаёт только active_queries, per-job результат балансеру
// недоступен, и выдавать «закончилось» за «успешно» было бы враньём. Если воркер
// пропал во время генерации — это failed (запись не останется «в полёте»
// навсегда).
func (r *imageResources) watchAsyncGeneration(h *imageLockHolder, req *imageRequestHandle) {
	if r == nil || h == nil {
		return
	}
	h.HandOff()
	go func() {
		defer h.Release("async_generation_finished")
		defer req.finish(types.ImageRequestStatusFinished, http.StatusAccepted, "", "", 0)
		activeSeen := false
		idle := 0
		pending := 0
		failures := 0
		ticker := time.NewTicker(imageAsyncWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
			}

			ctx, cancel := context.WithTimeout(context.Background(), r.probeTimeout())
			snap := r.refresh(ctx, h.backendID)
			cancel()

			if snap == nil || !snap.contractOK || snap.lastErr != "" {
				failures++
				if failures >= imageAsyncMaxPollErrors {
					logger.Get().Warnw("async image generation watch: worker unreachable, releasing GPU lock",
						"backend", h.backendID, "host", h.host, "failures", failures)
					// Воркер пропал: генерация не завершилась — фиксируем failed
					// (idempotent finish, поэтому defer ниже уже не сработает).
					req.finish(types.ImageRequestStatusFailed, 0, "image_worker_unreachable",
						"worker unreachable during async generation", 0)
					return
				}
				continue
			}
			failures = 0

			if snap.activeQueries() > 0 {
				activeSeen = true
				idle = 0
				continue
			}
			if activeSeen {
				idle++
				if idle >= imageAsyncIdlePolls {
					logger.Get().Infow("async image generation finished (active_queries=0)",
						"backend", h.backendID, "host", h.host)
					return
				}
				continue
			}
			// Генерация ещё не началась (или уже закончилась мгновенно) —
			// ждём старта ограниченное время, чтобы не держать лок вечно.
			pending++
			if pending >= imageAsyncStartupGracePolls {
				logger.Get().Infow("async image generation watch: no activity observed, releasing GPU lock",
					"backend", h.backendID, "host", h.host, "polls", pending)
				return
			}
		}
	}()
}

// ============================================================
// Наблюдаемость
// ============================================================

// snapshotMetrics — срез состояния Phase 6 для /api/metrics и диагностики.
// clusterSnapshot — снимок состояния image-бэкенда для WebUI/Monitor
// (types.BackendMetrics.Image).
//
// R-Image (2026-10-02): данные уже собираются для гейта VRAM, поэтому UI
// получает их без дополнительных запросов. nil — если по бэкенду нет снимка
// (например, он только что зарегистрирован и поллер ещё не успел).
func (r *imageResources) clusterSnapshot(backendID string) *types.ImageBackendMetrics {
	if r == nil || backendID == "" {
		return nil
	}
	r.mu.Lock()
	s := r.snapshots[backendID]
	r.mu.Unlock()
	if s == nil {
		return nil
	}

	out := &types.ImageBackendMetrics{
		State:        s.state,
		CurrentModel: s.currentModel,
		VramFreeMB:   s.vramFreeMB,
		VramTotalMB:  s.vramTotalMB,
		UpdatedAt:    s.at,
		LastError:    s.lastErr,
	}
	// Phase 8: поток запросов (агрегаты + лента последних). UI показывает их
	// рядом с состоянием воркера — иначе по таблице невозможно понять, идут ли
	// на бэкенд генерации и с каким исходом.
	out.Requests = r.imageRequests().snapshot(backendID)
	out.Recent = r.imageRequests().recentFor(backendID, imageRecentLimitPerBackend)
	// Модели отдаём ВСЕ (включая незагруженные): оператору нужно видеть, что
	// вообще установлено и что можно загрузить, а не только текущую модель.
	for _, m := range s.models {
		out.Models = append(out.Models, types.ImageModelBrief{
			Name:           m.Name,
			State:          m.State,
			Family:         m.Family,
			SizeBytes:      m.SizeBytes,
			VramEstimateMB: m.VramEstimateMB,
			ActiveQueries:  m.ActiveQueries,
			Error:          m.Error,
		})
	}
	return out
}

func (r *imageResources) snapshotMetrics() map[string]interface{} {
	if r == nil {
		return map[string]interface{}{}
	}
	settings := r.settings()

	r.mu.Lock()
	held := 0
	for _, lk := range r.locks {
		if lk != nil && lk.held {
			held++
		}
	}
	byReason := make(map[string]int64, len(r.gateDenied))
	for k, v := range r.gateDenied {
		byReason[k] = v
	}
	perBackend := make(map[string]interface{}, len(r.snapshots))
	known := make(map[string]bool, len(r.snapshots))
	for id, s := range r.snapshots {
		known[id] = true
		if s == nil {
			continue
		}
		entry := map[string]interface{}{
			"state":            s.state,
			"contract_ok":      s.contractOK,
			"current_model":    s.currentModel,
			"active_queries":   s.activeQueries(),
			"vram_free_mb":     s.vramFreeMB,
			"vram_total_mb":    s.vramTotalMB,
			"vram_source":      s.vramSource,
			"snapshot_age_sec": int(time.Since(s.at).Seconds()),
		}
		// Phase 8: поток запросов по этому воркеру (total/ok/failed/rejected,
		// in_flight, rps, длительности, отказы по кодам).
		if req := r.imageRequests().snapshotMap(id); req != nil {
			entry["requests"] = req
		}
		if m := s.loaded(); m != nil {
			entry["loaded_model"] = m.Name
			entry["loaded_vram_estimate_mb"] = m.VramEstimateMB
			entry["loaded_bundle_bytes"] = m.SizeBytes
		}
		if s.lastErr != "" {
			entry["error"] = s.lastErr
		}
		perBackend[id] = entry
	}
	r.mu.Unlock()

	// Предохранитель от роста карты счётчиков (см. imageRequestStore.prune).
	r.imageRequests().prune(known, time.Now())

	waits := r.lockWaits.Load()
	avgWaitMS := int64(0)
	if waits > 0 {
		avgWaitMS = r.lockWaitSumMS.Load() / waits
	}

	// Строгость к неизвестной оценке НЕ читаем из поля напрямую: спросим
	// замороженный контракт. Так отчёт метрик не зависит от имени настройки
	// (FIX-2 переводит её в opt-in BlockOnUnknownVRAMEstimate) и всегда
	// показывает ФАКТИЧЕСКОЕ поведение гейта.
	unknownBlocked := !types.EvaluateImageVRAM(types.ImageVramEstimate{}, 0, settings).Allowed

	return map[string]interface{}{
		"coexistence_policy":         string(settings.EffectiveCoexistencePolicy()),
		"gate_disabled":              settings.GateDisabled,
		"vram_headroom_mb":           settings.VramHeadroomMB,
		"unknown_estimate_blocked":   unknownBlocked,
		"queue_wait_timeout_sec":     int(r.queueWait().Seconds()),
		"exclusive_lock_timeout_sec": int(r.lockFuse().Seconds()),
		"gate_allowed_total":         r.gateAllowed.Load(),
		"gate_denied_total":          r.gateDeniedTotal.Load(),
		"gate_denied_by_reason":      byReason,
		"gate_skipped_no_data_total": r.gateSkippedNoData.Load(),
		"lock_acquired_total":        r.lockAcquired.Load(),
		"lock_held":                  held,
		"lock_wait_total":            waits,
		"lock_wait_ms_avg":           avgWaitMS,
		"lock_wait_time_histogram":   r.lockWaitHist.Snapshot(),
		"lock_rejected_total":        r.lockRejected.Load(),
		"lock_force_released_total":  r.lockForced.Load(),
		"text_slots_withheld_total":  r.textSlotsWithheld.Load(),
		// Phase 6 follow-up (п.2): ожидание текстового запроса урезано до
		// balancing.image.queueWaitTimeoutSec (единственная причина — image-лок).
		"text_wait_capped_total": r.textWaitCapped.Load(),
		// Phase 8: поток запросов ко ВСЕМУ image-пулу (агрегат + общая лента
		// последних запросов). Per-backend счётчики — в "backends" выше.
		"requests": r.imageRequests().aggregateMap(imageRecentLimitGlobal),
		"backends": perBackend,
	}
}
