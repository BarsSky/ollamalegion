package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// agentTimeoutChecker - данные для фоновой проверки таймаута агентов
type agentTimeoutChecker struct {
	stop   chan struct{}
	active bool
	mu     sync.Mutex
}

// StartAgentTimeoutChecker - запуск фоновой проверки таймаута агентов
func (p *Proxy) StartAgentTimeoutChecker(timeout time.Duration) {
	// Останавливаем предыдущий, если был
	p.StopAgentTimeoutChecker()

	checker := &agentTimeoutChecker{
		stop:   make(chan struct{}),
		active: true,
	}
	p.agentChecker = checker

	go func() {
		ticker := time.NewTicker(timeout / 2)
		defer ticker.Stop()

		for {
			select {
			case <-checker.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				for _, state := range p.backends {
					if state.Backend.HasAgent && time.Since(state.Backend.LastAgentContact) > timeout {
						state.Backend.HasAgent = false
						logger.Get().Warnw("agent timeout", "backend", state.Backend.ID, "timeout", timeout)
					}
				}
				p.mu.Unlock()
			}
		}
	}()
}

// StopAgentTimeoutChecker - остановка фоновой проверки таймаута агентов
func (p *Proxy) StopAgentTimeoutChecker() {
	if p.agentChecker != nil {
		p.agentChecker.mu.Lock()
		if p.agentChecker.active {
			close(p.agentChecker.stop)
			p.agentChecker.active = false
		}
		p.agentChecker.mu.Unlock()
		p.agentChecker = nil
	}
}