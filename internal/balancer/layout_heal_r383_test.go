package balancer

// layout_heal_r383_test.go — R383 (2026-11-01): автолечение вырожденной раскладки.
//
// Проверяем: правило решения (когда лечить), троттлинг (без вечного цикла
// unload/load), поиск узла по адресу раскладки и сам путь «выгрузить → загрузить»
// с токеном воркера.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR383_HealDecision — правило «лечить ли».
func TestR383_HealDecision(t *testing.T) {
	loaded := types.LlamaCppModel{Name: "m", State: "loaded", LayoutDegraded: true, Path: "/app/models/m.gguf"}

	cases := []struct {
		name          string
		model         types.LlamaCppModel
		layoutKnown   bool
		layoutEnabled bool
		servers       int
		active        int
		want          bool
		wantReason    string
	}{
		{"деградация и простой — лечим", loaded, true, true, 1, 0, true, ""},
		{"раскладка в порядке — нечего лечить", types.LlamaCppModel{Name: "m", State: "loaded"}, true, true, 1, 0, false, ""},
		{"модель ещё грузится", types.LlamaCppModel{Name: "m", State: "loading", LayoutDegraded: true}, true, true, 1, 0, false, "модель не завершила загрузку"},
		{"воркер не сообщает состав", loaded, false, true, 1, 0, false, "воркер не сообщает состав раскладки"},
		{"раскладка выключена", loaded, true, false, 0, 0, false, "раскладка выключена — деградировать нечему"},
		{"нет узлов", loaded, true, true, 0, 0, false, "раскладка выключена — деградировать нечему"},
		{"модель занята — не рвём стрим", loaded, true, true, 1, 2, false, "на модели есть активные стримы"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := shouldHealLayoutR383(tc.model, tc.layoutKnown, tc.layoutEnabled, tc.servers, tc.active)
			if got != tc.want {
				t.Fatalf("shouldHeal = %v (%q), ожидалось %v", got, why, tc.want)
			}
			if tc.wantReason != "" && why != tc.wantReason {
				t.Errorf("причина = %q, ожидалась %q", why, tc.wantReason)
			}
		})
	}
}

// TestR383_HealThrottle — кулдаун и лимит попыток: без них лечение превращается
// в вечный unload/load, если узел мёртв физически.
func TestR383_HealThrottle(t *testing.T) {
	key := "test-throttle|model"
	resetLayoutHealR383(key)

	if !layoutHealAllowedR383(key) {
		t.Fatal("первая попытка обязана быть разрешена")
	}
	if layoutHealAllowedR383(key) {
		t.Fatal("вторая попытка сразу после первой обязана быть запрещена кулдауном")
	}
	if got := layoutHealAttemptsR383(key); got != 1 {
		t.Errorf("счётчик попыток = %d, ожидалось 1", got)
	}

	// Исчерпание лимита: имитируем старые попытки.
	layoutHealStateR383.mu.Lock()
	e := layoutHealStateR383.m[key]
	e.attempts = layoutHealMaxAttemptsR383
	e.lastAt = layoutHealStateR383.m[key].lastAt.Add(-2 * layoutHealCooldownR383)
	layoutHealStateR383.mu.Unlock()
	if layoutHealAllowedR383(key) {
		t.Fatal("после лимита попыток лечение обязано прекратиться")
	}

	resetLayoutHealR383(key)
	if layoutHealAttemptsR383(key) != 0 {
		t.Fatal("reset обязан обнулять счётчик")
	}
}

// TestR383_HealSwitch — выключатель LB_LAYOUT_HEAL.
func TestR383_HealSwitch(t *testing.T) {
	t.Setenv("LB_LAYOUT_HEAL", "0")
	if layoutHealEnabledR383() {
		t.Fatal("LB_LAYOUT_HEAL=0 обязан выключать автолечение")
	}
	t.Setenv("LB_LAYOUT_HEAL", "")
	if !layoutHealEnabledR383() {
		t.Fatal("по умолчанию автолечение включено — иначе его просто нет")
	}
}

// TestR383_NodeLookupByHost — узел раскладки находится по хосту из строки
// host:port, при совпадении предпочитается запись с ролью donor.
func TestR383_NodeLookupByHost(t *testing.T) {
	p := newReplicationFreeProxyR379(t, "node-a", "node-b")
	p.mu.Lock()
	p.backends["node-a"].Backend.Host = "100.64.0.7"
	p.backends["node-a"].Backend.Role = string(types.BackendRoleDonor)
	p.backends["node-b"].Backend.Host = "100.64.0.8"
	p.backends["node-b"].Backend.Role = string(types.BackendRoleMain)
	p.mu.Unlock()

	node := p.backendByNodeHostR383(nodeHostOf("100.64.0.7:50052"))
	if node == nil || node.ID != "node-a" {
		t.Fatalf("узел по хосту найден неверно: %+v", node)
	}
	if node := p.backendByNodeHostR383("100.64.0.9"); node != nil {
		t.Fatalf("неизвестный хост не должен находиться: %+v", node)
	}
}

// TestR383_ReloadPath — «выгрузить, потом загрузить», с токеном воркера.
func TestR383_ReloadPath(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var auth []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/models/unload", "/api/models/load":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host := hostPort[:strings.LastIndex(hostPort, ":")]
	port := hostPort[strings.LastIndex(hostPort, ":")+1:]

	p := newReplicationFreeProxyR379(t, "master")
	p.mu.Lock()
	p.backends["master"].Backend.Host = host
	if n, err := strconv.Atoi(port); err == nil {
		p.backends["master"].Backend.CppWorkerPort = n
	}
	p.backends["master"].Backend.CppWorkerApiToken = "tok-383"
	p.mu.Unlock()

	if err := p.reloadModelForLayoutHealR383("master", "m", "/app/models/m.gguf"); err != nil {
		t.Fatalf("перезагрузка через воркер не удалась: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Фоновый поллер прокси тоже стучится в этот сервер (/api/models,
	// /api/models/files) — нас интересуют только наши два вызова и их порядок.
	var ours []string
	var oursAuth []string
	for i, c := range calls {
		if c == "/api/models/unload" || c == "/api/models/load" {
			ours = append(ours, c)
			oursAuth = append(oursAuth, auth[i])
		}
	}
	if len(ours) != 2 || ours[0] != "/api/models/unload" || ours[1] != "/api/models/load" {
		t.Fatalf("порядок вызовов лечения = %v, ожидалось unload → load", ours)
	}
	for i, a := range oursAuth {
		if a != "Bearer tok-383" {
			t.Errorf("вызов %d без токена воркера: Authorization=%q", i, a)
		}
	}
}

// TestR383_PollerTriggersHeal — сквозной путь без живого стенда: поллер читает
// /api/models с layout_degraded, переносит факт в кэш и запускает лечение
// (unload → load на воркере). Это то, что раньше не работало вовсе: балансер
// не декодировал поля и не лечил.
func TestR383_PollerTriggersHeal(t *testing.T) {
	t.Setenv("LB_LAYOUT_HEAL", "1")
	key := "master|m"
	resetLayoutHealR383(key)
	defer resetLayoutHealR383(key)

	var mu sync.Mutex
	var unload, load int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			_, _ = w.Write([]byte(`{"rpc_enabled":true,"rpc_servers_active":["100.64.0.7:50052"],` +
				`"models":[{"name":"m","path":"/app/models/m.gguf","state":"loaded",` +
				`"layout_degraded":true,"layout_degraded_reason":"слои локально"}]}`))
		case "/api/models/unload":
			mu.Lock()
			unload++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"success":true}`))
		case "/api/models/load":
			mu.Lock()
			load++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host := hostPort[:strings.LastIndex(hostPort, ":")]
	port, err := strconv.Atoi(hostPort[strings.LastIndex(hostPort, ":")+1:])
	if err != nil {
		t.Fatalf("порт тестового сервера: %v", err)
	}

	// R383-фикс теста, ВТОРАЯ ИТЕРАЦИЯ (2026-11-12). Первая (2026-11-03) меняла
	// адрес бэкенда ПОСЛЕ NewProxy и останавливала фоновый поллер. Этого мало:
	// прокси создавался с дефолтным адресом 127.0.0.1:18092, то есть на машине с
	// поднятым стендом его первый же тик опрашивал ЖИВОЙ cppworker, получал
	// rpc_enabled=false и писал в кэш пустой снимок — иногда уже после ручного
	// опроса, перетирая результат. Живой симптом: локально 1 падение на ~50
	// прогонов под -race, на self-hosted раннере (там стенд тоже поднят) —
	// красный CI на коммите из ОДНИХ ДОКОВ.
	//
	// Теперь адрес известен ДО создания прокси: любой тик фонового поллера идёт на
	// тестовый сервер и пишет те же данные, поэтому гонка «кто последний записал»
	// больше не решает исход. Поллер всё равно останавливаем — не нужно лишних
	// запросов, — но корректность теста на этом больше не держится.
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{{
			ID:                "master",
			Name:              "test master",
			Host:              host,
			CppWorkerPort:     port,
			CppWorkerApiToken: "tok-383",
			Type:              types.BackendTypeLlamaCpp,
			Weight:            1,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 4,
		}},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 60,
			MetricsInterval:     60,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := newProxyWithCleanup(t, cfg)
	if p.llamaCppMetricsPoller != nil {
		p.llamaCppMetricsPoller.Stop()
	}

	poller := newLlamaCppMetricsPoller(p)
	if ok := poller.pollBackend(backendInfo{id: "master", host: host, port: port}); !ok {
		t.Fatal("/api/models не разобран — тест проверяет не то")
	}

	// Поля обязаны доехать до кэша метрик: на них смотрит и автолечение, и API.
	enabled, servers, known := p.cppworkerRPCLayout("master")
	if !known || !enabled || len(servers) != 1 {
		t.Fatalf("состав раскладки не попал в кэш: known=%v enabled=%v servers=%v", known, enabled, servers)
	}
	p.metricsMgr.mu.RLock()
	var degradedReported bool
	if lm := p.metricsMgr.llamaMetrics["master"]; lm != nil {
		for _, m := range lm.LoadedModels {
			if m.Name == "m" && m.LayoutDegraded {
				degradedReported = true
			}
		}
	}
	p.metricsMgr.mu.RUnlock()
	if !degradedReported {
		t.Fatal("layout_degraded не перенесён в LoadedModels — балансер снова слеп")
	}

	// Лечение асинхронное: ждём выгрузку и загрузку.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := unload > 0 && load > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if unload == 0 || load == 0 {
		t.Fatalf("автолечение не сработало: unload=%d load=%d", unload, load)
	}
}
