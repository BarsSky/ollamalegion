package modelreplication

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// GroupController управляет жизненным циклом инстансов моделей.
// Фоновый цикл проверяет количество загруженных инстансов и корректирует их.
type GroupController struct {
	manager *ModelGroupManager

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
	running bool

	// interval определяет периодичность проверки (по умолчанию 10с)
	interval time.Duration
}

// NewGroupController создаёт новый контроллер групп.
func NewGroupController(manager *ModelGroupManager) *GroupController {
	return &GroupController{
		manager:  manager,
		interval: 10 * time.Second,
	}
}

// SetInterval устанавливает интервал между реконсиляциями.
func (gc *GroupController) SetInterval(d time.Duration) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if d > 0 {
		gc.interval = d
	}
}

// Start запускает фоновый цикл контроля групп.
// Возвращает ошибку, если контроллер уже запущен.
func (gc *GroupController) Start() error {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	if gc.running {
		logger.Get().Warnw("group controller already running")
		return nil
	}

	gc.stopCh = make(chan struct{})
	gc.running = true

	gc.wg.Add(1)
	go gc.runLoop()

	logger.Get().Infow("group controller started",
		"interval", gc.interval.String())
	return nil
}

// Stop останавливает фоновый цикл.
// Блокируется до полной остановки goroutine.
func (gc *GroupController) Stop() error {
	gc.mu.Lock()
	if !gc.running {
		gc.mu.Unlock()
		return nil
	}
	close(gc.stopCh)
	gc.mu.Unlock()

	gc.wg.Wait()

	gc.mu.Lock()
	gc.running = false
	gc.mu.Unlock()

	logger.Get().Infow("group controller stopped")
	return nil
}

// IsRunning возвращает true, если фоновый цикл активен.
func (gc *GroupController) IsRunning() bool {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.running
}

// runLoop — основной цикл контроллера с тикером.
func (gc *GroupController) runLoop() {
	defer gc.wg.Done()

	gc.mu.Lock()
	interval := gc.interval
	gc.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Выполняем первую реконсиляцию сразу при старте
	gc.reconcileGroups()

	for {
		select {
		case <-ticker.C:
			gc.reconcileGroups()
		case <-gc.stopCh:
			logger.Get().Infow("group controller loop stopping")
			return
		}
	}
}

// reconcileGroups проверяет и корректирует количество инстансов для всех групп.
func (gc *GroupController) reconcileGroups() {
	// Проверяем, включён ли менеджер
	if !gc.manager.IsEnabled() {
		return
	}

	logger.Get().Debugw("group controller reconcile")

	// Delegat к ModelGroupManager
	gc.manager.EnsureInstances()
}

// TriggerReconcile выполняет внеочередную реконсиляцию (для API).
func (gc *GroupController) TriggerReconcile() {
	if !gc.manager.IsEnabled() {
		logger.Get().Debugw("trigger reconcile skipped: manager disabled")
		return
	}
	logger.Get().Infow("trigger reconcile")
	gc.manager.EnsureInstances()
}
