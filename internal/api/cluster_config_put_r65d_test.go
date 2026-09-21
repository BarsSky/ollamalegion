//go:build llama_stub

// cluster_config_put_r65d_test.go — R65d (2026-09-20): регрессия на
// PUT /api/v1/cluster/config.
//
// Найденный дефект (аудит 2026-09-20): серверная структура запроса содержала
// только algorithm/modelAffinity/sessionStickiness/useEnhancedScoring/
// predictionFiltering/gpu*cpu*ram*Usage/minFreeDisk/operatingMode/initialized/
// backendEngine/llamaCpp. WebUI при этом отправлял ещё пять секций
// (modelReplication, rpcCoordinator, virtualModels, distInference) и семь полей
// llamaCpp (flashAttnType, splitMode, idleUnloadMinutes, enableMetrics,
// metricsRetentionSeconds, enableReasoning, reasoningBudget), которых НЕ БЫЛО
// ни в структуре запроса, ни в types.LlamaCppConfig.
//
// json.Decoder молча игнорирует неизвестные ключи, поэтому PUT отвечал
// {"success":true}, а verifySync перечитывал старые значения и откатывал форму —
// оператор видел тост «Settings saved» при неизменившейся настройке.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// putClusterConfig выполняет PUT /api/v1/cluster/config с заданным телом.
func putClusterConfig(t *testing.T, s *Server, body interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/v1/cluster/config = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v (%s)", err, rec.Body.String())
	}
	return resp
}

// TestR65d_ClusterConfigPut_AppliesModelReplication — секция modelReplication
// раньше игнорировалась целиком.
func TestR65d_ClusterConfigPut_AppliesModelReplication(t *testing.T) {
	s := authEnabledServer(t)

	resp := putClusterConfig(t, s, map[string]interface{}{
		"modelReplication": map[string]interface{}{
			"enabled":             true,
			"defaultMinInstances": 4,
			"defaultMaxInstances": 7,
			"idleUnloadAfter":     "42m",
		},
	})

	// Должно быть в списке применённых полей.
	updated, _ := resp["updated"].([]interface{})
	found := false
	for _, u := range updated {
		if u == "modelReplication" {
			found = true
		}
	}
	if !found {
		t.Errorf("modelReplication отсутствует в resp.updated: %v "+
			"(до R65d секция молча игнорировалась)", updated)
	}

	mr := s.config.Balancing.ModelReplication
	if !mr.Enabled || mr.DefaultMinInstances != 4 || mr.DefaultMaxInstances != 7 || mr.IdleUnloadAfter != "42m" {
		t.Errorf("modelReplication не применён: %+v", mr)
	}
}

// TestR65d_ClusterConfigPut_AppliesRpcCoordinator — секция rpcCoordinator.
func TestR65d_ClusterConfigPut_AppliesRpcCoordinator(t *testing.T) {
	s := authEnabledServer(t)

	putClusterConfig(t, s, map[string]interface{}{
		"rpcCoordinator": map[string]interface{}{
			"enabled":        true,
			"coordinatorURL": "http://coordinator.example:18050",
			"workerPort":     18051,
			"protocol":       "grpc",
			"timeout":        "45s",
		},
	})

	rc := s.config.Balancing.RpcCoordinator
	if !rc.Enabled {
		t.Error("rpcCoordinator.enabled не применён")
	}
	if rc.CoordinatorURL != "http://coordinator.example:18050" {
		t.Errorf("rpcCoordinator.coordinatorURL = %q", rc.CoordinatorURL)
	}
	if rc.WorkerPort != 18051 {
		t.Errorf("rpcCoordinator.workerPort = %d, want 18051", rc.WorkerPort)
	}
	if rc.Protocol != "grpc" {
		t.Errorf("rpcCoordinator.protocol = %q, want grpc", rc.Protocol)
	}
}

// TestR65d_ClusterConfigPut_AppliesVirtualModelsAndDistInference.
func TestR65d_ClusterConfigPut_AppliesVirtualModelsAndDistInference(t *testing.T) {
	s := authEnabledServer(t)

	putClusterConfig(t, s, map[string]interface{}{
		"virtualModels": map[string]interface{}{
			"enabled":   true,
			"coordMode": "parallel",
			"timeout":   12345,
		},
		"distInference": map[string]interface{}{
			"enabled":  true,
			"grpcPort": 19123,
		},
	})

	vm := s.config.Balancing.VirtualModels
	if !vm.Enabled || vm.CoordMode != "parallel" || vm.Timeout != 12345 {
		t.Errorf("virtualModels не применён: %+v", vm)
	}
	di := s.config.Balancing.DistInference
	if !di.Enabled || di.GrpcPort != 19123 {
		t.Errorf("distInference не применён: %+v", di)
	}
}

// TestR65d_ClusterConfigPut_PreservesNestedCollections — groups/workers/models
// управляются отдельными endpoint'ами и НЕ должны затираться формой настроек.
func TestR65d_ClusterConfigPut_PreservesNestedCollections(t *testing.T) {
	s := authEnabledServer(t)

	// Предзаполняем вложенные коллекции, как если бы они были созданы через
	// /api/v1/replication/*, /api/v1/rpc/workers, /api/v1/virtual-models.
	s.config.Balancing.RpcCoordinator.Workers = []string{"http://worker-1:18050"}

	putClusterConfig(t, s, map[string]interface{}{
		"rpcCoordinator": map[string]interface{}{
			"enabled":        true,
			"coordinatorURL": "http://coordinator.example:18050",
		},
		"modelReplication": map[string]interface{}{"enabled": true},
	})

	if len(s.config.Balancing.RpcCoordinator.Workers) != 1 ||
		s.config.Balancing.RpcCoordinator.Workers[0] != "http://worker-1:18050" {
		t.Errorf("PUT затёр rpcCoordinator.workers: %v "+
			"(они управляются через /api/v1/rpc/workers)", s.config.Balancing.RpcCoordinator.Workers)
	}
}

// TestR65d_ClusterConfigPut_AppliesPreviouslyIgnoredLlamaCppFields —
// семь полей llamaCpp, которых не было в types.LlamaCppConfig.
func TestR65d_ClusterConfigPut_AppliesPreviouslyIgnoredLlamaCppFields(t *testing.T) {
	s := authEnabledServer(t)

	putClusterConfig(t, s, map[string]interface{}{
		"llamaCpp": map[string]interface{}{
			"contextLength":           8192,
			"numGpuLayers":            12,
			"flashAttnType":           1,
			"splitMode":               1,
			"idleUnloadMinutes":       17,
			"enableMetrics":           false,
			"metricsRetentionSeconds": 1234,
			"enableReasoning":         true,
			"reasoningBudget":         2048,
		},
	})

	lc := s.config.LlamaCpp
	if lc.ContextLength != 8192 {
		t.Errorf("contextLength = %d, want 8192", lc.ContextLength)
	}
	if lc.FlashAttnType != 1 {
		t.Errorf("flashAttnType = %d, want 1 (поле отсутствовало в LlamaCppConfig до R65d)", lc.FlashAttnType)
	}
	if lc.SplitMode != 1 {
		t.Errorf("splitMode = %d, want 1 (поле отсутствовало до R65d)", lc.SplitMode)
	}
	if lc.IdleUnloadMinutes != 17 {
		t.Errorf("idleUnloadMinutes = %d, want 17 (поле отсутствовало до R65d)", lc.IdleUnloadMinutes)
	}
	if lc.EnableMetrics {
		t.Error("enableMetrics = true, want false (явное выключение через UI не сохранялось)")
	}
	if lc.MetricsRetentionSeconds != 1234 {
		t.Errorf("metricsRetentionSeconds = %d, want 1234", lc.MetricsRetentionSeconds)
	}
	if !lc.EnableReasoning {
		t.Error("enableReasoning = false, want true")
	}
	if lc.ReasoningBudget != 2048 {
		t.Errorf("reasoningBudget = %d, want 2048", lc.ReasoningBudget)
	}
}

// TestR65d_ClusterConfigPut_AutoValuesZeroAreHonored — flashAttnType=0 («off»)
// и splitMode=0 («none») — валидные значения, а не «не задано». Раньше логика
// вида `if src.X != 0` их бы отбросила.
func TestR65d_ClusterConfigPut_AutoValuesZeroAreHonored(t *testing.T) {
	s := authEnabledServer(t)

	// Сначала ставим ненулевые, чтобы убедиться, что 0 действительно применяется.
	putClusterConfig(t, s, map[string]interface{}{
		"llamaCpp": map[string]interface{}{"flashAttnType": 1, "splitMode": 2},
	})
	putClusterConfig(t, s, map[string]interface{}{
		"llamaCpp": map[string]interface{}{"flashAttnType": 0, "splitMode": 0},
	})

	if s.config.LlamaCpp.FlashAttnType != 0 {
		t.Errorf("flashAttnType = %d, want 0 (значение 0 = 'off' должно применяться)",
			s.config.LlamaCpp.FlashAttnType)
	}
	if s.config.LlamaCpp.SplitMode != 0 {
		t.Errorf("splitMode = %d, want 0 (значение 0 = 'none' должно применяться)",
			s.config.LlamaCpp.SplitMode)
	}
}

// TestR65d_ClusterConfigPut_AgentSectionRejected — секция agent не
// поддерживается: применить тайминги чужого процесса через этот endpoint
// невозможно, поэтому сервер не должен делать вид, что применил их.
func TestR65d_ClusterConfigPut_AgentSectionRejected(t *testing.T) {
	s := authEnabledServer(t)

	resp := putClusterConfig(t, s, map[string]interface{}{
		"agent": map[string]interface{}{
			"collectInterval":   99,
			"heartbeatInterval": 98,
		},
	})

	updated, _ := resp["updated"].([]interface{})
	for _, u := range updated {
		if u == "agent" {
			t.Errorf("сервер сообщил о применении секции agent, хотя это " +
				"конфигурация отдельного процесса cmd/agent")
		}
	}
}

// TestR65d_ClusterConfigPut_GetReturnsAllSections — GET должен возвращать те же
// секции, которые принимает PUT, иначе verifySync на клиенте всегда видит
// расхождение.
func TestR65d_ClusterConfigPut_GetReturnsAllSections(t *testing.T) {
	s := authEnabledServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/config", nil)
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, section := range []string{"modelReplication", "rpcCoordinator", "virtualModels", "distInference", "llamaCpp"} {
		if _, ok := cfg[section]; !ok {
			t.Errorf("GET /api/v1/cluster/config не возвращает секцию %q — "+
				"клиентский verifySync не сможет подтвердить применение", section)
		}
	}
}

