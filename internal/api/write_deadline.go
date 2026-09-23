package api

import (
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// extendWriteDeadline — R66d (2026-09-23): снимает серверный WriteTimeout для
// длинных admin-операций.
//
// ПРОБЛЕМА (подтверждена на живом стеке). apiHTTPServer (порт 18081) создан с
//
//	WriteTimeout: 60 * time.Second   // cmd/balancer/main.go:383
//
// Холодная загрузка модели длится дольше: gemma-4-E4B-it-Q4_K_M (4.2 GB) на
// RTX 3070 — 88 с, Qwen3.8-27B (15 GB) — минуты. Ответ балансера пишется уже
// ПОСЛЕ истечения write-deadline, поэтому Go молча закрывает соединение и
// клиент не получает ни одного байта (curl: `HTTP 000`, пустое тело, при этом
// в логах балансера `success:true` и модель реально загружена).
//
// Симптом в WebUI: кнопка «Загрузить» / смена настроек модели выглядит
// сломанной — GgufApi.manageModel не может прочитать ответ и показывает
// "Load failed: ...", хотя загрузка прошла успешно. Отсюда же жалоба
// «через форму WebUI не работает загрузка модели на llama.cpp-бэкенд».
//
// РЕШЕНИЕ: перед длинной операцией снять deadline
// (SetWriteDeadline(time.Time{}) = без таймаута). Это тот же приём, который уже
// применяется для SSE-ответов: handlers_events.go:120,
// gguf_backend_proxy.go:281, handlers_cppworker_apply_async.go:353.
//
// Если ResponseWriter не поддерживает SetWriteDeadline (обёртка/тест), просто
// логируем — поведение остаётся прежним (60s), операция не ломается.
func extendWriteDeadline(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		logger.Get().Warnw("extendWriteDeadline: SetWriteDeadline failed — "+
			"длинная операция может быть обрезана серверным WriteTimeout",
			"error", err)
	}
}
