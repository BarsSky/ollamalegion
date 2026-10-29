// live_copy_probe_r303_test.go — R303: D4 двухузлового отчёта.
//
// ЖИВОЙ ДЕФЕКТ. Модель загружена на доноре, кэш метрик её ещё не видел
// (поллер раз в ~30 c). Запрос через балансер:
//
//	19:07:42  routeRequest: /api/chat
//	          … 5 минут тишины, мастер не получил ни одного POST /api/models/load
//	19:12:42  клиенту 503 «being reloaded to n_ctx=32768 (loaded=0); retry in 30s»
//
// То есть балансер выбрал ПУСТОЙ узел, preflight на нём честно увидел «модели
// нет» и запланировал загрузку с нуля; клиент отвисел всё окно ожидания и
// получил отказ с числами, которых не было (модель была жива рядом).
//
// Тесты фиксируют правило R303: перед «грузить/растить ЗДЕСЬ» спрашиваем живой
// кластер, где модель уже загружена, и, если она загружена на другом узле,
// уходим туда; а если этот узел недоступен для опроса — не выдумываем «модели
// нет» и не блокируем загрузку (loadedWindowUnknown).
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// fakeCppWorkerR303 — минимальный cppworker: отдаёт /api/models из строки.
// hits считает запросы, чтобы проверить кэш опроса (TTL), а не только ответ.
func fakeCppWorkerR303(t *testing.T, modelsJSON string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// backendStateR303 — BackendState с host/port от httptest-сервера.
func backendStateR303(t *testing.T, id, srvURL string) *BackendState {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", srvURL, err)
	}
	host := u.Hostname()
	port, perr := strconv.Atoi(u.Port())
	if perr != nil {
		t.Fatalf("порт из %q: %v", srvURL, perr)
	}
	return &BackendState{Backend: &types.Backend{
		ID: id, Host: host, CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 1, Weight: 1,
	}}
}

// newLiveProbeProxyR303 — прокси с ДВУМЯ живыми cppworker'ами и ПУСТЫМ кэшем
// метрик: именно это состояние было на стенде (свежая загрузка на доноре).
func newLiveProbeProxyR303(t *testing.T, emptyURL, donorURL string) *Proxy {
	t.Helper()
	resetLiveCopyProbeCacheR303()
	t.Cleanup(resetLiveCopyProbeCacheR303)
	p := &Proxy{
		backends: map[string]*BackendState{
			"node-empty": backendStateR303(t, "node-empty", emptyURL),
			"node-donor": backendStateR303(t, "node-donor", donorURL),
		},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
		queueMgr:     NewQueueManager(nil, 10, 1, 30),
		config:       &types.LoadBalancerConfig{},
		statePath:    t.TempDir() + "/state.json",
	}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	return p
}

const liveProbeModelR303 = "gemma-4-E4B-it-Q4_K_M"

// TestR303_LiveProbeFindsLoadedCopyWithoutCache — главный случай D4: в кэше
// копий нет, но модель ЖИВА на доноре — находим её живым опросом.
func TestR303_LiveProbeFindsLoadedCopyWithoutCache(t *testing.T) {
	var donorHits int32
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		&donorHits)

	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	alt, perSeq, ok := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384, nil)
	if !ok || alt != "node-donor" {
		t.Fatalf("живой опрос не нашёл загруженную копию: alt=%q perSeq=%d ok=%v", alt, perSeq, ok)
	}
	if perSeq != 32768 {
		t.Errorf("perSeq=%d, ожидалось 32768 (окно на слот с донора)", perSeq)
	}
	if got := atomic.LoadInt32(&donorHits); got != 1 {
		t.Errorf("донор опрошен %d раз, ожидался 1", got)
	}

	// Второй вызов должен попасть в TTL-кэш, а не опрашивать узел снова:
	// иначе каждый клиентский запрос бил бы по всем узлам кластера.
	if alt2, _, ok2 := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384, nil); !ok2 || alt2 != alt {
		t.Fatalf("повторный опрос дал другое решение: %q ok=%v", alt2, ok2)
	}
	if got := atomic.LoadInt32(&donorHits); got != 1 {
		t.Errorf("TTL-кэш не сработал: донор опрошен %d раз", got)
	}
}

// TestR303_LiveProbePrefersCoveringWindow — если загружены ДВЕ копии, берём ту,
// чьё окно покрывает запрос (иначе клиент получит «окно меньше запрошенного»).
func TestR303_LiveProbePrefersCoveringWindow(t *testing.T) {
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	small := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":8192,"context_per_seq":8192}]}`,
		nil)
	big := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		nil)

	resetLiveCopyProbeCacheR303()
	t.Cleanup(resetLiveCopyProbeCacheR303)
	p := &Proxy{
		backends: map[string]*BackendState{
			"node-a-small": backendStateR303(t, "node-a-small", small.URL),
			"node-b-big":   backendStateR303(t, "node-b-big", big.URL),
			// Пустой узел с меньшим ID: детерминированный порядок не должен
			// склонять выбор к нему.
			"node-0-empty": backendStateR303(t, "node-0-empty", empty.URL),
		},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
		queueMgr:     NewQueueManager(nil, 10, 1, 30),
		config:       &types.LoadBalancerConfig{},
		statePath:    t.TempDir() + "/state.json",
	}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)

	alt, perSeq, ok := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384, nil)
	if !ok || alt != "node-b-big" {
		t.Fatalf("выбрана копия %q (perSeq=%d), ожидалась покрывающая окно node-b-big", alt, perSeq)
	}
}

// TestR303_LiveProbeExcludesCurrentBackend — текущий узел в кандидаты не идёт
// (иначе «переезд» может вернуть тот же узел — пинг-понг).
func TestR303_LiveProbeExcludesCurrentBackend(t *testing.T) {
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		nil)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	alt, _, ok := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384, map[string]bool{"node-donor": true})
	if ok || alt != "" {
		t.Fatalf("исключённый узел всё равно выбран: %q", alt)
	}
}

// TestR303_LiveProbeFlagOff — LB_LIVE_COPY_PROBE=0 возвращает прежнее поведение
// (никаких опросов кластера) без рестарта.
func TestR303_LiveProbeFlagOff(t *testing.T) {
	t.Setenv("LB_LIVE_COPY_PROBE", "0")

	var donorHits int32
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768}]}`,
		&donorHits)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	if alt, _, ok := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384, nil); ok {
		t.Fatalf("при выключенном флаге всё равно выбран узел %q", alt)
	}
	if got := atomic.LoadInt32(&donorHits); got != 0 {
		t.Errorf("при выключенном флаге узел опрошен %d раз", got)
	}
}

// TestR303_NotLoadedHereButAliveElsewhere — preflight-решение: «здесь модели нет
// (опрос прошёл), а на другом узле она живая» → не планируем загрузку с нуля.
func TestR303_NotLoadedHereButAliveElsewhere(t *testing.T) {
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		nil)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	alt, perSeq, ok := p.modelNotLoadedHereButAliveElsewhereR303("node-empty", liveProbeModelR303, 16384)
	if !ok || alt != "node-donor" || perSeq != 32768 {
		t.Fatalf("решение preflight: alt=%q perSeq=%d ok=%v, ожидался node-donor/32768", alt, perSeq, ok)
	}
}

// TestR303_NotLoadedHere_UnknownStateIsNotAbsent — если опрос текущего узла НЕ
// прошёл (состояние неизвестно), запрет на загрузку не выдаётся: иначе один
// недоступный узел останавливал бы нормальную загрузку модели.
func TestR303_NotLoadedHere_UnknownStateIsNotAbsent(t *testing.T) {
	// Донор живой и модель у него есть.
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768}]}`,
		nil)
	// «Мёртвый» узел: сервер уже закрыт → опрос невозможен (state=unknown).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	resetLiveCopyProbeCacheR303()
	t.Cleanup(resetLiveCopyProbeCacheR303)
	p := &Proxy{
		backends: map[string]*BackendState{
			"node-dead":  backendStateR303(t, "node-dead", deadURL),
			"node-donor": backendStateR303(t, "node-donor", donor.URL),
		},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
		queueMgr:     NewQueueManager(nil, 10, 1, 30),
		config:       &types.LoadBalancerConfig{},
		statePath:    t.TempDir() + "/state.json",
	}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)

	if alt, perSeq, ok := p.modelNotLoadedHereButAliveElsewhereR303("node-dead", liveProbeModelR303, 16384); ok {
		t.Fatalf("по недостоверным данным выдан запрет загрузки (alt=%q perSeq=%d) — "+
			"недоступный узел не должен блокировать нормальную загрузку", alt, perSeq)
	}
}

// TestR303_SelectionUsesLiveProbe — селекция узла для Ollama-поверхности:
// при пустом кэше метрик уходит на узел с живой копией, а не на пустой.
func TestR303_SelectionUsesLiveProbe(t *testing.T) {
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		nil)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	got := p.selectBackendPreferWindow(liveProbeModelR303, types.BackendTypeLlamaCpp, 16384)
	if got != "node-donor" {
		t.Fatalf("выбран узел %q, ожидался node-donor (модель жива там; иначе preflight "+
			"планирует загрузку с нуля на пустом узле и клиент ждёт минуты)", got)
	}
}

// TestR303_SelectionFreshCacheWins — если кэш уже знает покрывающую копию, живой
// опрос не нужен (лишние запросы к узлам на горячем пути запрещены).
func TestR303_SelectionFreshCacheWins(t *testing.T) {
	var donorHits int32
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		&donorHits)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	// Кэш уже содержит донора как покрывающую копию.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["node-donor"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: liveProbeModelR303, ContextLength: 32768, ContextPerSeq: 32768}},
	}
	p.metricsMgr.mu.Unlock()

	time.Sleep(2 * time.Millisecond)
	got := p.selectBackendPreferWindow(liveProbeModelR303, types.BackendTypeLlamaCpp, 16384)
	if got != "node-donor" {
		t.Fatalf("выбран узел %q, ожидался node-donor", got)
	}
	if n := atomic.LoadInt32(&donorHits); n != 0 {
		t.Errorf("при известной копии в кэше узел всё равно опрошен %d раз", n)
	}
}

// TestR303_StickySessionRetarget_FindsAlternative — сценарий залипшей сессии:
// узел сессии модели НЕ имеет (кэш молчит, живой опрос это подтверждает), а
// другая копия жива → переезд на неё (и привязка сессии перевешивается в
// proxy.go, R303).
func TestR303_StickySessionRetarget_FindsAlternative(t *testing.T) {
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768,"context_per_seq":32768}]}`,
		nil)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	// Узел сессии модели не имеет — это то, что проверяет proxy.go перед слотом.
	if !p.modelAbsentOnBackendR303("node-empty", liveProbeModelR303) {
		t.Fatal("modelAbsentOnBackendR303 не видит, что модели на узле сессии нет")
	}
	alt, perSeq, ok := p.findLiveLoadedCopyR303(liveProbeModelR303, 16384,
		map[string]bool{"node-empty": true})
	if !ok || alt != "node-donor" || perSeq != 32768 {
		t.Fatalf("переезд залипшей сессии: alt=%q perSeq=%d ok=%v, ожидался node-donor/32768",
			alt, perSeq, ok)
	}
}

// TestR303_ModelAbsentOnBackend_UnknownIsNotAbsent — недоступный узел НЕ считается
// узлом «без модели»: иначе переезд принимался бы по недостоверным данным.
func TestR303_ModelAbsentOnBackend_UnknownIsNotAbsent(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	resetLiveCopyProbeCacheR303()
	t.Cleanup(resetLiveCopyProbeCacheR303)
	p := &Proxy{
		backends:     map[string]*BackendState{"node-dead": backendStateR303(t, "node-dead", deadURL)},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
		queueMgr:     NewQueueManager(nil, 10, 1, 30),
		config:       &types.LoadBalancerConfig{},
		statePath:    t.TempDir() + "/state.json",
	}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)

	if p.modelAbsentOnBackendR303("node-dead", liveProbeModelR303) {
		t.Fatal("недоступный узел объявлен узлом «без модели» — переезд по недостоверным данным")
	}
}

// TestR303_ModelAbsentOnBackend_LoadedIsNotAbsent — модель на узле загружена,
// переезжать некуда.
func TestR303_ModelAbsentOnBackend_LoadedIsNotAbsent(t *testing.T) {
	donor := fakeCppWorkerR303(t,
		`{"count":1,"models":[{"name":"`+liveProbeModelR303+`","state":"loaded","context_size":32768}]}`,
		nil)
	empty := fakeCppWorkerR303(t, `{"count":0,"models":[]}`, nil)
	p := newLiveProbeProxyR303(t, empty.URL, donor.URL)

	if p.modelAbsentOnBackendR303("node-donor", liveProbeModelR303) {
		t.Fatal("узел с загруженной моделью объявлен узлом «без модели»")
	}
}
