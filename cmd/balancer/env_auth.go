// Round 52 (2026-08-24): LB_API_TOKEN env override for balancer auth tokens.
//
// Background:
//   - config.bundled.json hardcodes "auth.tokens": ["bundled-default"] (no
//     env hint in the file).
//   - cppworker, agent, webui все читают свои токены из .env.bundled-with-agent
//     через CPPWORKER_API_TOKEN / API_TOKEN / BALANCER_TOKEN.
//   - При свежем деплое с одним .env файлом: WebUI/agent отправляют правильный
//     токен (из env), балансер ожидает "bundled-default" (из config.json).
//     → HTTP 401 на /api/v1/backends и любых admin endpoint'ах.
//
// Fix: LB_API_TOKEN (single token) или LB_AUTH_TOKENS (CSV, для нескольких)
// ПОЛНОСТЬЮ заменяют conf.Auth.Tokens. Если env пустой — оставляем config.json
// (backward compat). Также принудительно включаем auth.Enabled=true когда
// env задан (иначе фикс молча теряется).
//
// Security: LB_API_TOKEN переопределяет config.json только в положительном
// случае (env set, non-empty). Negative path (env unset) → config.json wins
// → no surprise behavior change for operators who don't set the env.
package main

import (
	"os"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// applyEnvAuthTokens — Round 52 env override для conf.Auth.Tokens.
//
// Приоритет env: LB_API_TOKEN (single) > LB_AUTH_TOKENS (CSV) > config.json.
// Применяется ДО создания api.NewServer, чтобы authenticator видел уже
// финальный список токенов.
func applyEnvAuthTokens(auth *types.AuthConfig) {
	// Собираем список токенов из env. Сначала LB_API_TOKEN (single),
	// затем LB_AUTH_TOKENS (CSV). Если LB_API_TOKEN задан — он
	// ПОЛНОСТЬЮ заменяет config.json (LB_AUTH_TOKENS игнорируется,
	// иначе оператор может неожиданно для себя открыть доступ по
	// "вспомогательному" токену из LB_AUTH_TOKENS).
	var envTokens []string

	if single := strings.TrimSpace(os.Getenv("LB_API_TOKEN")); single != "" {
		envTokens = append(envTokens, single)
	} else if csv := os.Getenv("LB_AUTH_TOKENS"); csv != "" {
		for _, t := range strings.Split(csv, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				envTokens = append(envTokens, t)
			}
		}
	}

	if len(envTokens) == 0 {
		// Env не задан — оставляем config.json как есть.
		// Round 52 guard: warn if config.json still has the legacy "bundled-default"
		// token — this is the root cause of the 401 fresh-deploy bug.
		if auth.Enabled {
			for _, t := range auth.Tokens {
				if t == "bundled-default" {
					logger.Get().Warnw(
						"auth.tokens contains legacy 'bundled-default' from config.json and LB_API_TOKEN env is not set. "+
							"Fresh deploys with .env.bundled-with-agent (and not --env-file) will get 401 from balancer. "+
							"Set LB_API_TOKEN in your .env to fix this.",
						"hint", "export LB_API_TOKEN=changeme-bundled-with-agent-token")
					break
				}
			}
		}
		return
	}

	// Env задан — полностью заменяем config.json tokens.
	oldCount := len(auth.Tokens)
	auth.Tokens = envTokens
	auth.Enabled = true // force enable (если в config.json было false)
	logger.Get().Infow("R52: auth tokens overridden from environment (config.json replaced)",
		"old_count", oldCount,
		"new_count", len(envTokens),
		"source", "LB_API_TOKEN/LB_AUTH_TOKENS",
	)
}
