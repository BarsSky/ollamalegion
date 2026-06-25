//go:build llama_stub

// Package balancer — unit-тесты для queryCppWorkerModels (EOF-retry + lastKnownModels
// fallback), matchCppWorkerModel, и reloadDedupRegistry (дедупликация in-flight reload).
//
// Что покрываем:
//   - queryCppWorkerModels: 3 попытки при EOF с exponential backoff,
//     fallback на lastKnownModels кэш при полном провале, обновление кэша на успехе.
//   - matchCppWorkerModel: 3 варианта имени (точное / basename без .gguf / basename с .gguf).
//   - reloadDedupRegistry: StartReloadIfNotPending дедуплицирует параллельные вызовы,
//     IsReloadPending отслеживает состояние, WaitReloadDone возвращает timeout при превышении.
//
// Все тесты параллельные (нет общего состояния).
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// queryCppWorkerModels — EOF retry + lastKnownModels fallback
// ============================================================

// newProxyWithCppWorkerMock — создаёт Proxy с зарегистрированным cppworker-моком
// (httptest-сервер на /api/models). Возвращает proxy и сам httptest-сервер
// (для программного управления ответами в тестах).
func newProxyWithCppWorkerMock(t *testing.T, backendID string) (*Proxy, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// default: пустой список. Тесты могут подменить srv через srv.Config.Handler
		// или работать с дефолтным ответом.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count":  0,
			"models": []map[string]interface{}{},
		})
	}))
	host, port := splitHostPort(t, srv.URL)
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{
		Backend: &types.Backend{
			ID:     backendID,
			Host:   host,
			Status: types.StatusHealthy,
			Type:   types.BackendTypeLlamaCpp,
		},
	}
	// Сохраняем port отдельно через поле (если есть — иначе используем
	// getBackendPort, который для CppWorker возвращает CppWorkerPort / EnginePort).
	p.backends[backendID].Backend.CppWorkerPort = port
	return p, srv
}

// splitHostPort — выделяет host и port из URL httptest-сервера.
func splitHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	// url вида http://127.0.0.1:34567
	const prefix = "http://"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("unexpected URL: %q", url)
	}
	addr := url[len(prefix):]
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		t.Fatalf("no port in URL: %q", url)
	}
	host := addr[:idx]
	portStr := addr[idx+1:]
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

// withCppWorkerHandler — подменяет handler httptest-сервера на время теста.
// Используется для эмуляции EOF / 500 / успеха.
func withCppWorkerHandler(t *testing.T, srv *httptest.Server, h http.HandlerFunc) {
	t.Helper()
	srv.Config.Handler = h
}

// TestQueryCppWorkerModels_BackendNotFound — если бэкенд не зарегистрирован, returns nil.
func TestQueryCppWorkerModels_BackendNotFound(t *testing.T) {
	t.Parallel()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	lr := NewLlamaCppRouter(p)
	if got := lr.queryCppWorkerModels("nonexistent"); got != nil {
		t.Errorf("query для nonexistent backend = %v, want nil", got)
	}
}

// TestQueryCppWorkerModels_Success — успешный ответ декодируется и возвращается.
func TestQueryCppWorkerModels_Success(t *testing.T) {
	t.Parallel()
	p, srv := newProxyWithCppWorkerMock(t, "cpp-1")
	defer srv.Close()
	lr := NewLlamaCppRouter(p)

	withCppWorkerHandler(t, srv, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": 2,
			"models": []map[string]interface{}{
				{"name": "qwen2.5.gguf", "state": "loaded", "path": "/models/qwen2.5.gguf"},
				{"name": "llama3.gguf", "state": "unloaded", "path": "/models/llama3.gguf"},
			},
		})
	})

	got := lr.queryCppWorkerModels("cpp-1")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "qwen2.5.gguf" || got[0].State != "loaded" {
		t.Errorf("models[0] = %+v, want qwen2.5.gguf/loaded", got[0])
	}
	// Кэш должен обновиться.
	lr.lastKnownModelsMu.RLock()
	cached, ok := lr.lastKnownModels["cpp-1"]
	lr.lastKnownModelsMu.RUnlock()
	if !ok || len(cached) != 2 {
		t.Errorf("lastKnownModels не обновлён: ok=%v, len=%d", ok, len(cached))
	}
}

// TestQueryCppWorkerModels_RetryOnEOF — EOF на 1-й попытке → успех на 2-й попытке.
// C-bridge может моргнуть на reload (RST соединения). Без retry balancer polling'ом
// зависает в `concurrent load already in progress, waiting`.
func TestQueryCppWorkerModels_RetryOnEOF(t *testing.T) {
	t.Parallel()
	p, srv := newProxyWithCppWorkerMock(t, "cpp-retry")
	defer srv.Close()
	lr := NewLlamaCppRouter(p)

	var attempt atomic.Int32
	withCppWorkerHandler(t, srv, func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n == 1 {
			// Эмулируем EOF: hijack и закрываем соединение без ответа.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter не поддерживает Hijacker")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		// 2-я попытка — успех.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": 1,
			"models": []map[string]interface{}{
				{"name": "recovered-model.gguf", "state": "loaded"},
			},
		})
	})

	got := lr.queryCppWorkerModels("cpp-retry")
	if attempt.Load() != 2 {
		t.Errorf("attempts = %d, want 2 (1 EOF + 1 success)", attempt.Load())
	}
	if len(got) != 1 || got[0].Name != "recovered-model.gguf" {
		t.Errorf("unexpected result: %+v", got)
	}
}

// TestQueryCppWorkerModels_AllRetriesFail_FallbackToCache — 3 провала → fallback
// на lastKnownModels, если ему < 30 секунд.
func TestQueryCppWorkerModels_AllRetriesFail_FallbackToCache(t *testing.T) {
	t.Parallel()
	p, srv := newProxyWithCppWorkerMock(t, "cpp-fb")
	defer srv.Close()
	lr := NewLlamaCppRouter(p)

	// Предзаполняем кэш: "старый успешный snapshot".
	lr.lastKnownModelsMu.Lock()
	lr.lastKnownModels["cpp-fb"] = []cppWorkerModelState{
		{Name: "cached-model.gguf", Path: "/models/cached.gguf", State: "loaded"},
	}
	lr.lastKnownModelsAt["cpp-fb"] = time.Now() // свежий (< 30s)
	lr.lastKnownModelsMu.Unlock()

	// Все 3 попытки — 500.
	var attempt atomic.Int32
	withCppWorkerHandler(t, srv, func(w http.ResponseWriter, r *http.Request) {
		attempt.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	got := lr.queryCppWorkerModels("cpp-fb")
	if attempt.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempt.Load())
	}
	if len(got) != 1 || got[0].Name != "cached-model.gguf" {
		t.Errorf("fallback не сработал, got = %+v", got)
	}
}

// TestQueryCppWorkerModels_AllRetriesFail_StaleCache — кэш старше 30s → nil,
// polling ничего не находит, ensureModelLoadedOnBackend пойдёт в LoadModel.
func TestQueryCppWorkerModels_AllRetriesFail_StaleCache(t *testing.T) {
	t.Parallel()
	p, srv := newProxyWithCppWorkerMock(t, "cpp-stale")
	defer srv.Close()
	lr := NewLlamaCppRouter(p)

	// "Старый" snapshot — 31 секунда назад.
	lr.lastKnownModelsMu.Lock()
	lr.lastKnownModels["cpp-stale"] = []cppWorkerModelState{
		{Name: "old-cached.gguf", State: "loaded"},
	}
	lr.lastKnownModelsAt["cpp-stale"] = time.Now().Add(-31 * time.Second)
	lr.lastKnownModelsMu.Unlock()

	withCppWorkerHandler(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if got := lr.queryCppWorkerModels("cpp-stale"); got != nil {
		t.Errorf("stale cache должен игнорироваться, got = %+v", got)
	}
}

// TestQueryCppWorkerModels_DecodeErrorRetried — невалидный JSON → retry.
func TestQueryCppWorkerModels_DecodeErrorRetried(t *testing.T) {
	t.Parallel()
	p, srv := newProxyWithCppWorkerMock(t, "cpp-dec")
	defer srv.Close()
	lr := NewLlamaCppRouter(p)

	var attempt atomic.Int32
	withCppWorkerHandler(t, srv, func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n <= 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not-a-json"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": 1,
			"models": []map[string]interface{}{
				{"name": "decoded.gguf", "state": "loaded"},
			},
		})
	})

	got := lr.queryCppWorkerModels("cpp-dec")
	if attempt.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (2 decode errors + 1 success)", attempt.Load())
	}
	if len(got) != 1 || got[0].Name != "decoded.gguf" {
		t.Errorf("unexpected result: %+v", got)
	}
}

// ============================================================
// matchCppWorkerModel — 3 варианта имени
// ============================================================

// TestMatchCppWorkerModel_ExactName — точное совпадение имени.
func TestMatchCppWorkerModel_ExactName(t *testing.T) {
	t.Parallel()
	m := cppWorkerModelState{Name: "qwen2.5-coder.gguf", Path: "/models/qwen2.5-coder.gguf"}
	if !matchCppWorkerModel("qwen2.5-coder.gguf", m) {
		t.Error("точное имя должно совпасть")
	}
}

// TestMatchCppWorkerModel_BasenameWithoutGGUF — путь заканчивается на .gguf,
// но клиент передал имя без расширения.
func TestMatchCppWorkerModel_BasenameWithoutGGUF(t *testing.T) {
	t.Parallel()
	m := cppWorkerModelState{Name: "", Path: "/models/qwen2.5-coder.gguf"}
	if !matchCppWorkerModel("qwen2.5-coder", m) {
		t.Error("basename без .gguf должен совпасть (qwen2.5-coder == qwen2.5-coder.gguf)")
	}
}

// TestMatchCppWorkerModel_BasenameWithGGUF — клиент передал .gguf, у модели только path.
func TestMatchCppWorkerModel_BasenameWithGGUF(t *testing.T) {
	t.Parallel()
	m := cppWorkerModelState{Name: "", Path: "/models/llama3.gguf"}
	if !matchCppWorkerModel("llama3.gguf", m) {
		t.Error("basename с .gguf должен совпасть")
	}
}

// TestMatchCppWorkerModel_NoMatch — не должно ложно совпадать.
func TestMatchCppWorkerModel_NoMatch(t *testing.T) {
	t.Parallel()
	m := cppWorkerModelState{Name: "qwen.gguf", Path: "/models/qwen.gguf"}
	if matchCppWorkerModel("llama3", m) {
		t.Error("несовпадающее имя должно возвращать false")
	}
}

// TestMatchCppWorkerModel_EmptyPath — без path проверяется только Name == modelName.
func TestMatchCppWorkerModel_EmptyPath(t *testing.T) {
	t.Parallel()
	m := cppWorkerModelState{Name: "exact", Path: ""}
	if !matchCppWorkerModel("exact", m) {
		t.Error("exact match должен работать без path")
	}
	if matchCppWorkerModel("different", m) {
		t.Error("разные имена не должны совпадать")
	}
}

// ============================================================
// reloadDedupRegistry — дедупликация in-flight reload
// ============================================================

// TestReloadDedup_FirstCallRegisters — первый вызов регистрирует reload, возвращает (entry, true=true).
func TestReloadDedup_FirstCallRegisters(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	var done sync.WaitGroup
	done.Add(1)
	entry, started := r.StartReloadIfNotPending("backend-A", "model-X", 8192, func(e *reloadEntry) {
		// Симулируем reload. callerFn не закрывает entry.done — это делает
		// обёртка StartReloadIfNotPending после возврата callerFn.
		done.Done()
	})
	// callerFn быстро вернётся (done.Done → возврат), обёртка закроет entry.done.
	// Тест ждёт на done.Wait, поэтому defer cleanup не нужен.
	if !started {
		t.Fatal("первый вызов должен вернуть started=true")
	}
	if entry == nil {
		t.Fatal("entry не должен быть nil")
	}
	// Проверяем, что reload теперь помечен как pending.
	if !r.IsReloadPending("backend-A", "model-X") {
		t.Error("после StartReloadIfNotPending IsReloadPending должен быть true")
	}
	done.Wait()
}

// safeCloseEntry — закрывает entry.done ровно один раз (защита от double-close).
// Используется в defer теста для гарантированного закрытия, когда callerFn
// не должен закрывать канал сам (контракт StartReloadIfNotPending:
// callerFn делает работу и возвращает, close делает обёртка).
func safeCloseEntry(entry *reloadEntry) {
	defer func() { _ = recover() }()
	close(entry.done)
}

// TestReloadDedup_SecondCallReuses — параллельный вызов с тем же key
// получает (existingEntry, false) и не запускает второй callerFn.
func TestReloadDedup_SecondCallReuses(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	// Запускаем первый reload и блокируем его.
	blocker := make(chan struct{})
	entry1, started1 := r.StartReloadIfNotPending("backend-A", "model-Y", 16384, func(e *reloadEntry) {
		<-blocker // ждём сигнала из теста
		// Контракт: callerFn не закрывает entry.done — это делает обёртка
		// StartReloadIfNotPending после callerFn возврата.
	})
	if !started1 {
		t.Fatal("первый вызов должен вернуть started=true")
	}

	// Второй вызов с тем же (backendID, modelName, targetNCtx) — должен вернуть тот же entry и started=false.
	entry2, started2 := r.StartReloadIfNotPending("backend-A", "model-Y", 16384, func(e *reloadEntry) {
		t.Errorf("второй callerFn НЕ должен быть вызван")
	})
	// Разблокируем первый callerFn и ждём завершения (обёртка закроет entry1.done).
	close(blocker)
	select {
	case <-entry1.done:
	case <-time.After(2 * time.Second):
		t.Fatal("callerFn не завершился за 2s")
	}

	if started2 {
		t.Error("второй вызов должен вернуть started=false (дедуп)")
	}
	if entry1 != entry2 {
		t.Error("entry должен быть тем же (existing)")
	}
}

// TestReloadDedup_WaitDoneSuccess — WaitReloadDone возвращает nil после успешного reload.
func TestReloadDedup_WaitDoneSuccess(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	entry, started := r.StartReloadIfNotPending("backend-B", "model-Z", 4096, func(e *reloadEntry) {
		// callerFn не закрывает entry.done — обёртка StartReloadIfNotPending
		// делает это после возврата callerFn.
	})
	if !started {
		t.Fatal("первый вызов должен вернуть started=true")
	}
	// Дожидаемся завершения обёртки (она закрыла entry.done после возврата callerFn).
	select {
	case <-entry.done:
	case <-time.After(2 * time.Second):
		t.Fatal("callerFn не завершился за 2s")
	}
	// Теперь WaitReloadDone должен сразу вернуть nil (reload уже завершён,
	// запись удалена из реестра → found == nil → return nil).
	if err := r.WaitReloadDone("backend-B", "model-Z", 100*time.Millisecond); err != nil {
		t.Errorf("WaitReloadDone после завершения = %v, want nil", err)
	}
	if r.IsReloadPending("backend-B", "model-Z") {
		t.Error("после завершения IsReloadPending должен быть false")
	}
}

// TestReloadDedup_WaitDoneTimeout — WaitReloadDone с timeout, который истекает
// раньше завершения reload, возвращает errReloadTimeout.
func TestReloadDedup_WaitDoneTimeout(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	_, started := r.StartReloadIfNotPending("backend-C", "model-W", 8192, func(e *reloadEntry) {
		// Долгий reload — тест не дождётся. callerFn не закрывает канал.
		time.Sleep(2 * time.Second)
	})
	// Без defer cleanup: тест проверяет только поведение WaitReloadDone
	// (timeout). Горутина callerFn живёт ~2s, но не помешает другим тестам.
	if !started {
		t.Fatal("первый вызов должен вернуть started=true")
	}

	start := time.Now()
	err := r.WaitReloadDone("backend-C", "model-W", 100*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitReloadDone должен вернуть ошибку при таймауте")
	}
	if err.Error() != "reload dedup wait timeout" {
		t.Errorf("error = %q, want \"reload dedup wait timeout\"", err.Error())
	}
	if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("WaitDone elapsed = %v, want ~100ms", elapsed)
	}
}

// TestReloadDedup_WaitDoneNoPending — WaitReloadDone для незарегистрированной
// модели возвращает nil (reload не запущен → ничего не ждём).
func TestReloadDedup_WaitDoneNoPending(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	if err := r.WaitReloadDone("nonexistent", "model", 1*time.Second); err != nil {
		t.Errorf("WaitDone для незарегистрированного reload = %v, want nil", err)
	}
}

// TestReloadDedup_DifferentTargetNCtxIsSeparate — разный targetNCtx → отдельная запись
// (поведение дедупа по полному ключу, не только по modelName).
func TestReloadDedup_DifferentTargetNCtxIsSeparate(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	blocker := make(chan struct{})
	entry1, started1 := r.StartReloadIfNotPending("backend-D", "model-V", 4096, func(e *reloadEntry) {
		<-blocker
		// callerFn не закрывает entry.done — обёртка закроет после возврата.
	})
	if !started1 {
		t.Fatal("первый вызов должен вернуть started=true")
	}

	// Другой targetNCtx — это отдельный reload (для теста n_ctx идёт в ключ).
	entry2, started2 := r.StartReloadIfNotPending("backend-D", "model-V", 16384, func(e *reloadEntry) {
		<-blocker
		// callerFn не закрывает entry.done — обёртка закроет после возврата.
	})
	if !started2 {
		t.Error("reload с другим targetNCtx должен стартовать отдельно")
	}
	if entry1 == entry2 {
		t.Error("entry должны различаться")
	}
	// cleanup: разблокируем оба reload (каждый в отдельной горутине) и ждём закрытия.
	close(blocker)
	select {
	case <-entry1.done:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-entry2.done:
	case <-time.After(2 * time.Second):
	}
}

// TestReloadDedup_ConcurrentSameKeyDedups — N параллельных горутин делают
// StartReloadIfNotPending с тем же ключом → callerFn вызывается ОДИН раз.
func TestReloadDedup_ConcurrentSameKeyDedups(t *testing.T) {
	t.Parallel()
	r := newReloadDedupRegistry()

	var callerCount atomic.Int32
	blocker := make(chan struct{})
	// Регистрируем через первый вызов.
	entry1, _ := r.StartReloadIfNotPending("backend-E", "model-U", 8192, func(e *reloadEntry) {
		callerCount.Add(1)
		<-blocker
		// callerFn не закрывает entry.done.
	})
	defer close(blocker)
	// cleanup: ждём закрытия entry1.done (обёртка закроет после возврата callerFn).
	select {
	case <-entry1.done:
	case <-time.After(2 * time.Second):
	}

	// 50 параллельных попыток с тем же ключом — все должны получить started=false.
	var wg sync.WaitGroup
	wg.Add(50)
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			_, started := r.StartReloadIfNotPending("backend-E", "model-U", 8192, func(e *reloadEntry) {
				callerCount.Add(1) // этот callback НЕ должен быть вызван
			})
			if started {
				t.Errorf("параллельная попытка должна вернуть started=false (дедуп)")
			}
		}()
	}
	wg.Wait()

	// Даём callerFn немного времени на запуск, чтобы он успел инкрементнуть counter.
	time.Sleep(50 * time.Millisecond)
	if got := callerCount.Load(); got != 1 {
		t.Errorf("callerFn вызван %d раз, want 1 (дедуп)", got)
	}
}
