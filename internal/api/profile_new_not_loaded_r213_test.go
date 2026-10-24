//go:build llama_stub

// profile_new_not_loaded_r213_test.go — R213 (2026-10-21).
//
// Живой дефект (WebUI GGUF → «применить и перезагрузить»): для модели, у которой
// профиля ещё нет и которая НЕ загружена, PUT /api/v1/cppworker/model-profiles/{name}
// отвечал 400 «contextLength is required for a new profile». Кнопка «применить»
// сохраняла НОЛЬ параметров, и со стороны оператора это выглядело как «WebUI
// игнорирует выставленные параметры».
//
// Теперь окно берётся из умолчаний (default-профиль балансера, затем env
// cppworker), значение записывается в профиль и одновременно называется в ответе
// (contextLengthSource), чтобы UI не выдавал его за выбор оператора.
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestUpsertNewProfileNotLoaded_UsesDefaultCtx_R213(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, srv := createProxyTestServer(t, mock.URL())
	defer server.Close()

	// default-профиль балансера: именно он применяется к модели без своего профиля.
	srv.proxy.SetDefaultModelProfile(types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     512,
	})

	// Форма WebUI GGUF шлёт частичный профиль (только параметры, которые
	// оператор реально выставил) — contextLength здесь нет.
	body := `{"kvCacheType":"q4_0","parallel":2}`
	req, err := http.NewRequest(http.MethodPut,
		server.URL+"/api/v1/cppworker/model-profiles/Some-New-Model.gguf",
		bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (модель не загружена — это не ошибка сохранения): %s",
			resp.StatusCode, string(raw))
	}

	var out struct {
		Status              string                     `json:"status"`
		Profile             types.LlamaCppModelProfile `json:"profile"`
		ContextLengthSource string                     `json:"contextLengthSource"`
		Message             string                     `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, string(raw))
	}

	if out.Profile.ContextLength != 65536 {
		t.Errorf("profile.contextLength = %d, want 65536 (из default-профиля)",
			out.Profile.ContextLength)
	}
	if out.ContextLengthSource != "defaultProfile" {
		t.Errorf("contextLengthSource = %q, want defaultProfile (UI должен назвать источник)", out.ContextLengthSource)
	}
	if out.Profile.KVCacheType != "q4_0" || out.Profile.Parallel != 2 {
		t.Errorf("параметры оператора не сохранились: kv=%q parallel=%d",
			out.Profile.KVCacheType, out.Profile.Parallel)
	}
	if out.Message == "" {
		t.Error("нет сообщения о том, что contextLength взят из умолчаний")
	}
}

// Контроль: явный contextLength из запроса НЕ перетирается умолчаниями.
func TestUpsertNewProfile_ExplicitCtxWins_R213(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, srv := createProxyTestServer(t, mock.URL())
	defer server.Close()

	srv.proxy.SetDefaultModelProfile(types.LlamaCppModelProfile{ContextLength: 65536})

	body := `{"contextLength":8192,"kvCacheType":"q8_0"}`
	req, err := http.NewRequest(http.MethodPut,
		server.URL+"/api/v1/cppworker/model-profiles/Other-New-Model",
		bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		Profile             types.LlamaCppModelProfile `json:"profile"`
		ContextLengthSource string                     `json:"contextLengthSource"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Profile.ContextLength != 8192 {
		t.Errorf("contextLength = %d, want 8192 (явное значение оператора)", out.Profile.ContextLength)
	}
	if out.ContextLengthSource != "" {
		t.Errorf("contextLengthSource = %q, want empty (значение задал оператор)", out.ContextLengthSource)
	}
}
