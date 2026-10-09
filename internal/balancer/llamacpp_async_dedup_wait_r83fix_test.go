//go:build llama_stub

// llamacpp_async_dedup_wait_r83fix_test.go — регресс на дефект, найденный живой
// проверкой стенда 2026-10-09 (6 пользователей → 1 обслужен, 5 получили 503).
//
// ЧТО БЫЛО. На холодную модель приходят N запросов одновременно. Первый
// реально запускает загрузку (ModelManager.ExecuteOperation блокируется на всё
// время загрузки), остальные получают от дедупликации
//
//	operation 'load' for model 'X' is already in progress on backend 'Y'
//
// Эта строка уходила в сигнальный канал loadDone как «провал» →
// waitForModelLoad/drainLoadFailure классифицировал её как "model load failed"
// → клиент получал 503 «load started, waiting for cppworker» через ~0.5 с,
// хотя загрузка шла нормально и завершалась успехом. Обслуживался только
// «победитель» дедупликации.
//
// Живой лог стенда (13:43:58):
//
//	handleOpenAIChatCompletions: auto-load failed error="model \"gemma-...\" load
//	  started, waiting for cppworker to finish (~30-180s depending on n_ctx)"
//	  (×5, через 480 мс) — при этом франк дождался загрузки за 127 с.
//
// ПОСЛЕ ФИКСА. «already in progress» не считается провалом: ожидающий поллит
// состояние загрузки (fetchModelLoadState) и дожидается её завершения в бюджете
// autoLoadWaitTimeoutForModel — как и задумано в R67a.

package balancer

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestEnsureModelLoaded_AsyncInProgress_AllWaitersServed — N одновременных
// запросов на холодную модель: загрузку запускает ровно один (дедупликация),
// но ЖДАТЬ и получить nil-ошибку должны ВСЕ — до фикса 4 из 5 получали
// "already in progress" как отказ загрузки.
func TestEnsureModelLoaded_AsyncInProgress_AllWaitersServed(t *testing.T) {
	// В этом пакете TestMain обнуляет LB_AUTO_LOAD_WAIT_SEC
	// (waits_testmain_test.go), иначе ожидание в тестах длилось бы минуты.
	// Здесь само ожидание — предмет проверки.
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "8")

	var (
		loaded    atomic.Bool
		elapsedMs atomic.Int64
		loadPosts atomic.Int64
	)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			if loaded.Load() {
				_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"async-dedup","state":"loaded"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load/progress":
			w.Header().Set("Content-Type", "application/json")
			if loaded.Load() {
				_, _ = w.Write([]byte(`{"state":"loaded"}`))
				return
			}
			// Растущий elapsedMs — признак живой загрузки (R83 §9.1).
			_, _ = w.Write([]byte(`{"state":"loading","elapsedMs":` +
				strconv.FormatInt(elapsedMs.Add(500), 10) + `}`))
		case "/api/models/load", "/api/models/load-with-params":
			loadPosts.Add(1)
			<-release
			loaded.Store(true)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"loaded"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer func() {
		// release закрывается явно ниже; повторный close паникует.
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"async-dedup": {ContextLength: 4096, NumGPULayers: -1},
			},
		},
		backends:        map[string]*BackendState{},
		metricsMgr:      NewMetricsManager(),
		modelManager:    mm,
		lbAutoLoadAsync: true,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	lr := NewLlamaCppRouter(p)

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := lr.ensureModelLoadedOnBackend("test-bk", "async-dedup")
			errs[i] = err
		}(i)
	}
	close(start)

	// Даём всем N запросам дойти до кика загрузки (первый заблокируется на
	// HTTP-загрузке, остальные получат «already in progress» от дедупликации),
	// затем отпускаем загрузку.
	time.Sleep(300 * time.Millisecond)
	close(release)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("ожидание загрузки не завершилось за 60 с: «already in progress» снова обрывает ожидание")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("запрос %d: получена ошибка %v — дедупликация «already in progress» не должна превращаться в отказ", i, err)
		}
	}
	// Не-вакуумность: реальную загрузку запустил ровно один запрос, остальные
	// прошли через дедупликацию и всё равно дождались.
	if got := loadPosts.Load(); got != 1 {
		t.Errorf("POST /api/models/load выполнен %d раз, ожидался 1 (дедупликация ModelManager)", got)
	}
}
