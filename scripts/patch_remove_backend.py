#!/usr/bin/env python3
"""Patch proxy.go: add graceful drain and immediate FlushState to RemoveBackend."""
import os

PROXY_FILE = os.path.join(os.path.dirname(__file__), '..', 'internal', 'balancer', 'proxy.go')

with open(PROXY_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

old_block = '''// RemoveBackend - удаление бэкенда
func (p *Proxy) RemoveBackend(backendID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.backends[backendID]; !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	delete(p.backends, backendID)

	p.metricsMgr.mu.Lock()
	delete(p.metricsMgr.metrics, backendID)
	p.metricsMgr.mu.Unlock()

	p.PublishEvent(types.Event{
		Type:      types.EventBackendRemove,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data:      map[string]interface{}{},
	})

	p.scheduleSave()

	return nil
}'''

new_block = '''// RemoveBackend - удаление бэкенда с graceful drain активных запросов.
// Помечает бэкенд как Draining, ждёт завершения активных запросов (с таймаутом 30с),
// затем удаляет из backends, metrics и принудительно сохраняет state.json.
func (p *Proxy) RemoveBackend(backendID string) error {
	p.mu.Lock()
	state, exists := p.backends[backendID]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	// Помечаем как Draining — новые запросы не будут направляться
	oldStatus := state.Backend.Status
	state.Backend.Status = types.StatusDraining
	p.mu.Unlock()

	if oldStatus != types.StatusDraining {
		p.PublishEvent(types.Event{
			Type:      types.EventStatusChange,
			Timestamp: time.Now().UTC(),
			BackendID: backendID,
			Data: map[string]interface{}{
				"oldStatus": string(oldStatus),
				"newStatus": string(types.StatusDraining),
			},
		})
	}

	// Ждём завершения активных запросов с таймаутом 30 секунд
	drainTimeout := time.After(30 * time.Second)
	drainTicker := time.NewTicker(200 * time.Millisecond)
	defer drainTicker.Stop()

drainLoop:
	for {
		select {
		case <-drainTimeout:
			logger.Get().Warnw("drain timeout reached, force-removing backend",
				"backend", backendID)
			break drainLoop
		case <-drainTicker.C:
			state.mu.Lock()
			active := state.ActiveReqs
			state.mu.Unlock()
			if active == 0 {
				logger.Get().Infow("all active requests drained, removing backend",
					"backend", backendID)
				break drainLoop
			}
		}
	}

	// Удаление бэкенда
	p.mu.Lock()
	delete(p.backends, backendID)
	p.mu.Unlock()

	p.metricsMgr.mu.Lock()
	delete(p.metricsMgr.metrics, backendID)
	p.metricsMgr.mu.Unlock()

	p.PublishEvent(types.Event{
		Type:      types.EventBackendRemove,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data:      map[string]interface{}{},
	})

	// Немедленное сохранение state.json, чтобы удалённый бэкенд не восстановился при перезагрузке
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state after backend removal", "backend", backendID, "error", err)
	}

	logger.Get().Infow("backend removed and state flushed", "backend", backendID)
	return nil
}'''

if old_block in content:
    content = content.replace(old_block, new_block)
    with open(PROXY_FILE, 'w', encoding='utf-8') as f:
        f.write(content)
    print('PATCHED: RemoveBackend updated with drain logic and FlushState')
else:
    print('NOT FOUND: RemoveBackend old block not matched')
    # Find RemoveBackend in the file
    for i, line in enumerate(content.split('\n')):
        if 'RemoveBackend' in line and 'func' in line:
            start = max(0, i)
            end = min(len(content.split('\n')), i+25)
            print(f'\nRemoveBackend context (lines {start+1}-{end}):')
            for j in range(start, end):
                print(f'{j+1}: {repr(content.split(chr(10))[j])}')
            break