// Package balancer — общий test helper для AuthChecker interface.
//
// Phase 8 P.2 backlog (2026-07-11): fakeAuthChecker — test double для
// AuthChecker, общий для RpcCoordinatorDispatcher + VirtualRouter тестов.
package balancer

import "net/http"

// fakeAuthChecker — test double для AuthChecker interface.
type fakeAuthChecker struct {
	enabled bool
	tokens  map[string]bool
}

func (f *fakeAuthChecker) IsEnabled() bool { return f.enabled }

func (f *fakeAuthChecker) Authenticate(r *http.Request) (bool, string) {
	if !f.enabled {
		return true, ""
	}
	token := r.Header.Get("X-API-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return false, ""
	}
	if f.tokens[token] {
		return true, token
	}
	return false, ""
}
