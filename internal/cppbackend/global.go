// internal/cppbackend/global.go — package-level singleton accessor.
//
// Round 37 (2026-08-18): CLI tools (`-feasible <model>`, `-auto-load <model>`)
// нужен доступ к Backend из main.go после flag.Parse() и до запуска HTTP-сервера.
// Чтобы не плодить глобалы в main, добавляем SetGlobalBackend + GetBackend.
//
// Использование:
//
//	backend := cppbackend.NewBackend(cfg, mm)
//	cppbackend.SetGlobalBackend(backend)   // в main после NewBackend
//	...
//	b := cppbackend.GetBackend()           // в CLI handlers
//
// Потокобезопасно: использует atomic.Value. Set/Read не блокируются.

package cppbackend

import "sync/atomic"

// globalBackend — package-level singleton для доступа к Backend из CLI tools
// (cmd/cppworker/cli_feasible.go, cli_auto_load.go).
//
// nil до вызова SetGlobalBackend (NewBackend не вызывает его автоматически —
// caller решает, нужен ли global access; обычно это main.go).
var globalBackend atomic.Value // *Backend

// SetGlobalBackend регистрирует Backend как package-level singleton.
// Вызывается один раз из main.go после создания Backend.
// Повторные вызовы перезаписывают (без warning — caller контролирует lifecycle).
func SetGlobalBackend(b *Backend) {
	globalBackend.Store(b)
}

// GetBackend возвращает зарегистрированный Backend или nil, если не задан.
// CLI tools ОБЯЗАНЫ проверять на nil (cppworker мог быть запущен без backend,
// например, в -healthcheck режиме).
func GetBackend() *Backend {
	v := globalBackend.Load()
	if v == nil {
		return nil
	}
	if b, ok := v.(*Backend); ok {
		return b
	}
	return nil
}
