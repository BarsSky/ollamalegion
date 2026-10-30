//go:build llama_stub

// profile_nctx_ceiling_r319_test.go — R319 (2026-10-30): потолок окна виден
// ДАЖЕ БЕЗ ПРОФИЛЯ у модели.
//
// ЖИВАЯ ЖАЛОБА (повторяется): «обучение 131072, VRAM 131072, почему окно
// 32768?». Потолок авто-роста (LB_NCTX_RELOAD_MAX_N_CTX) не отдавался ни в одном
// ответе API, если у модели не было профиля: карта effectiveContext строится
// только по существующим профилям, а /api/v1/cluster/config такого поля не имеет
// вовсе. Оператор искал причину в интерфейсе и не находил.
//
// Теперь у обоих ответов (список и одиночный профиль) есть nctxCeiling —
// кластерное значение с причиной, независимое от профилей.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR319_ProfileListExposesClusterCeiling — потолок в списке профилей.
func TestR319_ProfileListExposesClusterCeiling(t *testing.T) {
	s := newAgentCapacityServer(t)
	coord := s.proxy.GetNCtxReloadCoordinator()
	cfg := coord.Config()
	cfg.AutoReloadMaxNCtx = 32768
	coord.SetConfig(cfg)

	rec := httptest.NewRecorder()
	s.handleListModelProfiles(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-profiles", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleListModelProfiles: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		NCtxCeiling struct {
			EnvCeiling int    `json:"envCeiling"`
			LimitedBy  string `json:"limitedBy"`
			Reason     string `json:"reason"`
		} `json:"nctxCeiling"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if list.NCtxCeiling.EnvCeiling != 32768 {
		t.Errorf("nctxCeiling.envCeiling = %d, want 32768: %s", list.NCtxCeiling.EnvCeiling, rec.Body.String())
	}
	if list.NCtxCeiling.LimitedBy != "env" {
		t.Errorf("nctxCeiling.limitedBy = %q, want env", list.NCtxCeiling.LimitedBy)
	}
	if !strings.Contains(list.NCtxCeiling.Reason, "LB_NCTX_RELOAD_MAX_N_CTX=32768") {
		t.Errorf("причина не называет переменную и число: %q", list.NCtxCeiling.Reason)
	}
}

// TestR319_ProfileListCeilingWithoutProfiles — ГЛАВНЫЙ случай R319: у модели
// профиля нет, но потолок всё равно обязан быть в ответе.
func TestR319_ProfileListCeilingWithoutProfiles(t *testing.T) {
	s := newAgentCapacityServer(t)
	coord := s.proxy.GetNCtxReloadCoordinator()
	cfg := coord.Config()
	cfg.AutoReloadMaxNCtx = 65536
	coord.SetConfig(cfg)

	rec := httptest.NewRecorder()
	s.handleListModelProfiles(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-profiles", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleListModelProfiles: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Total       int                        `json:"total"`
		NCtxCeiling map[string]json.RawMessage `json:"nctxCeiling"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if list.Total != 0 {
		t.Fatalf("ожидался пустой реестр профилей, total=%d", list.Total)
	}
	raw, ok := list.NCtxCeiling["envCeiling"]
	if !ok {
		t.Fatalf("nctxCeiling.envCeiling отсутствует при пустом реестре профилей: %s", rec.Body.String())
	}
	if string(raw) != "65536" {
		t.Errorf("nctxCeiling.envCeiling = %s, want 65536", string(raw))
	}
}

// TestR319_SingleProfileExposesClusterCeiling — одиночный профиль несёт тот же
// блок (карточка читает именно его).
func TestR319_SingleProfileExposesClusterCeiling(t *testing.T) {
	s := newAgentCapacityServer(t)
	s.proxy.SetModelProfile("tiny-model", types.LlamaCppModelProfile{ContextLength: 8192})
	coord := s.proxy.GetNCtxReloadCoordinator()
	cfg := coord.Config()
	cfg.AutoReloadMaxNCtx = 32768
	coord.SetConfig(cfg)

	rec := httptest.NewRecorder()
	s.getModelProfile(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-profiles/tiny-model", nil), "tiny-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("getModelProfile: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var single struct {
		NCtxCeiling struct {
			EnvCeiling int    `json:"envCeiling"`
			Reason     string `json:"reason"`
		} `json:"nctxCeiling"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if single.NCtxCeiling.EnvCeiling != 32768 {
		t.Errorf("nctxCeiling.envCeiling = %d, want 32768: %s", single.NCtxCeiling.EnvCeiling, rec.Body.String())
	}
	if single.NCtxCeiling.Reason == "" {
		t.Error("nctxCeiling.reason пуст — карточка не сможет объяснить ограничение")
	}
}
