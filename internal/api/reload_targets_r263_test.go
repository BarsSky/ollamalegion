package api

// reload_targets_r263_test.go — R263 (2026-10-28): загрузка модели не уходит на
// мёртвый узел, а отказ объяснён.
//
// ЖИВОЙ ФАКТ (стенд r262). `POST /api/v1/cluster/models/{model}/reload
// {operation:"load"}` без backendId выбрал `cppworker-cyborg-bundled-agent`
// (offline/unhealthy, сеть недоступна) и «ушёл в никуда»: оператор ждал
// несколько минут и получал ошибку из другого места. С явным backendId работало.
//
// ЧТО ФИКСИРУЕМ:
//  1. непригодный узел не попадает в цели, если есть пригодный;
//  2. узел, исключённый оператором (R250 `Disabled`), не попадает в цели;
//  3. если пригодных нет — 503 с причинами по каждому кандидату (а не «нет
//     бэкендов» и не тихая попытка);
//  4. явный backendId на непригодном узле — БЫСТРЫЙ отказ с причиной (раньше
//     операция уходила в сеть и висела до таймаута);
//  5. явный backendId на пригодном узле работает как раньше (семантика 200 с
//     per-backend результатами не менялась).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReloadTargetsServer — сервер с заданным составом llama_cpp-бэкендов.
//
// Состояние узла задаётся прямо в конфиге: cluster state собирается из записей
// прокси, поэтому статус/disabled доезжают до выбора целей без сети.
func newReloadTargetsServer(t *testing.T, backends []types.Backend) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 8080, APIPort: 8081,
			StatePath: t.TempDir() + "/state.json",
		},
		Backends: backends,
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

// reloadBackend — запись llama_cpp-узла для теста.
func reloadBackend(id, host string, port int, status types.BackendStatus, disabled bool) types.Backend {
	return types.Backend{
		ID: id, Name: id, Host: host, CppWorkerPort: port,
		Type: types.BackendTypeLlamaCpp, Weight: 1, MaxConcurrentReqs: 1,
		Status: status, Disabled: disabled,
	}
}

// doReload — POST reload и разобранный ответ (код + тело).
func doReload(t *testing.T, s *Server, model, body string) (int, map[string]interface{}, time.Duration) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/"+model+"/reload",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	s.clusterReloadModelHandler(rec, req)
	elapsed := time.Since(start)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "ответ не JSON: %s", rec.Body.String())
	return rec.Code, resp, elapsed
}

// TestR263_OfflineBackendNotSelectedWhenHealthyExists — главный живой случай.
func TestR263_OfflineBackendNotSelectedWhenHealthyExists(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{
		// Порт 1 заведомо закрыт: проверяем ВЫБОР цели (живой узел взят, мёртвый
		// пропущен), а не сетевую доставку — иначе тест ждёт dial-таймауты.
		reloadBackend("node-healthy", "127.0.0.1", 1, types.StatusHealthy, false),
		reloadBackend("cppworker-cyborg-bundled-agent", "192.168.13.34", 18092, types.StatusOffline, false),
	})

	sel := s.selectReloadTargetsChecked("")
	assert.Equal(t, []string{"node-healthy"}, sel.Targets, "мёртвый узел не должен попадать в цели")
	require.Len(t, sel.Skipped, 1, "пропущенный узел обязан быть в отчёте")
	assert.Equal(t, "cppworker-cyborg-bundled-agent", sel.Skipped[0].BackendID)
	assert.Contains(t, sel.Skipped[0].Reason, "offline")
	assert.Equal(t, "192.168.13.34:18092", sel.Skipped[0].Address)

	// В ответе операция идёт только на живой узел, а пропуск виден в `skipped`.
	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load"}`)
	assert.Equal(t, http.StatusOK, code)
	results, _ := resp["results"].([]interface{})
	require.Len(t, results, 1)
	first, _ := results[0].(map[string]interface{})
	assert.Equal(t, "node-healthy", first["backendId"])
	skipped, _ := resp["skipped"].([]interface{})
	require.Len(t, skipped, 1, "оператор должен видеть, кого не тронули")
	sk, _ := skipped[0].(map[string]interface{})
	assert.Equal(t, "cppworker-cyborg-bundled-agent", sk["backendId"])
}

// TestR263_NoUsableBackends — все узлы недоступны: 503 с причиной по каждому.
func TestR263_NoUsableBackends(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{
		reloadBackend("node-offline", "10.0.0.2", 18092, types.StatusOffline, false),
		reloadBackend("node-unhealthy", "10.0.0.3", 18092, types.StatusUnhealthy, false),
		reloadBackend("node-starting", "10.0.0.4", 18092, types.StatusStarting, false),
	})

	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load"}`)
	assert.Equal(t, http.StatusServiceUnavailable, code,
		"узлы есть, но непригодны — это 503 с причинами, а не 404 «нет бэкендов»")
	assert.Equal(t, "no usable llama_cpp backend", resp["error"])
	skipped, _ := resp["skipped"].([]interface{})
	require.Len(t, skipped, 3, "причина обязана быть по каждому кандидату")
}

// TestR263_ExplicitBackendFastFailure — явный backendId на недоступном узле:
// быстрый отказ с причиной вместо многоминутного ожидания.
func TestR263_ExplicitBackendFastFailure(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{
		reloadBackend("node-healthy", "10.0.0.1", 18092, types.StatusHealthy, false),
		reloadBackend("node-offline", "10.0.0.9", 18092, types.StatusOffline, false),
	})

	code, resp, elapsed := doReload(t, s, "gemma-4", `{"operation":"load","backendId":"node-offline"}`)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, resp["message"], "offline")
	assert.Equal(t, "node-offline", resp["backendId"])
	assert.Equal(t, "10.0.0.9:18092", resp["address"])
	// Ключевое: никакого сетевого ожидания — отказ по состоянию узла.
	assert.Less(t, elapsed, 3*time.Second, "отказ по недоступному узлу должен быть мгновенным")
}

// TestR263_DisabledBackendNotSelected — R250: «не использовать бэкенд» исключает
// узел и из операций загрузки, а не только из выбора балансером.
func TestR263_DisabledBackendNotSelected(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{
		reloadBackend("node-healthy", "10.0.0.1", 18092, types.StatusHealthy, false),
		reloadBackend("node-disabled", "10.0.0.5", 18092, types.StatusHealthy, true),
	})

	sel := s.selectReloadTargetsChecked("")
	assert.Equal(t, []string{"node-healthy"}, sel.Targets)
	require.Len(t, sel.Skipped, 1)
	assert.Contains(t, sel.Skipped[0].Reason, "excluded by operator")

	// Явно указанный выключенный узел — тоже отказ, с причиной.
	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load","backendId":"node-disabled"}`)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, resp["message"], "excluded by operator")
}

// TestR263_ExplicitHealthyStillWorks — семантика успешного ответа не изменилась.
func TestR263_ExplicitHealthyStillWorks(t *testing.T) {
	// Порт 1 заведомо закрыт: нас интересует КОД ответа и то, что узел ВЗЯТ в цели
	// (per-backend ошибка соединения — отдельная история R262).
	s := newReloadTargetsServer(t, []types.Backend{
		reloadBackend("node-healthy", "127.0.0.1", 1, types.StatusHealthy, false),
	})

	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load","backendId":"node-healthy"}`)
	assert.Equal(t, http.StatusOK, code, "агрегированный ответ остаётся 200")
	results, _ := resp["results"].([]interface{})
	require.Len(t, results, 1)
	first, _ := results[0].(map[string]interface{})
	assert.Equal(t, "node-healthy", first["backendId"])
}

// TestR263_ExplicitUnknownBackendIs404 — «такого узла нет» по-прежнему 404.
func TestR263_ExplicitUnknownBackendIs404(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{
		reloadBackend("node-healthy", "10.0.0.1", 18092, types.StatusHealthy, false),
	})
	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load","backendId":"nope"}`)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, resp["message"], "not found")
}

// TestR263_NoLlamaCppBackendsStill404 — прежняя семантика для пустого кластера.
func TestR263_NoLlamaCppBackendsStill404(t *testing.T) {
	s := newReloadTargetsServer(t, []types.Backend{{
		ID: "ollama-1", Host: "10.0.0.7", OllamaPort: 11434,
		Type: types.BackendTypeOllama, Status: types.StatusHealthy,
	}})
	code, resp, _ := doReload(t, s, "gemma-4", `{"operation":"load"}`)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "no llama_cpp backends available", resp["error"])
}
