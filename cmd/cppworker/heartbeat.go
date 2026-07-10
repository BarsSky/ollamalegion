// heartbeat.go — Round 6 #7: configurable heartbeat interval via env.
// Used by SSE/streaming responses to keep TCP connection alive during long
// generations and prevent idle-timeout from clients (Cline/Roo/OpenWebUI).
//
// Env: OLLAMALEGION_HEARTBEAT_MS — milliseconds between keepalive frames.
// Default: 15000 (15s). Set 0 or negative to disable heartbeat (not recommended
// — most clients have 30-60s idle timeout).
//
// Loaded once at startup (sync.Once), parsed safely (negative values fallback).

package main

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// heartbeatInterval — cached resolved heartbeat interval. Set once at first
// call to getHeartbeatInterval, then immutable until process restart.
var (
	heartbeatIntervalOnce sync.Once
	heartbeatIntervalVal  time.Duration
	heartbeatIntervalDone bool // false means "not configured", true means "user-set"
)

const defaultHeartbeat = 15 * time.Second

// getHeartbeatInterval returns the configured heartbeat interval, or `def` if
// env is unset / invalid. Callers pass their preferred default (15s for SSE
// streams, 100ms for buffered tool-call paths).
func getHeartbeatInterval(def time.Duration) time.Duration {
	heartbeatIntervalOnce.Do(func() {
		raw := os.Getenv("OLLAMALEGION_HEARTBEAT_MS")
		if raw == "" {
			return
		}
		ms, err := strconv.Atoi(raw)
		if err != nil {
			return
		}
		if ms <= 0 {
			return // 0 = disabled; caller decides how to handle
		}
		heartbeatIntervalVal = time.Duration(ms) * time.Millisecond
		heartbeatIntervalDone = true
	})
	if heartbeatIntervalDone {
		return heartbeatIntervalVal
	}
	return def
}
