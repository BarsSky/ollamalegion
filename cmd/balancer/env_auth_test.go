// Round 52 (2026-08-24): tests for LB_API_TOKEN env override.
//
// Background:
//   config.bundled.json has "auth.tokens": ["bundled-default"] hardcoded.
//   cppworker/agent/webui читают токены из .env.bundled-with-agent через
//   CPPWORKER_API_TOKEN/API_TOKEN/BALANCER_TOKEN. При свежем деплое с
//   одним .env файлом: WebUI отправляет правильный токен, балансер ожидает
//   "bundled-default" → 401 на /api/v1/backends.
//
//   Fix: LB_API_TOKEN (single) или LB_AUTH_TOKENS (CSV) полностью заменяют
//   conf.Auth.Tokens. Если env пустой — оставляем config.json.

package main

import (
	"os"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestApplyEnvAuthTokens_NoEnv_ConfigKept(t *testing.T) {
	// Clear both env vars
	os.Unsetenv("LB_API_TOKEN")
	os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: true,
		Tokens:  []string{"bundled-default"},
	}
	applyEnvAuthTokens(auth)

	if !auth.Enabled {
		t.Error("auth.Enabled should be preserved when env is not set")
	}
	if len(auth.Tokens) != 1 || auth.Tokens[0] != "bundled-default" {
		t.Errorf("auth.Tokens should be unchanged when env is not set, got %v", auth.Tokens)
	}
}

func TestApplyEnvAuthTokens_LB_API_TOKEN_OverridesConfig(t *testing.T) {
	os.Setenv("LB_API_TOKEN", "my-secret-from-env")
	defer os.Unsetenv("LB_API_TOKEN")
	os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: true,
		Tokens:  []string{"bundled-default"},
	}
	applyEnvAuthTokens(auth)

	if len(auth.Tokens) != 1 || auth.Tokens[0] != "my-secret-from-env" {
		t.Errorf("LB_API_TOKEN should replace config.json tokens, got %v", auth.Tokens)
	}
	if !auth.Enabled {
		t.Error("auth.Enabled should be true after env override")
	}
}

func TestApplyEnvAuthTokens_LB_AUTH_TOKENS_CSV(t *testing.T) {
	os.Unsetenv("LB_API_TOKEN")
	os.Setenv("LB_AUTH_TOKENS", "token1,token2,token3")
	defer os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: true,
		Tokens:  []string{"bundled-default"},
	}
	applyEnvAuthTokens(auth)

	if len(auth.Tokens) != 3 {
		t.Errorf("LB_AUTH_TOKENS CSV should produce 3 tokens, got %d: %v", len(auth.Tokens), auth.Tokens)
	}
	if auth.Tokens[0] != "token1" || auth.Tokens[1] != "token2" || auth.Tokens[2] != "token3" {
		t.Errorf("LB_AUTH_TOKENS order/whitespace wrong, got %v", auth.Tokens)
	}
}

func TestApplyEnvAuthTokens_BothSet_LB_API_TOKEN_Wins(t *testing.T) {
	// Both env vars set: LB_API_TOKEN wins (single token), LB_AUTH_TOKENS is ignored
	os.Setenv("LB_API_TOKEN", "winner")
	os.Setenv("LB_AUTH_TOKENS", "loser1,loser2")
	defer os.Unsetenv("LB_API_TOKEN")
	defer os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: false, // even disabled
		Tokens:  []string{"bundled-default"},
	}
	applyEnvAuthTokens(auth)

	if len(auth.Tokens) != 1 || auth.Tokens[0] != "winner" {
		t.Errorf("LB_API_TOKEN should win over LB_AUTH_TOKENS, got %v", auth.Tokens)
	}
	if !auth.Enabled {
		t.Error("auth.Enabled should be true after env override (force enable)")
	}
}

func TestApplyEnvAuthTokens_EnvEnablesDisabledAuth(t *testing.T) {
	// auth.Enabled=false in config, but env sets token — env should enable
	os.Setenv("LB_API_TOKEN", "forced-token")
	defer os.Unsetenv("LB_API_TOKEN")
	os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: false,
		Tokens:  nil,
	}
	applyEnvAuthTokens(auth)

	if !auth.Enabled {
		t.Error("env should force-enable auth when set")
	}
	if len(auth.Tokens) != 1 || auth.Tokens[0] != "forced-token" {
		t.Errorf("env token should be applied, got %v", auth.Tokens)
	}
}

func TestApplyEnvAuthTokens_LegacyConfig_LogsWarning(t *testing.T) {
	// When config.json has "bundled-default" AND env is not set,
	// should log a warning. We can't easily assert the log here,
	// but we can verify the function does NOT panic and tokens
	// are preserved.
	os.Unsetenv("LB_API_TOKEN")
	os.Unsetenv("LB_AUTH_TOKENS")

	auth := &types.AuthConfig{
		Enabled: true,
		Tokens:  []string{"bundled-default"},
	}
	applyEnvAuthTokens(auth) // should not panic; should log warn

	if len(auth.Tokens) != 1 || auth.Tokens[0] != "bundled-default" {
		t.Errorf("legacy config should be kept (no env), got %v", auth.Tokens)
	}
}
