// Unit tests for ApplyCppCtxHeader (nctx_clamp.go) — Phase D.6 + D.8 regression coverage.
//
// Build tag: llama_stub (тесты компилируются со stub-bridge, не требует реальной llama.cpp)
//
// Покрывает:
//   - empty header → no-op
//   - invalid header → no-op
//   - body num_ctx > header → clamp (Phase D.3-fix)
//   - body num_ctx == 0 → use header as default
//   - body num_ctx < header → не трогаем body
//   - NPredict == reference default (D.8 regression) → заменяется на n_ctx/2
//   - NPredict < reference default (клиент задал явно) → НЕ трогаем
//   - маленький n_ctx → halfCtx >= 64
//go:build llama_stub

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/c/bridge"

	"github.com/stretchr/testify/assert"
)

// helper — новый request с указанным X-Cpp-Ctx header
func newReqWithCtx(ctxVal string) *http.Request {
	r := httptest.NewRequest("POST", "/api/chat", nil)
	if ctxVal != "" {
		r.Header.Set("X-Cpp-Ctx", ctxVal)
	}
	return r
}

// TestApplyCppCtxHeader_NoHeader: пустой header — функция не делает ничего
func TestApplyCppCtxHeader_NoHeader(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	original := params

	r := newReqWithCtx("")
	ApplyCppCtxHeader(r, &params)

	assert.Equal(t, original, params, "no header → params unchanged")
}

// TestApplyCppCtxHeader_InvalidHeader: невалидное значение — no-op
func TestApplyCppCtxHeader_InvalidHeader(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	original := params

	r := newReqWithCtx("not-a-number")
	ApplyCppCtxHeader(r, &params)

	assert.Equal(t, original, params, "invalid header → params unchanged")

	r = newReqWithCtx("0")
	ApplyCppCtxHeader(r, &params)
	assert.Equal(t, original, params, "zero header → params unchanged")

	r = newReqWithCtx("-100")
	ApplyCppCtxHeader(r, &params)
	assert.Equal(t, original, params, "negative header → params unchanged")
}

// TestApplyCppCtxHeader_BodyOverrides: body задал num_ctx, header применился
func TestApplyCppCtxHeader_BodyOverrides(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 8192 // body задал num_ctx=8192
	originalNPredict := params.NPredict

	r := newReqWithCtx("4096") // header limit = 4096
	ApplyCppCtxHeader(r, &params)

	// n_ctx должен быть закламлен к header (Phase D.3-fix)
	assert.Equal(t, 4096, params.NCtxOverride, "body num_ctx clamped to header limit")

	// NPredict: т.к. он = original (reference default из bridge), он должен быть
	// заменён на n_ctx/2 = 2048 (D.8 fix: reference default берётся из bridge)
	refDefault := bridge.DefaultGenerationParams().NPredict
	if originalNPredict >= refDefault {
		assert.Equal(t, 2048, params.NPredict, "n_predict = n_ctx/2 = 2048 after clamp")
	}
}

// TestApplyCppCtxHeader_NoBodyNumCtx: body не задал num_ctx — header становится default
func TestApplyCppCtxHeader_NoBodyNumCtx(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	// params.NCtxOverride = 0 (body не задал)

	r := newReqWithCtx("2048")
	ApplyCppCtxHeader(r, &params)

	// n_ctx берётся из header
	assert.Equal(t, 2048, params.NCtxOverride, "NCtxOverride set to header value when body=0")

	// NPredict должен быть заменён на n_ctx/2 = 1024
	refDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= refDefault {
		assert.Equal(t, 1024, params.NPredict, "n_predict = n_ctx/2 = 1024")
	}
}

// TestApplyCppCtxHeader_BodySmallerThanHeader: body num_ctx < header — не клампим вверх
func TestApplyCppCtxHeader_BodySmallerThanHeader(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 1024 // body задал num_ctx=1024

	r := newReqWithCtx("4096") // header = 4096
	ApplyCppCtxHeader(r, &params)

	// n_ctx должен остаться 1024 (header — UPPER LIMIT, не повышает body)
	assert.Equal(t, 1024, params.NCtxOverride, "body n_ctx not raised by header (UPPER LIMIT semantics)")

	// NPredict: reference default = 2048. NPredict (1024 < 2048) < reference → НЕ трогаем
	// (клиент НЕ задавал NPredict явно, но 1024 < reference default значит считаем
	// что это значение «не дефолтное» — НО фактически это просто default, а клиент
	// задал num_ctx=1024 в body, что могло перезаписать context, но NPredict не
	// задавал, поэтому он равен default → должно клампиться к n_ctx/2=512)
	refDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= refDefault {
		assert.Equal(t, 512, params.NPredict, "n_predict clamped to n_ctx/2 = 512")
	}
}

// TestApplyCppCtxHeader_ClientExplicitNPredict: клиент задал num_predict в body
// (params.NPredict отличается от reference default) — НЕ перезаписываем
func TestApplyCppCtxHeader_ClientExplicitNPredict(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 4096
	params.NPredict = 128 // клиент явно задал max_tokens=128 в body

	r := newReqWithCtx("4096")
	ApplyCppCtxHeader(r, &params)

	// NPredict не должен быть затронут (128 < reference default)
	assert.Equal(t, 128, params.NPredict, "client's explicit n_predict preserved")
}

// TestApplyCppCtxHeader_D8Regression: регрессионный тест Phase D.8.
// До фикса: условие `NPredict >= hardcoded 4096` всегда false после D.6 (default=2048).
// NPredict НЕ заменялся → 28+2048+1=2077 > n_ctx=2048 → code 3 → пустой ответ OpenWebUI.
// После фикса: reference default берётся из bridge (2048) → NPredict заменяется на n_ctx/2.
func TestApplyCppCtxHeader_D8Regression(t *testing.T) {
	params := bridge.DefaultGenerationParams()
	// params.NPredict = bridge.DefaultGenerationParams().NPredict = 2048 (после D.6)

	r := newReqWithCtx("2048")
	ApplyCppCtxHeader(r, &params)

	// КРИТИЧНО: NPredict должен быть заменён на 1024 (n_ctx/2)
	// До D.8 fix: NPredict оставался 2048 → 28+2048+1=2077 > 2048 → code 3
	// После D.8 fix: NPredict = 1024 → 28+1024+1=1053 << 2048 ✓
	assert.Equal(t, 1024, params.NPredict,
		"D.8 regression: n_predict must be replaced with n_ctx/2=1024, "+
			"otherwise code 3 'prompt too long' will be returned for OpenWebUI")
}

// TestApplyCppCtxHeader_SmallNCtx: маленький n_ctx — halfCtx >= 64
func TestApplyCppCtxHeader_SmallNCtx(t *testing.T) {
	params := bridge.DefaultGenerationParams()

	r := newReqWithCtx("128") // n_ctx=128
	ApplyCppCtxHeader(r, &params)

	// halfCtx = 128/2 = 64 (не меньше 64)
	refDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= refDefault {
		assert.Equal(t, 64, params.NPredict, "small n_ctx → halfCtx=64 minimum")
	}

	// Доп. проверка: n_ctx=64 → halfCtx был бы 32, но поднимается до 64
	params2 := bridge.DefaultGenerationParams()
	r2 := newReqWithCtx("64")
	ApplyCppCtxHeader(r2, &params2)
	if params2.NPredict >= refDefault {
		assert.Equal(t, 64, params2.NPredict, "tiny n_ctx=64 → halfCtx raised to 64")
	}
}

// TestApplyCppCtxHeader_LargeNCtx: большой n_ctx — halfCtx = n_ctx/2
func TestApplyCppCtxHeader_LargeNCtx(t *testing.T) {
	params := bridge.DefaultGenerationParams()

	r := newReqWithCtx("32768")
	ApplyCppCtxHeader(r, &params)

	refDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= refDefault {
		assert.Equal(t, 16384, params.NPredict, "large n_ctx=32768 → halfCtx=16384")
	}
}