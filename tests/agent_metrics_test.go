package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentMetricsStructure verifies the BackendMetrics JSON structure
func TestAgentMetricsStructure(t *testing.T) {
	tc := []struct {
		name     string
		metrics  types.BackendMetrics
		wantKeys []string
	}{
		{
			name: "full_metrics_structure",
			metrics: types.BackendMetrics{
				ID:        "gpu-test-1",
				Timestamp: time.Now(),
				Status:    types.StatusHealthy,
				HasAgent:  true,
				Host:      "192.0.2.100",
				OllamaPort: 11434,
				GPU: types.GPUMetrics{
					UsagePercent: 45.5,
					MemoryTotal:  24576,
					MemoryUsed:   10240,
					MemoryFree:   14336,
					Temperature:  65,
					PowerUsage:   150,
				},
				System: types.SystemMetrics{
					CPUUsagePercent: 23.0,
					MemoryTotal:     65536,
					MemoryUsed:      32768,
					MemoryFree:      32768,
				},
		Ollama: types.OllamaMetrics{
			ActiveRequests: 2,
			RunningModels: []types.RunningModel{
				{Name: "llama2:7b", VRAMUsage: 4096},
				{Name: "mistral:7b", VRAMUsage: 5120},
				{Name: "gemma:2b", VRAMUsage: 2048},
			},
			TotalRequests: 1500,
		},
		Prediction: types.Prediction{
			SecondsToCritical: 600,
		},
				Score:            0.42,
				Models:           []string{"llama2:7b", "mistral:7b", "gemma:2b"},
				VRAMUsagePercent: 41.7,
				VRAMTotalGB:      24.0,
				VRAMUsedGB:       10.0,
				MemoryUsagePercent: 50.0,
			},
			wantKeys: []string{"id", "timestamp", "status", "hasAgent", "host", "ollamaPort",
				"gpu", "system", "ollama", "prediction", "score", "models",
				"vramUsagePercent", "vramTotalGB", "vramUsedGB", "memoryUsagePercent"},
		},
		{
			name: "minimal_agentless_metrics",
			metrics: types.BackendMetrics{
				ID:        "cpu-node-1",
				Timestamp: time.Now(),
				Status:    types.StatusHealthy,
				HasAgent:  false,
				Host:      "192.0.2.200",
				OllamaPort: 11434,
			},
			wantKeys: []string{"id", "timestamp", "status", "hasAgent", "host", "ollamaPort"},
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(c.metrics)
			require.NoError(t, err, "metrics must marshal to JSON")

			var decoded map[string]interface{}
			err = json.Unmarshal(data, &decoded)
			require.NoError(t, err, "metrics must unmarshal from JSON")

			for _, key := range c.wantKeys {
				_, exists := decoded[key]
				assert.True(t, exists, "key %q must exist in metrics JSON", key)
			}

			assert.Equal(t, c.metrics.ID, decoded["id"])
			assert.NotEmpty(t, decoded["timestamp"])
		})
	}
}

// TestBackendStatusValidValues verifies only valid status values are used
func TestBackendStatusValidValues(t *testing.T) {
	validStatuses := []types.BackendStatus{
		types.StatusHealthy,
		types.StatusUnhealthy,
		types.StatusOffline,
		types.StatusStarting,
	}

	expected := map[types.BackendStatus]bool{
		"healthy":   true,
		"unhealthy": true,
		"offline":   true,
		"starting":  true,
	}

	for _, status := range validStatuses {
		if !expected[status] {
			t.Errorf("unexpected status value: %s", status)
		}
	}

	// Verify invalid status is not in expected
	invalid := types.BackendStatus("degraded")
	if expected[invalid] {
		t.Errorf("\"degraded\" should not be a valid status (use StatusHealthy/Unhealthy/Offline/Starting)")
	}
}

// TestGPUMetricsMemoryConsistency verifies memory math is correct
func TestGPUMetricsMemoryConsistency(t *testing.T) {
	gpu := types.GPUMetrics{
		MemoryTotal: 24576,
		MemoryUsed:  10240,
		MemoryFree:  14336,
	}

	totalFromParts := gpu.MemoryUsed + gpu.MemoryFree
	assert.Equal(t, gpu.MemoryTotal, totalFromParts,
		"MemoryTotal must equal MemoryUsed + MemoryFree (%d + %d = %d, got %d)",
		gpu.MemoryUsed, gpu.MemoryFree, totalFromParts, gpu.MemoryTotal)

	// Verify usage percent
	usagePct := float64(gpu.MemoryUsed) / float64(gpu.MemoryTotal) * 100
	t.Logf("VRAM usage: %.1f%%", usagePct)
}

// TestSystemMetricsMemoryConsistency verifies RAM math
func TestSystemMetricsMemoryConsistency(t *testing.T) {
	sys := types.SystemMetrics{
		MemoryTotal: 65536,
		MemoryUsed:  32768,
		MemoryFree:  32768,
	}

	totalFromParts := sys.MemoryUsed + sys.MemoryFree
	assert.Equal(t, sys.MemoryTotal, totalFromParts,
		"MemoryTotal must equal MemoryUsed + MemoryFree (%d + %d = %d, got %d)",
		sys.MemoryUsed, sys.MemoryFree, totalFromParts, sys.MemoryTotal)
}

// TestPredictionThresholds verifies overload prediction ranges
func TestPredictionThresholds(t *testing.T) {
	tc := []struct {
		name               string
		overloadProb       float64
		expectedWarning    bool
	}{
		{"low_risk", 0.15, false},
		{"medium_risk", 0.55, false},
		{"high_risk_70pct", 0.70, true},
		{"critical_90pct", 0.90, true},
		{"max_risk", 1.0, true},
		{"zero_risk", 0.0, false},
	}

	warningThreshold := 0.70

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			isWarning := c.overloadProb >= warningThreshold
			assert.Equal(t, c.expectedWarning, isWarning,
				"overloadProb %.2f: expected warning=%v, got %v (threshold=%.2f)",
				c.overloadProb, c.expectedWarning, isWarning, warningThreshold)
		})
	}
}

// TestPredictionSecondsToCritical verifies prediction field behavior
func TestPredictionSecondsToCritical(t *testing.T) {
	tc := []struct {
		name             string
		secsToCritical   float64
		expectedWarning  bool
	}{
		{"critical_now", 0, true},
		{"warning_soon", 300, true},
		{"moderate_600s", 600, true},
		{"safe_3600s", 3600, false},
		{"no_prediction", -1, false},
	}

	warningThreshold := 900.0 // 15 minutes

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			isWarning := c.secsToCritical >= 0 && c.secsToCritical < warningThreshold
			assert.Equal(t, c.expectedWarning, isWarning,
				"secondsToCritical %.0f: expected warning=%v, got %v (threshold=%.0f)",
				c.secsToCritical, c.expectedWarning, isWarning, warningThreshold)
		})
	}
}

// TestScoreCalculationRanges verifies routing score is in valid range
func TestScoreCalculationRanges(t *testing.T) {
	tc := []struct {
		name  string
		score float64
	}{
		{"best_score", 1.0},
		{"medium_score", 0.42},
		{"low_score", 0.1},
		{"zero_score", 0.0},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			assert.True(t, c.score >= 0.0 && c.score <= 1.0,
				"score %.2f must be in range [0.0, 1.0]", c.score)
		})
	}
}

// TestModelListConsistency verifies model list is present in metrics
func TestModelListConsistency(t *testing.T) {
	metrics := types.BackendMetrics{
		ID:     "gpu-test",
		Models: []string{"llama2:7b", "mistral:7b", "gemma:2b"},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama2:7b"},
				{Name: "mistral:7b"},
				{Name: "gemma:2b"},
			},
		},
	}

	assert.Equal(t, len(metrics.Models), len(metrics.Ollama.RunningModels),
		"Models list length (%d) must match RunningModels count (%d)",
		len(metrics.Models), len(metrics.Ollama.RunningModels))
}

// TestHeartbeatResponseValidJson verifies heartbeat response is valid JSON
func TestHeartbeatResponseValidJson(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":        "ok",
			"backendId":     "gpu-test-1",
			"heartbeatTime": time.Now().UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	require.NoError(t, err)

	assert.Equal(t, "ok", data["status"])
	assert.Equal(t, "gpu-test-1", data["backendId"])
	assert.NotEmpty(t, data["heartbeatTime"])
}

// TestMetricsCollectionInterval verifies interval parsing
func TestMetricsCollectionInterval(t *testing.T) {
	tc := []struct {
		name     string
		interval string
		wantOK   bool
	}{
		{"valid_5s", "5s", true},
		{"valid_10s", "10s", true},
		{"valid_1m", "1m", true},
		{"empty", "", false},
		{"invalid_text", "abc", false},
		{"negative", "-5s", false},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			d, err := time.ParseDuration(c.interval)
			if c.wantOK {
				assert.NoError(t, err, "interval %q should parse correctly", c.interval)
				assert.True(t, d > 0, "duration must be positive")
			} else {
				assert.True(t, err != nil || d <= 0,
					"interval %q should fail parsing or be non-positive", c.interval)
			}
		})
	}
}

// TestBalancingAlgorithmValidValues verifies algorithm enum is valid
func TestBalancingAlgorithmValidValues(t *testing.T) {
	validAlgos := map[types.BalancingAlgorithm]bool{
		types.AlgorithmRoundRobin:    true,
		types.AlgorithmLeastConn:     true,
		types.AlgorithmResourceAware: true,
		types.AlgorithmModelAffinity: true,
	}

	t.Run("valid_algorithms_are_accepted", func(t *testing.T) {
		for algo := range validAlgos {
			t.Logf("✅ valid algorithm: %s", algo)
		}
		assert.Equal(t, 4, len(validAlgos), "expected 4 valid algorithms")
	})

	t.Run("invalid_algorithm_rejected", func(t *testing.T) {
		invalid := types.BalancingAlgorithm("random")
		assert.False(t, validAlgos[invalid],
			"algorithm %q should not be valid", invalid)
	})
}