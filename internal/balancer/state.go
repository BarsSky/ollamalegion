package balancer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// SaveState - сохранение текущего состояния backends в state.json
func (p *Proxy) SaveState() error {
	p.mu.RLock()
	backends := make([]types.Backend, 0, len(p.backends))
	for _, state := range p.backends {
		backends = append(backends, *state.Backend)
	}
	p.mu.RUnlock()

	state := types.StateFile{
		Version:  types.StateVersion,
		Updated:  time.Now().UTC(),
		Backends: backends,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	dir := filepath.Dir(p.statePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create state directory: %w", err)
		}
	}

	if err := os.WriteFile(p.statePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	logger.Get().Infow("state saved", "path", p.statePath, "backends", len(backends))
	return nil
}

// LoadState - загрузка состояния из state.json (merge поверх текущих backends)
func (p *Proxy) LoadState() error {
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("state file does not exist")
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var state types.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("failed to parse state file: %w", err)
	}

	if state.Version != types.StateVersion {
		logger.Get().Warnw("state version mismatch", "got", state.Version, "expected", types.StateVersion)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	stateBackendMap := make(map[string]types.Backend)
	for _, b := range state.Backends {
		stateBackendMap[b.ID] = b
	}

	for i := range p.config.Backends {
		if saved, ok := stateBackendMap[p.config.Backends[i].ID]; ok {
			p.config.Backends[i].Status = saved.Status
			p.config.Backends[i].LastHealthCheck = saved.LastHealthCheck
			p.config.Backends[i].ConsecutiveFailures = saved.ConsecutiveFailures
			p.config.Backends[i].ActiveRequests = saved.ActiveRequests
			p.config.Backends[i].HasAgent = saved.HasAgent
			p.config.Backends[i].LastAgentContact = saved.LastAgentContact
			p.config.Backends[i].RuntimeRequestTimeout = saved.RuntimeRequestTimeout
			// Round 13 (2026-07-10): restore AgentID — нужен для dedup migration
			// (canonical = hasAgent && agentId!=""). Без этого LoadState
			// восстанавливал hasAgent=true, но терял agentId → миграция
			// не находила canonical и оставляла оба бэкенда.
			p.config.Backends[i].AgentID = saved.AgentID

			if bs, exists := p.backends[p.config.Backends[i].ID]; exists {
				bs.Backend = &p.config.Backends[i]
			}
		}
	}

		for _, saved := range state.Backends {
			if _, exists := p.backends[saved.ID]; !exists {
				backend := saved
				p.backends[backend.ID] = &BackendState{
					Backend:         &backend,
					ActiveReqs:      0,
					LastUsed:        time.Time{},
					WarmingUpModels: make(map[string]*types.WarmupState),
				}
				p.config.Backends = append(p.config.Backends, backend)
			}
		}

	// Round 13 (2026-07-10): migration — drop orphan legacy "agent-only" backends
	// (created by old /agents/register when cppworker had its own backend).
	// Canonical = backend with hasAgent=true AND agentId!="" (set via AttachAgentToBackend).
	// Legacy = backend with hasAgent=true BUT agentId=="" (created by old agent handler).
	// If both exist for same (host, cppWorkerPort) → drop legacy, keep canonical.
	type bmKey struct {
		host string
		port int
	}
	canonicalByKey := make(map[bmKey]string)
	legacyIDs := make([]string, 0)
	for id, state := range p.backends {
		b := state.Backend
		if b.Type != types.BackendTypeLlamaCpp || b.CppWorkerPort == 0 {
			continue
		}
		k := bmKey{host: b.Host, port: b.CppWorkerPort}
		if b.AgentID != "" {
			// Real attach — this is canonical
			canonicalByKey[k] = id
		}
	}
	for id, state := range p.backends {
		b := state.Backend
		if b.Type != types.BackendTypeLlamaCpp || b.CppWorkerPort == 0 {
			continue
		}
		k := bmKey{host: b.Host, port: b.CppWorkerPort}
		if b.AgentID == "" && canonicalByKey[k] != "" && canonicalByKey[k] != id {
			legacyIDs = append(legacyIDs, id)
		}
	}
	if len(legacyIDs) > 0 {
		logger.Get().Infow("LoadState: dropping legacy orphan backends (Round 13 migration)",
			"count", len(legacyIDs), "ids", legacyIDs)
		for _, id := range legacyIDs {
			delete(p.backends, id)
			// Также удаляем из p.config.Backends
			for i, b := range p.config.Backends {
				if b.ID == id {
					p.config.Backends = append(p.config.Backends[:i], p.config.Backends[i+1:]...)
					break
				}
			}
		}
		// Сохраняем state.json сразу чтобы миграция применилась при следующем рестарте
		go func() {
			time.Sleep(1 * time.Second)
			if err := p.SaveState(); err != nil {
				logger.Get().Warnw("LoadState: failed to save migrated state", "error", err)
			}
		}()
	}

	logger.Get().Infow("state loaded", "path", p.statePath, "backends", len(state.Backends))
	return nil
}

// scheduleSave - debounced autosave через 5 секунд
func (p *Proxy) scheduleSave() {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()

	if p.saveTimer != nil {
		p.saveTimer.Stop()
	}

	p.saveTimer = time.AfterFunc(5*time.Second, func() {
		if err := p.SaveState(); err != nil {
			logger.Get().Errorw("autosave error", "error", err)
		}
	})
}

// FlushState - немедленное сохранение состояния (для graceful shutdown)
func (p *Proxy) FlushState() error {
	p.saveMu.Lock()
	if p.saveTimer != nil {
		p.saveTimer.Stop()
		p.saveTimer = nil
	}
	p.saveMu.Unlock()

	return p.SaveState()
}