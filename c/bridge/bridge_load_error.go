// bridge_load_error.go — R60.59 (2026-09-14): wrapLoadError helper.
//
// Lives in a separate file (not bridge.go) so it compiles under BOTH
// -tags llama_stub AND !llama_stub. bridge.go requires CGO + llama.h
// and is only compiled in non-stub mode. bridge_stub.go replaces all
// bridge.go symbols with no-op stubs in stub mode.
//
// wrapLoadError maps C-bridge error_msg → Go error, wrapping
// "aborted by user" sentinel so callers (backend.LoadModelWithOpts,
// balancer proxy) can detect user-initiated cancel via errors.Is.
package bridge

import (
	"fmt"
	"strings"
)

// wrapLoadError маппит C-bridge error_msg в Go error. Если error_msg содержит
// "aborted by user" (R60.57 checkpoint string) — оборачивает в ErrAborted
// sentinel чтобы backend.LoadModelWithOpts и balancer proxy могли отличить
// user-initiated cancel от generic load failure через errors.Is.
//
// Без этого балансер видел abort как generic 500 → OpenWebUI retry cascades
// до timeout → клиент зависал (R60.58 symptom).
func wrapLoadError(errStr string) error {
	if errStr == "" {
		return fmt.Errorf("load model failed (no error message from C-bridge)")
	}
	if strings.Contains(errStr, "aborted by user") {
		return fmt.Errorf("load cancelled: %s: %w", errStr, ErrAborted)
	}
	return fmt.Errorf("load model failed: %s", errStr)
}
