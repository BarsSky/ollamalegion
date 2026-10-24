// image_tool_gate_r119.go — R119 (2026-10-15), пункт 2 плана: нехватка VRAM при
// генерации картинок сообщается ЯВНО и МАШИНОЧИТАЕМО, и это доезжает до модели
// через инструмент генерации, который предоставляет балансер.
//
// ЖИВОЙ ДЕФЕКТ (две половины одного и того же):
//
//  1. Прямой image-API (/v1/images/*, /sdapi/v1/*) гейт проходит и при нехватке
//     VRAM отвечает `503 {code:"insufficient_vram", hint:…, freeMb, requiredMb}`.
//     А ИНСТРУМЕНТ (`generate_image`), который балансер подкладывает в чат,
//     гейт НЕ проходил вообще: `generateImageForTool` резолвил цель, при нужде
//     грузил модель и сразу POST-ил `/api/image/generate`. То есть ровно тот путь,
//     которым пользуется модель по просьбе пользователя («нарисуй кота»),
//     не проверял ни VRAM, ни эксклюзивный лок GPU.
//  2. Ошибка этого пути уезжала модели как `{"status":"error","error":"<текст>"}` —
//     без кода и без чисел. Модель могла только пересказать невнятный текст
//     воркера, а пользователь не понимал, чего не хватает и что сделать.
//
// ТЕПЕРЬ: инструмент проходит тот же гейт (`beforeGeneration` — VRAM + лок GPU) и
// возвращает структурированный отказ: машинный код, числа (needed/free/headroom),
// подсказку OOM-лестницы и человеческую фразу. Результат уходит модели как
// tool-сообщение, поэтому она может честно объяснить пользователю причину.
package balancer

import (
	"errors"
	"fmt"
	"strings"

	"ollama-loadbalancer/internal/i18n"
)

// imageToolError — отказ инструмента генерации картинок с машиночитаемыми полями.
type imageToolError struct {
	// Code — машинный код причины (types.ImageGateInsufficientVRAM и др.).
	Code string
	// Message — человеческая фраза (её пересказывает модель).
	Message string
	// Hint — что сделать (OOM-лестница).
	Hint string
	// Числа: сколько нужно, сколько свободно, какой запас требует политика.
	RequiredMB int
	FreeMB     int
	HeadroomMB int
	// RetryAfterSec — сколько подождать (для «GPU занят другой генерацией»).
	RetryAfterSec int
	// Reason — исходная ошибка (если отказ пришёл не от гейта).
	Reason error
}

func (e *imageToolError) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = "image generation rejected"
	}
	if e.RequiredMB > 0 || e.FreeMB > 0 {
		msg = fmt.Sprintf("%s (needs %d MB, free %d MB, headroom %d MB)",
			msg, e.RequiredMB, e.FreeMB, e.HeadroomMB)
	}
	if e.Hint != "" {
		msg += ". Hint: " + e.Hint
	}
	return msg
}

func (e *imageToolError) Unwrap() error { return e.Reason }

// toolPayload — то, что получает МОДЕЛЬ как результат вызова инструмента.
//
// Машинный код и числа здесь не для красоты: по ним модель (и любой автоматический
// клиент) может выбрать действие — подождать (retryAfterSec), уменьшить разрешение,
// сменить квантование, попросить оператора освободить карту — и объяснить это
// пользователю словами.
func (e *imageToolError) toolPayload() map[string]interface{} {
	payload := map[string]interface{}{
		"status": "error",
		"error":  e.Error(),
	}
	if e.Code != "" {
		payload["code"] = e.Code
	}
	if e.Hint != "" {
		payload["hint"] = e.Hint
	}
	if e.Message != "" {
		payload["message"] = e.Message
	}
	if e.RequiredMB > 0 {
		payload["requiredMb"] = e.RequiredMB
	}
	if e.FreeMB > 0 {
		payload["freeMb"] = e.FreeMB
	}
	if e.HeadroomMB > 0 {
		payload["headroomMb"] = e.HeadroomMB
	}
	if e.RetryAfterSec > 0 {
		payload["retryAfterSec"] = e.RetryAfterSec
	}
	return payload
}

// imageToolErrorPayload — единая точка превращения ошибки инструмента в результат
// для модели. Используется ОБОИМИ циклами (OpenAI и Ollama): раньше каждый писал
// `{"status":"error","error": err.Error()}`, и код с числами терялся.
func imageToolErrorPayload(err error) map[string]interface{} {
	if err == nil {
		return map[string]interface{}{"status": "error", "error": i18n.T("balancer.image_tool_gate.result_missing")}
	}
	var gateErr *imageToolError
	if errors.As(err, &gateErr) {
		return gateErr.toolPayload()
	}
	return map[string]interface{}{"status": "error", "error": err.Error()}
}

// imageToolGateDenied — отказ гейта в виде структурированной ошибки инструмента.
func imageToolGateDenied(d imageGateDecision, model string) *imageToolError {
	code := strings.TrimSpace(d.Code)
	if code == "" {
		code = "image_gate_denied"
	}
	msg := strings.TrimSpace(d.Message)
	if msg == "" {
		msg = "image generation denied by the resource gate"
	}
	if model != "" {
		msg = fmt.Sprintf("%s (model %q)", msg, model)
	}
	return &imageToolError{
		Code:          code,
		Message:       msg,
		Hint:          strings.TrimSpace(d.Hint),
		RequiredMB:    d.Verdict.RequiredMB,
		FreeMB:        d.Verdict.FreeMB,
		HeadroomMB:    d.Verdict.HeadroomMB,
		RetryAfterSec: d.RetryAfterSec,
	}
}
