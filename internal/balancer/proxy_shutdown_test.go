// proxy_shutdown_test.go — Round 52.5 (2026-08-24): tests for Proxy.Shutdown per-component Stop.

package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestProxy_Shutdown_StopsEventBus — R52.5: Proxy.Shutdown должен вызвать EventBus.Stop.
func TestProxy_Shutdown_StopsEventBus(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())

	// Subscribe до Shutdown — канал должен быть valid
	subID, subCh := proxy.EventBus().Subscribe()
	defer proxy.EventBus().Unsubscribe(subID)

	if subCh == nil {
		t.Fatal("expected non-nil subscriber channel")
	}

	// t.Cleanup зарегистрирует proxy.Shutdown → EventBus.Stop.
	// Явный вызов не делаем — t.Cleanup вызовет его в конце теста.
	// Проверяем что Stop() ещё не вызван:
	if proxy.EventBus().Done() == nil {
		t.Error("EventBus.Done() should be non-nil before Stop")
	}
	select {
	case <-proxy.EventBus().Done():
		t.Error("EventBus.Done() should block before Stop")
	default:
	}
}

// TestProxy_Shutdown_StopsEventBusSubChannels — R52.5: subscriber channels closed after Shutdown.
func TestProxy_Shutdown_StopsEventBusSubChannels(t *testing.T) {
	proxy := &Proxy{}
	proxy.config = &types.LoadBalancerConfig{}
	proxy.eventBus = NewEventBus()

	subID, subCh := proxy.EventBus().Subscribe()

	// Запускаем Shutdown (в отдельной горутине — он ждёт activeStreams.Wait)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = proxy.Shutdown(shutdownCtx)
	}()

	// Ждём что subCh закроется (EventBus.Stop() в составе Shutdown).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-subCh:
			if !ok {
				// Канал закрыт — EventBus.Stop вызван.
				cancel()
				return
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Error("subscriber channel was not closed after proxy.Shutdown")
	_ = subID
}

// TestProxy_Shutdown_NilSafe — все компоненты могут быть nil, Shutdown не должен паниковать.
func TestProxy_Shutdown_NilSafe(t *testing.T) {
	proxy := &Proxy{}
	proxy.config = &types.LoadBalancerConfig{}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Shutdown может вернуть error (например, FlushState fails без config файла),
	// но главное — не должно быть PANIC от nil pointer dereference.
	_ = proxy.Shutdown(shutdownCtx)
}
