package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
)

// Round 41 (2026-08-19): DRY-helper для HTTP-запросов к балансировщику.
//
// До R41 три места (register, metrics, heartbeat) дублировали одну и ту же
// последовательность: создать запрос → выставить Content-Type → выставить
// X-Agent-ID. Заголовок X-API-Token НЕ выставлялся ни в одном из них, что
// при включённом auth на балансере давало 401 и бесконечный restart-loop
// (см. internal/api/auth.go:56 — AuthMiddleware читает a.headerName, по
// умолчанию "X-API-Token").
//
// authedRequest инкапсулирует контракт:
//   - Content-Type: application/json (для POST с body)
//   - X-Agent-ID: <agentId>
//   - X-API-Token: <balancerToken> — ТОЛЬКО если cfg.BalancerToken != ""
//
// Пустой токен = заголовок не выставляется. Это backward-compat для
// dev-режима (auth выключен на балансере). В проде (auth: enabled) пустой
// токен даст 401 — это документированное поведение, и cmd/agent/main.go
// пишет WARNING на старте если токен пустой.
//
// Возвращает *http.Request готовый к Do(). Не выполняет сам запрос —
// разделение позволяет caller-у добавить свои заголовки (например,
// X-Backend-ID для register-attach-payload) и явно управлять lifecycle.
//
// body == nil допустим для GET/DELETE. Для POST с body используйте
// bytes.NewReader([]byte) или strings.NewReader.
func (a *Agent) authedRequest(ctx context.Context, method, url string, body *bytes.Reader) (*http.Request, error) {
	var bodyReader = body
	if bodyReader == nil {
		// http.NewRequestWithContext паникует на nil body для POST — оборачиваем в NoBody.
		// NoBody — это http.NoBody, sentinel для запросов без тела.
		bodyReader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to build request %s %s: %w", method, url, err)
	}

	// Content-Type: application/json — всегда, даже для GET (некоторые балансеры
	// проверяют). Можно переопределить после возврата, если нужен другой.
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json")
	}

	// X-Agent-ID — agent identifier для логов балансера и dedup-проверок.
	req.Header.Set("X-Agent-ID", a.config.AgentID)

	// X-API-Token — auth на балансере. Только если задан.
	if a.config.BalancerToken != "" {
		req.Header.Set("X-API-Token", a.config.BalancerToken)
	}

	return req, nil
}
