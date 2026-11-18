package balancer

import (
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// UnloadScheduler — LRU выгрузка неиспользуемых моделей для освобождения VRAM
type UnloadScheduler struct {
	// Порядок полей — под fieldalignment (govet в CI): сначала ПОЛЯ УКАЗАТЕЛЕЙ
	// (proxy/map/канал), затем мьютекс (в нём указателей нет), затем длительности,
	// счётчик и булевы. Область, которую сканирует GC, — ровно 32 байта; при
	// другом порядке структура занимает лишние байты и линтер краснеет.
	proxy    *Proxy
	lastUsed map[string]time.Time // key: "modelName@backendID" → lastRequest
	pending  map[string]time.Time // R503: кулдаун сообщений о простое (вместо метки WarmingUpModels)
	stopCh   chan struct{}
	mu       sync.RWMutex

	checkInterval time.Duration
	idleTimeout   time.Duration
	loopStarts    int32 // R502: сколько раз РЕАЛЬНО поднят цикл (диагностика и тест)
	enabled       bool
	started       bool // R502: Start идемпотентен (аудит X-1)
}

// NewUnloadScheduler — создание планировщика выгрузки.
//
// ВАЖНО (2026-06-22): по умолчанию планировщик ВЫКЛЮЧЕН. Раньше здесь всегда
// стоял enabled=true с idleTimeout=30m, и балансировщик раз в 5 минут помечал
// модели как «unloading» через WarmingUpModels (без реальной выгрузки с бэкенда).
// Это приводило к:
//   - пометке моделей как «выгружаемых» через 30 мин idle, даже если пользователь
//     активно с ними работает (LRU-эвристика работала по lastUse на бэкенде,
//     а не по реальной активности пользователя);
//   - путанице в мониторе и WebUI (модель «выгружена» по мнению балансера, но
//     реально живёт в VRAM на cppworker).
//
// Теперь по умолчанию:
//   - idleTimeout = 0 → фоновый цикл не запускается,
//   - enabled = false,
//   - для включения нужно явно задать balancing.modelInstances.idleUnloadAfter,
//     например "30m" или "1h".
func NewUnloadScheduler(proxy *Proxy) *UnloadScheduler {
	idleTimeout := time.Duration(0)
	if proxy.config.Balancing.ModelInstances.IdleUnloadAfter != "" {
		if d, err := time.ParseDuration(proxy.config.Balancing.ModelInstances.IdleUnloadAfter); err == nil {
			idleTimeout = d
		}
	}

	enabled := idleTimeout > 0
	if !enabled {
		logger.Get().Infow("unloadScheduler: disabled (idleUnloadAfter not set or 0). "+
			"Models will remain in VRAM until explicit /api/models/unload or container restart.",
			"idleTimeout", idleTimeout.String())
	}

	us := &UnloadScheduler{
		proxy:         proxy,
		lastUsed:      make(map[string]time.Time),
		pending:       make(map[string]time.Time),
		checkInterval: 5 * time.Minute,
		idleTimeout:   idleTimeout,
		stopCh:        make(chan struct{}),
		enabled:       enabled,
	}

	return us
}

// RecordModelUse — запись использования модели на бэкенде
func (us *UnloadScheduler) RecordModelUse(modelName, backendID string) {
	if !us.enabled || modelName == "" || backendID == "" {
		return
	}
	key := modelName + "@" + backendID
	us.mu.Lock()
	us.lastUsed[key] = time.Now()
	us.mu.Unlock()
}

// Start — запуск фонового цикла проверки.
//
// R502 / аудит X-1: запуск ИДЕМПОТЕНТЕН. Раньше планировщик создавался и
// стартовал в конструкторе прокси, а `cmd/balancer/main.go` создавал ВТОРОЙ объект,
// подменял указатель и запускал его ещё раз: первый цикл продолжал крутиться, но
// его `lastUsed` больше никто не обновлял (RecordModelUse идёт через указатель
// прокси) — «мёртвая» выгрузка и двойные записи в логе. Теперь объект один, а
// повторный Start ничего не делает.
func (us *UnloadScheduler) Start() {
	if !us.enabled {
		return
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if us.started {
		return
	}
	us.started = true
	go us.loop()
}

// Stop — остановка планировщика
func (us *UnloadScheduler) Stop() {
	us.mu.Lock()
	defer us.mu.Unlock()

	if us.stopCh != nil {
		select {
		case <-us.stopCh:
		default:
			close(us.stopCh)
		}
		us.stopCh = nil
	}
	us.started = false
}

// SetEnabled — включение/отключение планировщика
func (us *UnloadScheduler) SetEnabled(enabled bool) {
	us.mu.Lock()
	defer us.mu.Unlock()

	if enabled && !us.enabled {
		us.enabled = true
		us.stopCh = make(chan struct{})
		us.started = true
		go us.loop()
	} else if !enabled && us.enabled {
		us.enabled = false
		us.started = false
		if us.stopCh != nil {
			select {
			case <-us.stopCh:
			default:
				close(us.stopCh)
			}
			us.stopCh = nil
		}
	}
}

// loop — основной цикл проверки idle моделей.
// stopCh копируется под RLock чтобы избежать data race с Stop/SetEnabled.
func (us *UnloadScheduler) loop() {
	atomic.AddInt32(&us.loopStarts, 1)
	ticker := time.NewTicker(us.checkInterval)
	defer ticker.Stop()

	us.mu.RLock()
	stopCh := us.stopCh
	us.mu.RUnlock()

	for {
		select {
		case <-ticker.C:
			us.checkAndUnload()
		case <-stopCh:
			return
		}
	}
}

// checkAndUnload — проверка моделей на простой и выгрузка
func (us *UnloadScheduler) checkAndUnload() {
	candidates := us.getUnloadCandidates()
	if len(candidates) == 0 {
		return
	}

	for _, c := range candidates {
		us.unloadModel(c.Model, c.BackendID)
	}
}

// UnloadCandidate — кандидат на выгрузку
type UnloadCandidate struct {
	Model     string
	BackendID string
	IdleTime  time.Duration
}

// getUnloadCandidates — возвращает список моделей, которые не использовались > idleTimeout
func (us *UnloadScheduler) getUnloadCandidates() []UnloadCandidate {
	us.mu.Lock()
	defer us.mu.Unlock()

	now := time.Now()

	// Собираем все запущенные модели по бэкендам
	us.proxy.metricsMgr.mu.RLock()
	defer us.proxy.metricsMgr.mu.RUnlock()

	// Защита: не выгружаем модели, которые РАБОТАЮТ. Источник — боевые сигналы
	// (активные слоты, очередь admission, живой поток сессии), а не legacy-списки
	// очереди, которые пусты после R78 (аудит X-2).
	busyBackends, activeKeys := us.activeModelsMap()

	var candidates []UnloadCandidate
	for id, metrics := range us.proxy.metricsMgr.metrics {
		// Пропускаем недоступные бэкенды
		us.proxy.mu.RLock()
		state, ok := us.proxy.backends[id]
		us.proxy.mu.RUnlock()
		if !ok || state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Бэкенд обслуживает запросы или держит очередь — выгружать нечего и нельзя.
		if busyBackends[id] {
			continue
		}

		// Пропускаем модели в процессе загрузки/прогрева
		state.mu.Lock()
		warmingUp := state.WarmingUpModels
		state.mu.Unlock()

		for _, model := range metrics.Ollama.RunningModels {
			key := model.Name + "@" + id

			// Пропускаем warming модели
			if _, isWarming := warmingUp[model.Name]; isWarming {
				continue
			}

			// Пропускаем модель с живым потоком сессии
			if activeKeys[key] {
				continue
			}

			lastUse, recorded := us.lastUsed[key]
			if !recorded {
				// Модель загружена но никогда не использовалась — используем время загрузки
				us.lastUsed[key] = now
				continue
			}

			idleTime := now.Sub(lastUse)
			if idleTime >= us.idleTimeout {
				candidates = append(candidates, UnloadCandidate{
					Model:     model.Name,
					BackendID: id,
					IdleTime:  idleTime,
				})
			}
		}
	}

	return candidates
}

// activeModelsMap — что считается «под нагрузкой» для целей выгрузки.
//
// R503 / аудит S4 X-2. БЫЛО: читались `queueMgr.processing`/`pending` —
// legacy-списки, которые после R78 всегда пусты (запросы ждут в admission-очереди).
// То есть защита «не выгружать модель под нагрузкой» не работала вовсе, и модель,
// обслуживающая живой запрос, могла получить метку «выгружается».
//
// ЧТО СЧИТАЕТСЯ НАГРУЗКОЙ СЕЙЧАС — три независимых боевых сигнала:
//  1. на бэкенде заняты слоты (ActiveReqs > 0): бэкенд обслуживает запрос, модель
//     в работе — выгружать нельзя независимо от того, совпало ли имя модели;
//  2. на бэкенде стоит очередь в admission (`waitingByBackend`): слот занят, и
//     запрос начнётся сразу, как освободится;
//  3. у сессии идёт поток (`HasActiveStream`): долгая генерация обновляет
//     LastRequestAt только по завершении, поэтому по времени простоя её не видно.
func (us *UnloadScheduler) activeModelsMap() (busyBackends, activeKeys map[string]bool) {
	busyBackends = make(map[string]bool)
	activeKeys = make(map[string]bool)

	for backendID, n := range us.proxy.admission.waitingByBackend() {
		if n > 0 {
			busyBackends[backendID] = true
		}
	}

	us.proxy.mu.RLock()
	states := make([]*BackendState, 0, len(us.proxy.backends))
	for _, st := range us.proxy.backends {
		states = append(states, st)
	}
	us.proxy.mu.RUnlock()
	for _, st := range states {
		if st == nil || st.Backend == nil {
			continue
		}
		st.mu.Lock()
		active := st.ActiveReqs
		id := st.Backend.ID
		st.mu.Unlock()
		if active > 0 {
			busyBackends[id] = true
		}
	}

	if us.proxy.sessionMgr != nil {
		for _, s := range us.proxy.sessionMgr.GetAllSnapshots() {
			if !s.HasActiveStream || s.Model == "" || s.BackendID == "" {
				continue
			}
			activeKeys[s.Model+"@"+s.BackendID] = true
		}
	}

	return busyBackends, activeKeys
}

// unloadModel — обработка простоя модели.
//
// R503 / аудит S4 X-2: метка `WarmingUpModels[lru_unload]` больше НЕ ставится.
//
// БЫЛО: «выгрузка» = метка в WarmingUpModels на 2 минуты. Метка означает
// «модель грузится»: она попадает в P2 селектора, в монитор (cluster_state
// публикует WarmingUpModels), а model_instance_controller исключает по ней бэкенд
// из размещения. То есть простаивающая модель объявлялась загружающейся, хотя с
// ней ничего не делали, и маршрут мог увести запрос «ждать загрузку» вместо
// нормального выбора.
//
// СЕЙЧАС: честная запись простоя. Реальной выгрузки здесь нет и быть не может:
// у Ollama нет endpoint'а выгрузки, а этот планировщик смотрит именно
// `metrics.Ollama.RunningModels`. Модель остаётся в VRAM (её выгрузит Ollama по
// keep_alive или /api/models/unload у cppworker), а мы снимаем учёт простоя и
// пишем это в лог явно, чтобы не выдавать метку за действие.
func (us *UnloadScheduler) unloadModel(modelName, backendID string) {
	us.proxy.mu.RLock()
	state, ok := us.proxy.backends[backendID]
	us.proxy.mu.RUnlock()
	if !ok || state == nil || state.Backend == nil {
		return
	}

	// Проверяем статус перед выгрузкой
	if state.Backend.Status != types.StatusHealthy {
		return
	}

	key := modelName + "@" + backendID

	// Кулдаун: не чаще одного сообщения на модель за 2 минуты (раньше ту же роль
	// играла метка WarmingUpModels — теперь она не нужна).
	us.mu.Lock()
	if until, seen := us.pending[key]; seen && time.Now().Before(until) {
		us.mu.Unlock()
		return
	}
	us.pending[key] = time.Now().Add(2 * time.Minute)
	delete(us.lastUsed, key)
	us.mu.Unlock()

	logger.Get().Infow("unloadScheduler: модель простаивает дольше idleUnloadAfter",
		"backend", backendID, "model", modelName,
		"real_unload", "не выполняется: у Ollama нет endpoint'а выгрузки; модель "+
			"остаётся в VRAM до keep_alive/выгрузки cppworker",
		"hint", "выгрузка на cppworker: POST /api/models/unload?name=…; "+
			"маршрутизация не меняется (аудит X-2)")
}
