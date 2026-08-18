package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/config"
)

// TestConfigLoadOrFail_ValidFile — Round 40: load valid config file
// succeeds and returns the in-memory config (no env fallback engaged).
func TestConfigLoadOrFail_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	validJSON := `{
		"initialized": true,
		"loadBalancer": {"host": "0.0.0.0", "port": 18080, "apiPort": 18081},
		"backends": [
			{"id": "test-backend", "name": "Test", "host": "127.0.0.1",
			 "ollamaPort": 11434, "agentPort": 18032, "status": "healthy", "type": "ollama"}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(validJSON), 0644); err != nil {
		t.Fatalf("setup: write config: %v", err)
	}

	// Clear env vars that could affect the test
	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")
	t.Setenv("AUTH_TOKENS", "")
	t.Setenv("BACKEND_0_ID", "")

	cfg, err := config.LoadOrFail(cfgPath)
	if err != nil {
		t.Fatalf("expected success on valid config, got error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	data := cfg.Get()
	if len(data.Backends) != 1 {
		t.Errorf("expected 1 backend from file, got %d", len(data.Backends))
	}
}

// TestConfigLoadOrFail_NotExistsWithFallback — file doesn't exist +
// LB_ALLOW_ENV_FALLBACK=true (default): LoadFromEnv is used with WARN log.
func TestConfigLoadOrFail_NotExistsWithFallback(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "does_not_exist.json")

	// Ensure env fallback is allowed (default).
	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")
	t.Setenv("LB_FAIL_ON_EMPTY_CONFIG", "false")
	t.Setenv("AUTH_TOKENS", "test-token-r40-1,test-token-r40-2")
	// No backends in env, but auth tokens set so we can detect env path was used.

	cfg, err := config.LoadOrFail(nonExistent)
	if err != nil {
		t.Fatalf("expected success on missing file with fallback, got: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config from env")
	}
	// Env fallback path: backends=0, profiles=0, but tokens populated.
	data := cfg.Get()
	if len(data.Auth.Tokens) != 2 {
		t.Errorf("expected 2 tokens from env, got %d", len(data.Auth.Tokens))
	}
}

// TestConfigLoadOrFail_NotExistsNoFallback — file doesn't exist +
// LB_ALLOW_ENV_FALLBACK=false: FAIL LOUD with CONFIG_REQUIRED.
func TestConfigLoadOrFail_NotExistsNoFallback(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "does_not_exist.json")

	t.Setenv("LB_ALLOW_ENV_FALLBACK", "false")

	cfg, err := config.LoadOrFail(nonExistent)
	if err == nil {
		t.Fatalf("expected error when file missing and fallback disabled, got cfg=%v", cfg)
	}
	if !strings.Contains(err.Error(), "CONFIG_REQUIRED") {
		t.Errorf("expected error to contain CONFIG_REQUIRED, got: %v", err)
	}
}

// TestConfigLoadOrFail_InvalidJSON — file EXISTS but parse error:
// MUST fail loud with CONFIG_INVALID. NO silent env fallback.
//
// This is the regression test for the Cline UND_ERR_SOCKET bug:
// auth.tokens was a list of objects, json.Unmarshal failed, balancer
// silently fell back to LoadFromEnv (0 profiles, 0 backends).
func TestConfigLoadOrFail_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	// Simulate the exact bug: auth.tokens is a list of objects (wrong schema)
	// instead of []string. This is what caused the silent fallback.
	invalidJSON := `{
		"initialized": true,
		"loadBalancer": {"host": "0.0.0.0", "port": 18080, "apiPort": 18081},
		"auth": {
			"enabled": true,
			"headerName": "X-API-Token",
			"tokens": [{"name": "tok1", "_note": "wrong schema!"}]
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(invalidJSON), 0644); err != nil {
		t.Fatalf("setup: write invalid config: %v", err)
	}

	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")
	// Even with fallback allowed, invalid JSON must fail loud.
	// (Fallback is only for "file not found", not parse errors.)

	cfg, err := config.LoadOrFail(cfgPath)
	if err == nil {
		t.Fatalf("expected FAIL LOUD on invalid JSON, got cfg=%v", cfg)
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "CONFIG_INVALID") {
		t.Errorf("expected error to contain CONFIG_INVALID, got: %v", err)
	}
	if !strings.Contains(errStr, "Refusing to fall back") {
		t.Errorf("expected error to mention 'Refusing to fall back', got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil cfg on error, got: %v", cfg)
	}
}

// TestConfigLoadOrFail_MalformedFile — file exists but is not valid JSON at all
// (e.g., truncated or hand-edited garbage). Same loud-fail behavior.
func TestConfigLoadOrFail_MalformedFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{this is not valid json at all`), 0644); err != nil {
		t.Fatalf("setup: write malformed config: %v", err)
	}

	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")

	cfg, err := config.LoadOrFail(cfgPath)
	if err == nil {
		t.Fatalf("expected FAIL LOUD on malformed JSON, got cfg=%v", cfg)
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Errorf("expected CONFIG_INVALID in error, got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil cfg, got: %v", cfg)
	}
}

// TestConfigLoadOrFail_EmptyConfigFailsWhenStrict — file doesn't exist, env
// fallback active, but LB_FAIL_ON_EMPTY_CONFIG=true → FAIL LOUD.
func TestConfigLoadOrFail_EmptyConfigFailsWhenStrict(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "no_config.json")

	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")
	t.Setenv("LB_FAIL_ON_EMPTY_CONFIG", "true")
	// Ensure no backends / profiles in env
	t.Setenv("BACKEND_0_ID", "")
	t.Setenv("AUTH_TOKENS", "")

	cfg, err := config.LoadOrFail(nonExistent)
	if err == nil {
		t.Fatalf("expected FAIL LOUD on empty env fallback, got cfg=%v", cfg)
	}
	if !strings.Contains(err.Error(), "CONFIG_EMPTY") {
		t.Errorf("expected CONFIG_EMPTY in error, got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil cfg, got: %v", cfg)
	}
}

// TestConfigLoadOrFail_PreservesErrorChain — ensures that the original
// json.Unmarshal error is wrapped in the CONFIG_INVALID error so the
// operator can see the underlying parse error (e.g., "cannot unmarshal
// object into Go struct field AuthConfig.auth.tokens of type string").
func TestConfigLoadOrFail_PreservesErrorChain(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	invalidJSON := `{"auth": {"tokens": [{"name": "x"}]}}`
	if err := os.WriteFile(cfgPath, []byte(invalidJSON), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Setenv("LB_ALLOW_ENV_FALLBACK", "true")

	_, err := config.LoadOrFail(cfgPath)
	if err == nil {
		t.Fatal("expected error")
	}
	// The underlying json error should mention "cannot unmarshal"
	if !strings.Contains(err.Error(), "cannot unmarshal") &&
		!strings.Contains(err.Error(), "json:") {
		t.Errorf("expected underlying json error to be preserved, got: %v", err)
	}
}

// TestConfigLoadOrFail_VerifyJSON — sanity check that the test JSON is
// actually invalid against the Go struct (catches drift in test fixtures).
func TestConfigLoadOrFail_VerifyJSON(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	invalidJSON := `{"auth": {"tokens": [{"name": "x"}]}}`
	if err := os.WriteFile(cfgPath, []byte(invalidJSON), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Direct Load (no loud-fail wrapper) — should return an error.
	_, err := config.Load(cfgPath)
	if err == nil {
		t.Fatal("test invariant broken: invalid JSON should fail to load")
	}
	// Try to decode separately to confirm the error is from json.Unmarshal
	var x map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(invalidJSON), &x); jsonErr != nil {
		// The fixture itself isn't even valid JSON → fix the fixture.
		t.Fatalf("test fixture is not valid JSON: %v", jsonErr)
	}
}
