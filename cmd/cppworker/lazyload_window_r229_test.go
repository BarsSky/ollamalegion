//go:build llama_stub

// lazyload_window_r229_test.go — R229 (2026-10-22): окно ленивой загрузки
// подчиняется заявленной лестнице приоритета:
//
//	явный num_ctx клиента  >  профиль модели  >  env CPPWORKER_CTX_SIZE
//
// Живой дефект (замер стенда, образ gpu-r227-stall): ленивая загрузка
// gemma-4-E4B-it-Q4_K_M шла с load_n_ctx=32768 и n_ctx_source=env, хотя профиль
// модели требует 12288. Причина — env-дефолт попадал в план ДО слияния с
// профилем, а слияние заполняет только пустые поля.
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR229_Window_ProfileBeatsEnvDefault(t *testing.T) {
	prof := &types.LlamaCppModelProfile{ContextLength: 12288}
	got, src := chooseLazyLoadWindow(0, 32768, 1, prof)
	if got != 12288 {
		t.Errorf("n_ctx = %d, want 12288 (профиль модели); 32768 — env-дефолт", got)
	}
	if src != "профиль модели" {
		t.Errorf("source = %q, want %q", src, "профиль модели")
	}
}

func TestR229_Window_ClientBeatsProfile(t *testing.T) {
	prof := &types.LlamaCppModelProfile{ContextLength: 12288}
	got, src := chooseLazyLoadWindow(32768, 8192, 1, prof)
	if got != 32768 {
		t.Errorf("n_ctx = %d, want 32768 (явный num_ctx клиента)", got)
	}
	if src == "профиль модели" {
		t.Errorf("источник окна %q: профиль не должен перебивать явный запрос клиента", src)
	}
}

func TestR229_Window_ClientWindowIsPerSlot(t *testing.T) {
	// Клиент просит 32768 НА КЛИЕНТА при parallel=2 → модели нужно 65536.
	got, src := chooseLazyLoadWindow(32768, 8192, 2, nil)
	if got != 65536 {
		t.Errorf("n_ctx = %d, want 65536 (32768 на клиента × 2 слота)", got)
	}
	if src == "" {
		t.Error("источник окна пуст")
	}
}

func TestR229_Window_EnvWhenNoProfile(t *testing.T) {
	got, src := chooseLazyLoadWindow(0, 32768, 1, nil)
	if got != 32768 || src != "env CPPWORKER_CTX_SIZE" {
		t.Errorf("n_ctx=%d source=%q, want 32768/env (профиля нет)", got, src)
	}
}

func TestR229_Window_EnvWhenProfileHasNoCtx(t *testing.T) {
	// Профиль без contextLength (0 = наследовать) не должен обнулять окно.
	prof := &types.LlamaCppModelProfile{ContextLength: 0}
	got, src := chooseLazyLoadWindow(0, 32768, 1, prof)
	if got != 32768 || src != "env CPPWORKER_CTX_SIZE" {
		t.Errorf("n_ctx=%d source=%q, want 32768/env (в профиле нет окна)", got, src)
	}
}
