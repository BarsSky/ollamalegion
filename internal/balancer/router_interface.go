package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/types"
)

// BackendRouter — общий интерфейс маршрутизаторов бэкендов (OllamaRouter,
// LlamaCppRouter). Определяет контракт диспетчеризации и метаданные для
// observability/dispatch priority.
//
// Реализуют: *OllamaRouter, *LlamaCppRouter.
//
// Этот интерфейс — первый шаг R59.15 (2026-09-03). До него в routeRequest
// были три повторяющихся лестницы `if p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) { return true }`
// с условиями по типу бэкенда. С интерфейсом dispatch может быть выражен
// через единый `[]BackendRouter` с приоритетами.
//
// R59.15a: интерфейс введён, диспетчеризация пока использует конкретные
// поля (без изменения поведения). R59.15b: routeRequest будет переписан
// на итерацию приоритетного списка.
type BackendRouter interface {
	// Route — диспетчеризация запроса. Возвращает true если обработан.
	Route(w http.ResponseWriter, r *http.Request) bool

	// BackendType — тип бэкенда, который обслуживает этот роутер.
	// Используется для приоритизации и фильтрации в dispatch.
	BackendType() types.BackendType

	// Name — человекочитаемое имя для логов и observability.
	// Примеры: "OllamaRouter", "LlamaCppRouter".
	Name() string
}

// Compile-time check: *OllamaRouter и *LlamaCppRouter должны реализовывать
// BackendRouter. Если кто-то уберёт метод, сборка упадёт здесь с понятной
// ошибкой вместо "interface{} doesn't implement BackendRouter" в routeRequest.
var (
	_ BackendRouter = (*OllamaRouter)(nil)
	_ BackendRouter = (*LlamaCppRouter)(nil)
)
