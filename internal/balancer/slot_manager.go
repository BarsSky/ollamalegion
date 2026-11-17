package balancer

import (
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// tryAcquireSlot — атомарная проверка и захват слота на бэкенде.
// Возвращает true если слот успешно захвачен (ActiveReqs < MaxConcurrentReqs).
// Если все слоты заняты, проверяет наличие зомби-сессий на бэкенде
// и принудительно освобождает один слот (preemption).
// Вызывающий код обязан вызвать releaseSlot после завершения запроса.
func (p *Proxy) tryAcquireSlot(backendID string) bool {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.Backend.Status != types.StatusHealthy {
		return false
	}

	maxReqs := state.Backend.EffectiveMaxConcurrentRequests()

	if maxReqs > 0 && state.ActiveReqs >= maxReqs {
		// Все слоты заняты — проверяем, не заняты ли они зомби-сессиями.
		// Если на бэкенде есть зомби-сессия (HasActiveStream=true, но LastRequestAt
		// старше порога), принудительно освобождаем слот для нового запроса.
		//
		// R251 (2026-10-28): порог берём из конфига (AdvancedTiming), а не
		// хардкодом 60 с.
		//
		// ЗАЧЕМ. С R251 флаг «идёт стрим» выставляется и на llama.cpp-путях (до
		// этого он жил только на нативном Ollama-пути), а префилл большой модели с
		// CPU-оффлоадом легко даёт паузу больше минуты ДО первого токена. С порогом
		// 60 с живой запрос помечался зомби, его сессия удалялась, а ActiveReqs
		// уменьшался — слот «освобождался» дважды: живой запрос продолжал занимать
		// движок, а admission пускал на узел лишний запрос.
		zombies := p.sessionMgr.DetectZombieSessions(p.getZombieSessionThreshold())
		for _, zombie := range zombies {
			if zombie.BackendID == backendID {
				logger.Get().Warnw("preempting zombie session slot for new request",
					"backend", backendID,
					"zombie_session_id", zombie.ID,
					"zombie_last_request", zombie.LastRequestAt,
				)
				// Удаляем зомби-сессию и её слот будет переиспользован
				p.sessionMgr.ForceRemove(zombie.ID)
				// Уменьшаем ActiveReqs чтобы освободить место
				if state.ActiveReqs > 0 {
					state.ActiveReqs--
				}
				// Захватываем слот для нового запроса
				state.ActiveReqs++
				state.LastUsed = time.Now()
				return true
			}
		}
		// Нет зомби для preemption — действительно нет свободных слотов
		return false
	}

	state.ActiveReqs++
	state.LastUsed = time.Now()
	return true
}

// releaseSlot — освобождение слота на бэкенде.
func (p *Proxy) releaseSlot(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	state.mu.Lock()
	if state.ActiveReqs > 0 {
		state.ActiveReqs--
	}
	state.mu.Unlock()

	// R67b (2026-09-23): слот освободился — будим admission-очередь, иначе
	// ожидающие запросы просыпались бы только по poll-интервалу (250 мс).
	p.admission.broadcast()
}

// checkResourceLimits - проверка лимитов ресурсов (SOFT-режим).
//
// R284 (2026-10-29): ВНЕШНЯЯ обёртка. Внутри — checkResourceLimitsLocked, которая
// НЕ берёт p.mu и требует, чтобы вызывающий уже держал p.mu (RLock).
//
// ЗАЧЕМ РАЗДЕЛЕНИЕ. Раньше эта функция брала p.mu.RLock сама, а её вызывали
// селекторы, которые УЖЕ держат p.mu.RLock (expandCandidates, selectByResources,
// selectBackendExcluding и ещё 9 мест). Получался рекурсивный RLock — а он не
// рекурсивен по контракту sync.RWMutex: если между первым и вторым RLock встаёт
// писатель, второй RLock не выдаётся НИКОГДА (писатель ждёт первого читателя,
// новые читатели ждут писателя). Живое следствие: под нагрузкой агентских метрик
// (каждый POST -> UpdateBackendAgentStatus -> p.mu.Lock) балансер вставал
// навсегда, при этом контейнер оставался «healthy», потому что healthcheck ходит
// на /api/v1/ping, который p.mu не трогает. Подробности и регрессионный тест —
// lock_deadlock_r284_test.go.
func (p *Proxy) checkResourceLimits(backendID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.checkResourceLimitsLocked(backendID)
}

// checkResourceLimitsLocked — та же проверка без захвата p.mu.
//
// ИНВАРИАНТ: вызывающий обязан держать p.mu (RLock или Lock). Внутри берутся
// только ЧУЖИЕ локи (metricsMgr.mu, state.mu) в порядке p.mu -> они, как и во
// всём пакете.
func (p *Proxy) checkResourceLimitsLocked(backendID string) bool {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	if !ok {
		// p.mu уже держит вызывающий — читаем карту напрямую, БЕЗ повторного
		// RLock (именно это повторение и вешало балансер).
		state, exists := p.backends[backendID]
		if !exists {
			return false
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs {
			return false
		}
		return true
	}

	limits := p.config.Resources
	hard := resourceLimitsHardBlockEnabled()

	// R118 (2026-10-14): МГНОВЕННАЯ УТИЛИЗАЦИЯ (VRAM-процент, GPU, CPU) больше НЕ
	// исключает здоровый бэкенд из кандидатов — она только логируется.
	//
	// ПОЧЕМУ. Живая жалоба: «второй запрос с той же машины не проходит», «нельзя
	// вести параллельную работу», «OpenWebUI получает 503». Разбор: checkResourceLimits
	// вызывается из 13 точек выбора бэкенда, и блокировка возвращала false —
	// бэкенд выпадал из кандидатов, запрос не вставал в admission-очередь, а
	// получал «all backends busy» (503). При этом на стенде модель загружена
	// частично на GPU, частично на CPU (16 слоёв), поэтому во время ЛЮБОЙ генерации
	// CPU ~96% и GPU ~100% — то есть занятый работой бэкенд блокировался сразу и для
	// всех: и второй экземпляр Cline, и OpenWebUI, и вторая модель.
	//
	// Это ещё и противоречило собственному контракту функции («SOFT-режим» в
	// комментарии) и дублировало admission: параллелизм уже ограничен слотами
	// (EffectiveMaxConcurrentRequests), и лишний запрос должен ЖДАТЬ в очереди, а не
	// получать 503. Процент занятой VRAM при загруженной модели — норма, а не
	// признак «нельзя обслуживать»: влезает ли НОВАЯ модель, решает путь загрузки
	// (preflight/memfit), а не допуск запроса к уже загруженной.
	//
	// Жёсткими остаются ФАКТЫ, при которых обслуживание физически невозможно:
	// критическая занятость RAM (своп/OOM) и нехватка места на диске. Их можно
	// вернуть к прежнему поведению флагом LB_RESOURCE_LIMITS_HARD_BLOCK=true.
	//
	// Headroom reservation: учитываем GPU headroom из конфигурации balancing
	headroom := p.config.Balancing.ResourceReservation.GPUHeadroomPercent
	if headroom > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > (100 - headroom) {
			if hard {
				logger.Get().Warnw("backend VRAM exceeds headroom reservation, blocking",
					"backend", backendID, "vram_percent", vramPercent, "headroom_pct", headroom)
				return false
			}
			// R488: мягкое наблюдение — не событие хода; без троттлинга оно вытесняло
			// из кольца логов строки request trace (живой архив: 733/2000 строк).
			if softLogAllowedR488(backendID, "vram_headroom", time.Now(), softLogEveryR488) {
				logger.Get().Infow(i18n.T("balancer.slot_manager.vram_above_headroom_soft_info"),
					"backend", backendID, "vram_percent", vramPercent, "headroom_pct", headroom)
			}
		}
	}

	// R499 / аудит S-2 (2026-11-19): порог GPU берётся из КОНФИГА, а не зашит.
	//
	// БЫЛО: `> 90` в коде, а Resources.GPU.MaxUsagePercent (LB_GPU_MAX_USAGE)
	// влиял только как выключатель (`> 0`): оператор менял число в настройках и
	// не видел эффекта. Значение по умолчанию — те же 90, поэтому поведение без
	// настройки не меняется.
	gpuMaxUsage := limits.GPU.MaxUsagePercent
	if gpuMaxUsage <= 0 {
		gpuMaxUsage = 90
	}
	if metrics.GPU.UsagePercent > gpuMaxUsage {
		if hard {
			logger.Get().Warnw("backend GPU usage critical, blocking", "backend", backendID,
				"gpu_usage", metrics.GPU.UsagePercent, "threshold", gpuMaxUsage)
			return false
		}
		if softLogAllowedR488(backendID, "gpu_busy", time.Now(), softLogEveryR488) {
			logger.Get().Infow(i18n.T("balancer.slot_manager.gpu_busy_soft_info"),
				"backend", backendID, "gpu_usage", metrics.GPU.UsagePercent, "threshold", gpuMaxUsage)
		}
	}

	// R499 / аудит S-3 (2026-11-19): у `Resources.GPU.MaxTemperature`
	// (LB_GPU_MAX_TEMP) НЕ БЫЛО ПОТРЕБИТЕЛЯ ВООБЩЕ — значение читалось из env,
	// попадало в конфиг и умирало там. Перегрев — тот самый факт «почему
	// медленно», который R495 S1/S2 видит в throttle-масках; здесь он закрывает
	// допуск при ЯВНО заданном пределе. По умолчанию границы нет: температура
	// зависит от карты и корпуса, выдумывать ей предел нельзя.
	if limits.GPU.MaxTemperature > 0 && metrics.GPU.Temperature > limits.GPU.MaxTemperature {
		if hard {
			logger.Get().Warnw("backend GPU temperature critical, blocking", "backend", backendID,
				"temperature", metrics.GPU.Temperature, "threshold", limits.GPU.MaxTemperature)
			return false
		}
		if softLogAllowedR488(backendID, "gpu_hot", time.Now(), softLogEveryR488) {
			logger.Get().Infow(i18n.T("balancer.slot_manager.gpu_hot_soft_info"),
				"backend", backendID, "temperature", metrics.GPU.Temperature,
				"threshold", limits.GPU.MaxTemperature)
		}
	}

	if limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > limits.GPU.MaxVRAMUsagePercent {
			// R493 (долг про шум): для VRAM — свой, более редкий период; см.
			// softLogEveryVramR493 — почему не 60 с.
			if softLogAllowedR488(backendID, "vram_threshold", time.Now(), softLogEveryVramR493) {
				logger.Get().Warnw("backend VRAM usage above threshold (degraded, not blocked)",
					"backend", backendID, "vram_percent", vramPercent, "threshold", limits.GPU.MaxVRAMUsagePercent)
			}
		}
	}

	// R499 / аудит S-2: порог CPU — из конфига (по умолчанию те же 95).
	cpuMaxUsage := limits.CPU.MaxUsagePercent
	if cpuMaxUsage <= 0 {
		cpuMaxUsage = 95
	}
	if metrics.System.CPUUsagePercent > cpuMaxUsage {
		if hard {
			logger.Get().Warnw("backend CPU usage critical, blocking", "backend", backendID,
				"cpu_usage", metrics.System.CPUUsagePercent, "threshold", cpuMaxUsage)
			return false
		}
		if softLogAllowedR488(backendID, "cpu_busy", time.Now(), softLogEveryR488) {
			logger.Get().Infow(i18n.T("balancer.slot_manager.cpu_busy_soft_info"),
				"backend", backendID, "cpu_usage", metrics.System.CPUUsagePercent, "threshold", cpuMaxUsage)
		}
	}

	// R499 / аудит S-2: порог RAM — из конфига. Раньше значение использовалось
	// только как выключатель (`> 0`), а порог был зашит 98 — то есть настройка
	// «память выше 85 %» ничего не ограничивала.
	ramMaxUsage := limits.Memory.MaxUsagePercent
	if ramMaxUsage <= 0 {
		ramMaxUsage = 98
	}
	if metrics.System.MemoryTotal > 0 {
		memPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		if memPercent > ramMaxUsage {
			logger.Get().Warnw("backend RAM usage critical, blocking", "backend", backendID,
				"ram_percent", memPercent, "threshold", ramMaxUsage)
			return false
		}
	}

	if metrics.System.DiskTotal > 0 && metrics.System.DiskFree < limits.Disk.MinFreeMB {
		logger.Get().Warnw("backend disk space critical, blocking", "backend", backendID,
			"disk_free_mb", metrics.System.DiskFree, "min_required_mb", limits.Disk.MinFreeMB)
		return false
	}

	if metrics.Ollama.MaxModels > 0 {
		if len(metrics.Ollama.RunningModels) >= metrics.Ollama.MaxModels {
			logger.Get().Debugw("backend model slots full (degraded, not blocked)",
				"backend", backendID, "loaded_models", len(metrics.Ollama.RunningModels), "max_models", metrics.Ollama.MaxModels)
		}
	}

	// Проверка слотов запросов вынесена в tryAcquireSlot для атомарности.
	// checkResourceLimits отвечает только за аппаратные/ресурсные лимиты.

	return true
}

// resourceLimitsHardBlockEnabled — R118 (2026-10-14): возвращать ли прежнее
// ЖЁСТКОЕ поведение сторожа ресурсов (блокировать бэкенд по мгновенной
// утилизации: VRAM-процент выше резерва, GPU > 90%, CPU > 95%).
//
// По умолчанию — нет: занятый работой бэкенд остаётся кандидатом, а лишние
// запросы ждут в admission-очереди (параллелизм ограничен слотами). Жёсткая
// блокировка по утилизации делала 503 из нормальной ситуации «GPU/CPU заняты
// собственной генерацией» — на стенде с частичным offload это блокировало ВСЕ
// запросы, включая второй экземпляр Cline и OpenWebUI.
//
//	LB_RESOURCE_LIMITS_HARD_BLOCK=true — вернуть прежнее поведение.
func resourceLimitsHardBlockEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_RESOURCE_LIMITS_HARD_BLOCK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// findModelOnAnyBackendNoVRAMCheck — ищет backend с загруженной моделью,
// игнорируя VRAM headroom check. Для read/mgmt endpoints (Round 22).
//
// Read operations не нагружают GPU/VRAM (нет inference), поэтому блокировка
// при 95% VRAM некорректна — нужно дать им пройти.
func (p *Proxy) findModelOnAnyBackendNoVRAMCheck(model string, bt types.BackendType) string {
	if model == "" {
		return ""
	}
	allowedTypes := p.getAllowedTypesList(bt)

	p.mu.RLock()
	defer p.mu.RUnlock()

	for id, state := range p.backends {
		// R92 (S2): воркер логического бэкенда не обслуживает запросы сам —
		// модель на нём только частью слоёв (см. backend_group_r92.go).
		// R304: сюда же донор — его VRAM отдана мастеру, свои копии ему нельзя.
		if p.cannotHostModelR304(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		p.metricsMgr.mu.RLock()
		// Проверяем оба cache: Ollama-agent и cppworker-poller.
		hasModel := false
		if metrics, ok := p.metricsMgr.metrics[id]; ok {
			if p.backendHasModel(metrics, model) {
				hasModel = true
			}
		}
		if !hasModel {
			if lm, ok := p.metricsMgr.llamaMetrics[id]; ok && lm != nil {
				for _, m := range lm.LoadedModels {
					if m.Name == model {
						hasModel = true
						break
					}
				}
			}
		}
		p.metricsMgr.mu.RUnlock()
		if hasModel {
			return id
		}
	}
	return ""
}

// selectAnyHealthy — выбирает любой healthy бэкенд подходящего типа.
// Для read/mgmt endpoints (Round 22) — fallback когда модель нигде не загружена.
func (p *Proxy) selectAnyHealthy(bt types.BackendType) string {
	allowedTypes := p.getAllowedTypesList(bt)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, state := range p.backends {
		// R92 (S2): воркер логического бэкенда не обслуживает запросы сам —
		// модель на нём только частью слоёв (см. backend_group_r92.go).
		// R304: сюда же донор — его VRAM отдана мастеру, свои копии ему нельзя.
		if p.cannotHostModelR304(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status == types.StatusHealthy {
			return id
		}
	}
	return ""
}
