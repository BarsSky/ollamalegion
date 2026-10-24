//go:build llama_stub

// request_param_precedence_r122_test.go — R122 (пункт 7 разбора стенда).
//
// ЧТО ПРОВЕРЯЕТСЯ. Формулировка пункта: «если модель не загружена, то при запросе
// в приоритете параметры, что пришли с запросом пользователя; если в запросе нет
// особых параметров — грузить дефолт».
//
// Порядок источников (num_ctx_resolver.go): запрос → профиль модели → дефолт
// бэкенда. Тесты фиксируют его для ХОЛОДНОЙ модели (ничего не загружено) и то,
// что выигравшее значение действительно уезжает в запрос автозагрузки — то есть
// клиент влияет на то, С КАКИМИ параметрами поднимется модель.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// coldProxyWithProfile — прокси, где модель НЕ загружена, а у модели задан профиль.
func coldProxyWithProfile(t *testing.T, ctxLen int) *Proxy {
	t.Helper()
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma": {ContextLength: ctxLen},
	}
	return p
}

// TestR122_Cold_RequestNumCtxWinsOverProfile — в запросе есть num_ctx: выигрывает
// ЗАПРОС, а не профиль оператора.
func TestR122_Cold_RequestNumCtxWinsOverProfile(t *testing.T) {
	p := coldProxyWithProfile(t, 8192)

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma","num_ctx":4096}`), "llama_adm")
	if res.Value != 4096 {
		t.Errorf("ResolveNumCtx = %d, want 4096: при незагруженной модели параметр из "+
			"запроса обязан выигрывать у профиля (%d)", res.Value, 8192)
	}
	if res.Source != NumCtxSourceRequest {
		t.Errorf("source = %q, want %q: значение пришло из запроса", res.Source, NumCtxSourceRequest)
	}
}

// TestR122_Cold_OllamaOptionsFormIsHonoured — то же для Ollama-формы
// (options.num_ctx): клиенты Ollama/OpenWebUI шлют именно её.
func TestR122_Cold_OllamaOptionsFormIsHonoured(t *testing.T) {
	p := coldProxyWithProfile(t, 8192)

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma","options":{"num_ctx":2048}}`), "llama_adm")
	if res.Value != 2048 || res.Source != NumCtxSourceRequest {
		t.Errorf("ResolveNumCtx = (%d, %q), want (2048, %q)", res.Value, res.Source, NumCtxSourceRequest)
	}
}

// TestR122_Cold_NoRequestParams_UsesProfile — в запросе параметров нет: грузится
// с профилем модели (дефолт оператора), а не с чем-то, выдуманным балансером.
func TestR122_Cold_NoRequestParams_UsesProfile(t *testing.T) {
	p := coldProxyWithProfile(t, 8192)

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma"}`), "llama_adm")
	if res.Value != 8192 {
		t.Errorf("ResolveNumCtx = %d, want 8192 (профиль модели — дефолт без запроса)", res.Value)
	}
	if res.Source != NumCtxSourceProfile {
		t.Errorf("source = %q, want %q", res.Source, NumCtxSourceProfile)
	}
}

// TestR122_Cold_NoRequestNoProfile_NoInventedValue — ни запроса, ни профиля:
// балансер НЕ выдумывает значение (0 = «не задано»), его подставит cppworker со
// своим дефолтом. Иначе автозагрузка поднимала бы модель с чужим окном.
func TestR122_Cold_NoRequestNoProfile_NoInventedValue(t *testing.T) {
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = nil

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma"}`), "llama_adm")
	if res.Source == NumCtxSourceRequest {
		t.Errorf("source = %q при пустом запросе: заявку из воздуха делать нельзя", res.Source)
	}
}

// TestR122_WinningValueReachesAutoLoadRequest — выигравшее значение обязано
// доехать до запроса автозагрузки ОКНОМ НА КЛИЕНТА (contextPerSeq): cppworker
// сам умножит его на число слотов.
func TestR122_WinningValueReachesAutoLoadRequest(t *testing.T) {
	p := coldProxyWithProfile(t, 8192)

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma","num_ctx":4096}`), "llama_adm")
	load := buildAutoLoadRequest("gemma", nil, &res.Value, nil)

	if load.ContextPerSeq == nil || *load.ContextPerSeq != 4096 {
		t.Fatalf("ContextPerSeq = %v, want 4096 (окно из запроса)", load.ContextPerSeq)
	}
	if load.ContextSize != nil {
		t.Errorf("ContextSize = %v, want nil: суммарное окно считает cppworker", *load.ContextSize)
	}
}
