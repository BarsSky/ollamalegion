// context_timeout.go — простой helper для создания контекста с таймаутом.
//
// Используется в горутинах auto-reload, чтобы корректно отменять UnloadModel/
// LoadModelWithOpts если они зависают (например, при долгой загрузке модели
// в условиях OOM или при зависшем bridge).
//
// Выделено в отдельный файл, чтобы не дублировать context.WithTimeout в
// handlers_config.go, handlers_model.go и reload_loop_protection_test.go.
package main

import (
	"context"
	"time"
)

// contextWithTimeout возвращает context с заданным таймаутом и функцию отмены.
// Использование:
//
//	ctx, cancel := contextWithTimeout(120 * time.Second)
//	defer cancel()
//	...
//
// При выходе из scope cancel() обязательно вызвать через defer, иначе
// горутина в LoadModelWithOpts может «утечь» после отмены контекста.
func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}