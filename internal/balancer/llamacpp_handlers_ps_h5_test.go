//go:build llama_stub

// llamacpp_handlers_ps_h5_test.go — H5 аудита client-api-fidelity.
//
// Остаток пункта H5: /api/ps для llama.cpp-бэкенда считает expires_at как
// LoadedAt + 30m без зажима к «сейчас». cppworker держит модель по
// LastUsedAt+keep_alive, а не по LoadedAt, поэтому модель, живущая дольше
// 30 минут (keep_alive=1h/24h), получает expires_at в прошлом — клиент
// (OpenWebUI, `ollama ps`, ollama-python) снова видит «уже истекла».
//
// Модель, которая лежит в LlamaCppMetrics.LoadedModels, загружена прямо
// сейчас, значит её expires_at не может быть в прошлом.
package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

type h5PSResponse struct {
	Models []struct {
		Name      string    `json:"name"`
		Digest    string    `json:"digest"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"models"`
}

func callHandlePS(t *testing.T, proxy *Proxy) h5PSResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/ps", nil)
	w := httptest.NewRecorder()
	proxy.llamaCppRouter.handlePS(w, req)
	if w.Code != 200 {
		t.Fatalf("handlePS: статус %d, body=%s", w.Code, w.Body.String())
	}
	var resp h5PSResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("handlePS: bad JSON: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

// TestH5_HandlePS_OldLoadedAt_ExpiresAtNotInPast — модель загружена 2 часа
// назад и всё ещё в LoadedModels: expires_at обязан быть в будущем.
//
// До фикса: expiresAt = (now-2h) + 30m = now-90m → в прошлом, тест падает.
func TestH5_HandlePS_OldLoadedAt_ExpiresAtNotInPast(t *testing.T) {
	loadedAt := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	proxy, _ := buildProxyWithStub(t, nil, []types.LlamaCppModel{
		{
			Name:     "gemma-4-E4B-it-Q4_K_M",
			Path:     "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
			Size:     16_464_440_224,
			State:    "loaded",
			LoadedAt: loadedAt,
		},
	})

	resp := callHandlePS(t, proxy)
	if len(resp.Models) != 1 {
		t.Fatalf("handlePS: ожидали 1 загруженную модель, получили %d: %+v", len(resp.Models), resp.Models)
	}
	m := resp.Models[0]

	if !m.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %s (LoadedAt=%s) — в прошлом, хотя модель в LoadedModels (H5, зажим к now)",
			m.ExpiresAt.Format(time.RFC3339), loadedAt)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(m.Digest) {
		t.Errorf("digest = %q, want sha256:<64 hex>", m.Digest)
	}
}

// TestH5_HandlePS_FreshLoadedAt_ExpiresAtKeepsLoadedAtWindow — контроль:
// для свежей загрузки поведение не меняется (LoadedAt + 30m).
func TestH5_HandlePS_FreshLoadedAt_ExpiresAtKeepsLoadedAtWindow(t *testing.T) {
	loadedAt := time.Now().Add(-1 * time.Minute).UTC()
	proxy, _ := buildProxyWithStub(t, nil, []types.LlamaCppModel{
		{
			Name:     "model-fresh",
			Path:     "/app/models/model-fresh.gguf",
			Size:     1_000_000_000,
			State:    "loaded",
			LoadedAt: loadedAt.Format(time.RFC3339),
		},
	})

	resp := callHandlePS(t, proxy)
	if len(resp.Models) != 1 {
		t.Fatalf("handlePS: ожидали 1 модель, получили %d", len(resp.Models))
	}
	want := loadedAt.Add(defaultPSExpiresWindow)
	if d := resp.Models[0].ExpiresAt.Sub(want); d > time.Second || d < -time.Second {
		t.Errorf("expires_at = %s, want ≈ %s (LoadedAt+%s)",
			resp.Models[0].ExpiresAt.Format(time.RFC3339), want.Format(time.RFC3339), defaultPSExpiresWindow)
	}
}
