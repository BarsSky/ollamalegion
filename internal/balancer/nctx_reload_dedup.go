// Package balancer — dedup in-flight reload между preflight и ensureModelLoaded.
//
// Проблема (2026-06-24): preflightNCtxReloadIfNeeded запускает executeAsyncReload
// в горутине (background reload). Параллельно с этим может прийти
// ensureModelLoadedOnBackend, который тоже пытается загрузить модель через
// ModelManager.LoadModel. Оба потока упираются в TryLockLoad cppworker'а,
// и balancer polling'ом видит state=loading — зависает в
// `concurrent load already in progress, waiting` loop.
//
// Решение: NCtxReloadCoordinator ведёт реестр in-flight reload'ов
// (per backendID). Новые вызовы проверяют реестр и либо дожидаются
// завершения существующего reload, либо возвращают ошибку.
//
// Использование:
//   - executeAsyncReload → coordinator.StartReloadIfNotPending(...)
//   - ensureModelLoadedOnBackend → coordinator.IsReloadPending(...)
//     и/или coordinator.WaitReloadDone(...)
package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// reloadKey — ключ в карте in-flight reload'ов: backendID + modelName.
// targetNCtx хранится для логирования.
type reloadKey struct {
	backendID  string
	modelName  string
	targetNCtx int
}

// reloadEntry — in-flight reload с done-каналом для ожидания.
type reloadEntry struct {
	done   chan struct{}
	err    error
	expiry time.Time // для TTL-очистки зависших записей
}

// reloadDedupRegistry — реестр in-flight reload'ов.
// Использует RWMutex: много читающих (IsReloadPending, WaitReloadDone)
// и редкие пишущие (Start, Finish).
type reloadDedupRegistry struct {
	mu      sync.RWMutex
	entries map[reloadKey]*reloadEntry

	// defaultTTL — если reload завис и не закрыл done-канал за это время,
	// запись удаляется из реестра (защита от утечки памяти при crash cppworker).
	defaultTTL time.Duration
}

// newReloadDedupRegistry создаёт реестр с default TTL 5 минут.
func newReloadDedupRegistry() *reloadDedupRegistry {
	return &reloadDedupRegistry{
		entries:    make(map[reloadKey]*reloadEntry),
		defaultTTL: 5 * time.Minute,
	}
}

// StartReloadIfNotPending — регистрирует reload и возвращает (entry, true),
// если reload ещё не запущен. Если reload с тем же (backendID, modelName,
// targetNCtx) уже идёт — возвращает (existingEntry, false) и не запускает
// второй (вызывающий может дождаться через WaitDone).
//
// callerFn вызывается ОДИН раз (для первого запроса) и должен закрыть
// entry.done по завершении. entry.err может быть nil (успех) или error.
//
// Потокобезопасно.
func (r *reloadDedupRegistry) StartReloadIfNotPending(
	backendID, modelName string,
	targetNCtx int,
	callerFn func(entry *reloadEntry),
) (*reloadEntry, bool) {
	r.mu.Lock()
	key := reloadKey{backendID: backendID, modelName: modelName, targetNCtx: targetNCtx}
	if existing, ok := r.entries[key]; ok {
		r.mu.Unlock()
		logger.Get().Debugw("reloadDedup: reusing in-flight reload",
			"backend", backendID, "model", modelName, "target_n_ctx", targetNCtx)
		return existing, false
	}
	entry := &reloadEntry{
		done:   make(chan struct{}),
		expiry: time.Now().Add(r.defaultTTL),
	}
	r.entries[key] = entry
	r.mu.Unlock()

	logger.Get().Infow("reloadDedup: starting new reload",
		"backend", backendID, "model", modelName, "target_n_ctx", targetNCtx)

	go func() {
		callerFn(entry)
		close(entry.done)
		r.mu.Lock()
		if cur, ok := r.entries[key]; ok && cur == entry {
			delete(r.entries, key)
		}
		r.mu.Unlock()
	}()

	return entry, true
}

// IsReloadPending возвращает true, если reload для данной модели сейчас идёт.
// Используется в ensureModelLoadedOnBackend чтобы не запускать LoadModel
// параллельно с reload.
func (r *reloadDedupRegistry) IsReloadPending(backendID, modelName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Проверяем любое targetNCtx для данной модели.
	now := time.Now()
	for k, v := range r.entries {
		if k.backendID == backendID && k.modelName == modelName {
			if v.expiry.After(now) {
				return true
			}
		}
	}
	return false
}

// WaitReloadDone ждёт завершения reload для данной модели (любого targetNCtx).
// timeout == 0 → без лимита.
//
// Возвращает nil при успехе (reload завершился), error при таймауте или
// если reload с такой моделью не запущен.
func (r *reloadDedupRegistry) WaitReloadDone(backendID, modelName string, timeout time.Duration) error {
	r.mu.RLock()
	var found *reloadEntry
	for k, v := range r.entries {
		if k.backendID == backendID && k.modelName == modelName {
			found = v
			break
		}
	}
	r.mu.RUnlock()

	if found == nil {
		// Reload не запущен — может уже завершился.
		return nil
	}

	if timeout == 0 {
		<-found.done
		return found.err
	}

	select {
	case <-found.done:
		return found.err
	case <-time.After(timeout):
		return errReloadTimeout
	}
}

// errReloadTimeout — sentinel для таймаута WaitReloadDone.
var errReloadTimeout = &reloadTimeoutError{}

type reloadTimeoutError struct{}

func (e *reloadTimeoutError) Error() string {
	return "reload dedup wait timeout"
}
