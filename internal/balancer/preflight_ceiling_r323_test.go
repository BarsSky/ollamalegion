// preflight_ceiling_r323_test.go — R323 (2026-10-30): ОДИН потолок окна для
// решателя и для preflight.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (OpenWebUI): «не смог удовлетвориться контекстным окном
// 131072 при запросе на 15128 символов, загруженная модель имела 131072».
//
// ПРИЧИНА. R255/R267 ввели правило: действующий потолок авто-роста =
// max(env LB_NCTX_RELOAD_MAX_N_CTX, заявленное в профиле модели окно). Правило
// получил РЕШАТЕЛЬ перезагрузки (nctx_reload.go), но не preflight:
//   - resolvePhysicalMaxContext (preflight_helper.go) брал min(профиль, gguf),
//     то есть env=32768 опускал предел ниже заявленных профилем 65536/131072;
//   - DecidePreflight сравнивал с СЫРЫМ cfg.AutoReloadMaxNCtx и отдавал 413
//     «auto_reload_ceiling_reason» на окно, которое профиль разрешает.
//
// Это ровно класс дефекта R267 («правка одного слоя не лечила другой»), поэтому
// тесты фиксируют оба места: формулу физического потолка и решение preflight.
package balancer

import (
	"strings"
	"testing"
)

// TestR323_PhysicalCeilingTakesMaxOfEnvAndDeclared — env не опускает предел ниже
// заявленного окна.
func TestR323_PhysicalCeilingTakesMaxOfEnvAndDeclared(t *testing.T) {
	cases := []struct {
		name           string
		gguf           int
		declared       int
		env            int
		want           int
		why            string
	}{
		{
			name: "профиль 65536 при env 32768 — побеждает профиль",
			gguf: 131072, declared: 65536, env: 32768, want: 65536,
			why: "правило max(env, declared): заявленное окно оператора выше env",
		},
		{
			name: "профиль 131072 при env 32768 — побеждает профиль",
			gguf: 131072, declared: 131072, env: 32768, want: 131072,
			why: "живая жалоба: модель и профиль держат 131072, env не должен резать",
		},
		{
			name: "профиль 16384 при env 32768 — побеждает env",
			gguf: 131072, declared: 16384, env: 32768, want: 32768,
			why: "env разрешает авто-рост до 32768, профиль ниже — действует env",
		},
		{
			name: "профиля нет — действует env",
			gguf: 131072, declared: 0, env: 32768, want: 32768,
			why: "без профиля единственный cap — env",
		},
		{
			name: "gguf ниже обоих — побеждает gguf",
			gguf: 8192, declared: 131072, env: 32768, want: 8192,
			why: "обучающий контекст модели — жёсткий верх",
		},
		{
			name: "ничего не задано — 0 (неизвестно)",
			gguf: 0, declared: 0, env: 0, want: 0,
			why: "0 = неизвестно, preflight падает на ModelMaxContext",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePhysicalMaxContext(tc.gguf, tc.declared, tc.env); got != tc.want {
				t.Errorf("resolvePhysicalMaxContext(%d, %d, %d) = %d, want %d (%s)",
					tc.gguf, tc.declared, tc.env, got, tc.want, tc.why)
			}
		})
	}
}

// TestR323_PreflightUsesEffectiveCeilingNotRawEnv — окно выше env, но не выше
// заявленного профилем, не должно отвергаться.
func TestR323_PreflightUsesEffectiveCeilingNotRawEnv(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:          "cppworker-gpu-bundled-agent",
		CurrentNCtx:        32768, // загружено суммарно
		CurrentContextPerSeq: 32768, // окно слота
		CurrentSlots:         1,
		MaxVRAMNCtx:        0,
		ModelMaxContext:    131072,
		PhysicalMaxContext: 131072,
		GGUFMaxContext:     131072,
		AutoReloadMaxNCtx:  32768,
	}
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		RequestedNCtxOverride: 65536,
		// Ровно то, что положил runInferencePreflight: max(env 32768, профиль 65536).
		EffectiveCeiling: 65536,
	}
	cfg := NCtxReloadConfig{AutoReloadNCtx: true, AutoReloadMaxNCtx: 32768}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightReject {
		t.Fatalf("окно 65536 отвергнуто при заявленных профилем 65536: %q (env-потолок 32768 не должен резать заявленное окно)", res.RejectBody)
	}
	if res.Decision != PreflightReload {
		t.Fatalf("decision = %v, want reload (нужно поднять окно до 65536)", res.Decision)
	}
	if res.TargetNCtx != 65536 {
		t.Errorf("TargetNCtx = %d, want 65536 (окно на клиента)", res.TargetNCtx)
	}
}

// TestR323_PreflightStillRejectsAboveEffectiveCeiling — за пределами max(env,
// declared) отказ остаётся, и текст называет действующее число, а не сырое env.
func TestR323_PreflightStillRejectsAboveEffectiveCeiling(t *testing.T) {
	state := &NCtxBackendState{
		BackendID: "cppworker-gpu-bundled-agent", CurrentNCtx: 32768,
		CurrentContextPerSeq: 32768, CurrentSlots: 1,
		// Физика заведомо шире проверяемого потолка: проверяем именно
		// операторский предел (max(env, declared) = 65536), а не GGUF.
		ModelMaxContext: 262144, PhysicalMaxContext: 262144, GGUFMaxContext: 262144,
		AutoReloadMaxNCtx: 32768,
	}
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		RequestedNCtxOverride: 200000,
		EffectiveCeiling:      65536,
	}
	cfg := NCtxReloadConfig{AutoReloadNCtx: true, AutoReloadMaxNCtx: 32768}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("200000 при действующем потолке 65536 обязан быть отказом, got %v", res.Decision)
	}
	// Текст отказа обязан называть ДЕЙСТВУЮЩИЙ потолок (max(env, declared)), а не
	// сырое env-значение: иначе оператор поднимает env, ничего не меняется и он
	// снова ищет причину.
	if !strings.Contains(res.RejectBody, "65536") {
		t.Errorf("в отказе нет действующего потолка 65536: %s", res.RejectBody)
	}
	if !strings.Contains(res.RejectBody, `"max_per_client_n_ctx":65536`) {
		t.Errorf("в отказе max_per_client_n_ctx не равен действующему потолку: %s", res.RejectBody)
	}
	if !strings.Contains(res.RejectBody, `"backend_ceiling_n_ctx":65536`) {
		t.Errorf("в отказе backend_ceiling_n_ctx не равен действующему потолку: %s", res.RejectBody)
	}
}
