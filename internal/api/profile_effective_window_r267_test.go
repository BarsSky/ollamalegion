//go:build llama_stub

// profile_effective_window_r267_test.go — R267 (2026-10-29): WebUI должен
// ВИДЕТЬ причину ограничения окна, а не только своё значение.
//
// ЖИВАЯ ЖАЛОБА. Оператор выставил в карточке модели окно 256000, а модель
// загрузилась с 32768 — и в интерфейсе не было ни одного места, где сказано
// «почему». Теперь GET model-profiles/{name} и список профилей отдают
// вычисленные поля: effectiveContextLength / limitedBy / limitReason, посчитанные
// ТОЙ ЖЕ формулой, что и решение о перезагрузке (R267).
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR267_ProfileResponseExposesEffectiveWindow — ответ API называет причину.
func TestR267_ProfileResponseExposesEffectiveWindow(t *testing.T) {
	s := newAgentCapacityServer(t)
	model := "qwen3.8-27b"
	s.proxy.SetModelProfile(model, types.LlamaCppModelProfile{ContextLength: 256000})
	// env-потолок авто-роста ниже заявленного окна — типовой случай со стенда.
	coord := s.proxy.GetNCtxReloadCoordinator()
	cfg := coord.Config()
	cfg.AutoReloadMaxNCtx = 32768
	coord.SetConfig(cfg)

	// (1) Одиночный профиль.
	rec := httptest.NewRecorder()
	s.getModelProfile(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-profiles/"+model, nil), model)
	if rec.Code != http.StatusOK {
		t.Fatalf("getModelProfile: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var single struct {
		Model     string `json:"model"`
		Effective struct {
			EffectiveContextLength int    `json:"effectiveContextLength"`
			LimitReason            string `json:"limitReason"`
			EnvCeiling             int    `json:"envCeiling"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if single.Effective.EffectiveContextLength != 256000 {
		t.Errorf("effectiveContextLength = %d, want 256000 (заявленное окно не режется env-потолком)",
			single.Effective.EffectiveContextLength)
	}
	if single.Effective.EnvCeiling != 32768 {
		t.Errorf("envCeiling = %d, want 32768 (оператор должен видеть оба числа)", single.Effective.EnvCeiling)
	}
	if single.Effective.LimitReason == "" {
		t.Errorf("в ответе нет limitReason — оператор снова не увидит причину: %s", rec.Body.String())
	}

	// (2) Список профилей — та же информация рядом с профилями.
	rec2 := httptest.NewRecorder()
	s.handleListModelProfiles(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-profiles", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("handleListModelProfiles: HTTP %d (%s)", rec2.Code, rec2.Body.String())
	}
	var list struct {
		Models    map[string]types.LlamaCppModelProfile `json:"models"`
		Effective map[string]struct {
			EffectiveContextLength int    `json:"effectiveContextLength"`
			LimitedBy              string `json:"limitedBy"`
			LimitReason            string `json:"limitReason"`
			EnvCeiling             int    `json:"envCeiling"`
		} `json:"effectiveContext"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &list); err != nil {
		t.Fatalf("список не JSON: %v", err)
	}
	if _, ok := list.Models[model]; !ok {
		t.Fatalf("профиль %q пропал из списка: %s", model, rec2.Body.String())
	}
	eff, ok := list.Effective[model]
	if !ok {
		t.Fatalf("в ответе нет блока effectiveContext для %q: %s", model, rec2.Body.String())
	}
	if eff.EffectiveContextLength != 256000 {
		t.Errorf("effectiveContextLength = %d, want 256000", eff.EffectiveContextLength)
	}
	if eff.EnvCeiling != 32768 {
		t.Errorf("envCeiling = %d, want 32768 (оператор должен видеть оба числа)", eff.EnvCeiling)
	}
	if eff.LimitReason == "" {
		t.Error("limitReason пуст — причина ограничения не объяснена")
	}
}

// TestR267_ProfileResponseNamesEnvLimitWhenDeclaredLower — обратный случай:
// заявленное окно НИЖЕ env-потолка, режет env и это названо.
func TestR267_ProfileResponseNamesEnvLimitWhenDeclaredLower(t *testing.T) {
	s := newAgentCapacityServer(t)
	model := "small-model"
	s.proxy.SetModelProfile(model, types.LlamaCppModelProfile{ContextLength: 8192})
	coord := s.proxy.GetNCtxReloadCoordinator()
	cfg := coord.Config()
	cfg.AutoReloadMaxNCtx = 131072
	coord.SetConfig(cfg)

	effective, limitedBy, reason, env := s.proxy.ProfileEffectiveWindow(model)
	if effective != 131072 {
		t.Errorf("effective = %d, want 131072 (env-потолок выше заявленного)", effective)
	}
	if limitedBy != "env" {
		t.Errorf("limitedBy = %q, want env", limitedBy)
	}
	if env != 131072 {
		t.Errorf("envCeiling = %d, want 131072", env)
	}
	if !strings.Contains(reason, "LB_NCTX_RELOAD_MAX_N_CTX") {
		t.Errorf("причина не называет переменную: %s", reason)
	}
}
