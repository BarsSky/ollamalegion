// adaptive_integration.go — интеграция адаптивного загрузчика в cppworker.
// Использует init() для регистрации роутов и перехвата обрывов стримов.
// НЕ требует правки main.go или router.go — всё через глобальные переменные.

package main

import (
	"net/http"
	"sync"
)

// routeRegistrations — список дополнительных роутов, которые регистрируются
// через setupRouter() при наличии адаптивного загрузчика.
var additionalRoutes []routeRegistration

type routeRegistration struct {
	pattern string
	handler http.HandlerFunc
}

// registerAdaptiveRoute добавляет роут, который будет зарегистрирован
// в setupRouter() если адаптивный загрузчик инициализирован.
func registerAdaptiveRoute(pattern string, handler http.HandlerFunc) {
	additionalRoutes = append(additionalRoutes, routeRegistration{pattern, handler})
}

// initAdaptiveLoaderOnce — защита от двойного init.
var initAdaptiveLoaderOnce sync.Once

// EnsureAdaptiveLoaderInit вызывается из setupRouter().
func EnsureAdaptiveLoaderInit() {
	initAdaptiveLoaderOnce.Do(func() {
		// Инициализируем с функцией reload
		initAdaptiveLoader(func(modelName string, reductionPct float64) error {
			// Unload model — балансер заметит и перезагрузит
			if err := backend.UnloadModel(modelName); err != nil {
				return err
			}
			return nil
		})
	})
}

// RecordStreamBreakWrapper — глобальный враппер, который подключается
// из safeStreamWriter.markBroken. Может быть nil (если не инициализирован).
var RecordStreamBreakWrapper func(modelName, reason, lastWriteErr string)

// init — регистрируем интеграцию при загрузке пакета.
func init() {
	// Регистрируем роуты для адаптивного API
	registerAdaptiveRoute("/api/v1/cppworker/adaptive/strategy", handleAdaptiveStrategy)
	registerAdaptiveRoute("/api/v1/cppworker/adaptive/environment", handleAdaptiveEnvironment)
	registerAdaptiveRoute("/api/v1/cppworker/adaptive/heal-config", handleAdaptiveHealConfig)

	// Подключаем NaN-детектор через глобальный враппер
	RecordStreamBreakWrapper = func(modelName, reason, lastWriteErr string) {
		RecordStreamBreak(modelName, reason, lastWriteErr)
	}

	_ = http.MethodGet // ensure net/http import
}
