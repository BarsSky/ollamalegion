// model_ready_wait.go — R113 (2026-10-13): ожидание ГОТОВНОСТИ модели.
//
// ЗАЧЕМ. «Модель загружена» в коде означало «есть запись в b.models»: GetModel()
// отдаёт запись в ЛЮБОМ состоянии, включая loading. Пока идёт загрузка или
// перезагрузка (n_ctx другого клиента, auto-tune, параллельный запрос), запись уже
// существует — и вызывающий считал модель готовой. Дальше генерация падала с
// «model X is not loaded (state: loading)», а стриминговый путь превращал это в
// чанк done:true: клиент (Cline) видел ЗАВЕРШЕНИЕ хода, не получив ни одного
// токена. Живая жалоба: «COMPLETE прилетает до того, как вообще происходит
// потоковая генерация ответа».
//
// Правильная семантика — как у Ollama: запрос к загружаемой модели ЖДЁТ её
// готовности; ошибка отдаётся, только если загрузка провалилась или истёк предел
// ожидания (тогда клиент получает 503 + Retry-After и повторит, а не решит, что
// задача выполнена).
package cppbackend

import (
	"context"
	"errors"
	"time"
)

// ErrModelIsLoading — модель ещё не готова (загрузка/перезагрузка идёт, либо
// истёк предел ожидания). Вызывающий обязан отдать это как «повтори позже»
// (503 + Retry-After), а НЕ как завершённый ответ.
var ErrModelIsLoading = errors.New("model is loading")

// DefaultModelReadyPoll — период опроса состояния.
const DefaultModelReadyPoll = 500 * time.Millisecond

// ModelState — состояние модели (loaded/loading/error/unloaded) и признак наличия
// записи. Отдельный метод нужен потому, что GetModel состояние не проверяет.
func (b *Backend) ModelState(name string) (LoadState, bool) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()
	if !exists {
		return StateUnloaded, false
	}
	inst.mu.Lock()
	state := inst.info.State
	inst.mu.Unlock()
	return state, true
}

// WaitForModelReady — блокируется, пока модель не перейдёт в StateLoaded.
//
// Возвращает:
//   - nil, если модель готова;
//   - ErrModelIsLoading, если истёк limit или отменён ctx;
//   - ошибку загрузки, если модель перешла в StateError;
//   - ошибку «model not found», если записи нет вовсе (загрузку никто не начинал).
//
// poll <= 0 → DefaultModelReadyPoll; limit <= 0 → ждать без предела (только ctx).
func (b *Backend) WaitForModelReady(ctx context.Context, name string, poll, limit time.Duration) error {
	if poll <= 0 {
		poll = DefaultModelReadyPoll
	}
	var deadline time.Time
	if limit > 0 {
		deadline = time.Now().Add(limit)
	}

	for {
		state, exists := b.ModelState(name)
		switch {
		case exists && state == StateLoaded:
			return nil
		case exists && state == StateError:
			return errors.New("model " + name + " failed to load (state: error)")
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			if exists {
				return ErrModelIsLoading
			}
			return errors.New("model " + name + " not found")
		}

		if ctx != nil {
			select {
			case <-ctx.Done():
				return ErrModelIsLoading
			case <-time.After(poll):
			}
		} else {
			time.Sleep(poll)
		}
	}
}
