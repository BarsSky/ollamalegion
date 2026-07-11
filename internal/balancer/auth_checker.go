// Package balancer — общий AuthChecker interface для rpc_coordinator dispatcher
// и virtual_router.
//
// Phase 8 P.1 Session 3.4: добавлен для RpcCoordinatorDispatcher.
// Phase 8 P.2 backlog (2026-07-11): расширен для VirtualRouter (тот же pattern).
//
// Реальная реализация — *api.TokenAuthenticator (см. internal/api/auth.go).
// Interface содержит только то, что dispatcher'ам нужно: IsEnabled() и Authenticate.
//
// Если interface не установлен (nil) или IsEnabled()=false — auth skip.
package balancer

import "net/http"

// AuthChecker — минимальный interface для проверки auth в dispatcher'ах
// (RpcCoordinatorDispatcher + VirtualRouter).
type AuthChecker interface {
	IsEnabled() bool
	Authenticate(r *http.Request) (bool, string)
}
