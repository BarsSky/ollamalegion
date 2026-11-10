// stream_flag_r484_test.go — R484: один разбор поля stream на все пути.
//
// До R484 предикатов было три с разными дефолтами для запроса БЕЗ stream, и
// один и тот же /api/chat без поля считался стримовым при выборе очереди/окна,
// но мог уехать в non-stream транспорт llama.cpp, а в лог писаться stream=true.
// Теперь разбор один (streamFlagFromBodyR484), а различия в дефолте передаются
// явно и проверяются здесь.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestR484_StreamFlagParsing — явное значение, дефолт и битое тело.
func TestR484_StreamFlagParsing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		def     bool
		want    bool
		wantExp bool
		wantOK  bool
	}{
		{"explicit true", `{"stream":true}`, false, true, true, true},
		{"explicit false", `{"stream":false}`, true, false, true, true},
		{"absent -> default true", `{"model":"m"}`, true, true, false, true},
		{"absent -> default false", `{"model":"m"}`, false, false, false, true},
		{"empty body", ``, true, false, false, false},
		{"invalid json", `{`, true, false, false, false},
		{"stream not bool", `{"stream":"true"}`, true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, exp, ok := streamFlagFromBodyR484([]byte(tc.body), tc.def)
			if got != tc.want || exp != tc.wantExp || ok != tc.wantOK {
				t.Fatalf("streamFlagFromBodyR484(%q,%v) = (%v,%v,%v), ожидалось (%v,%v,%v)",
					tc.body, tc.def, got, exp, ok, tc.want, tc.wantExp, tc.wantOK)
			}
		})
	}
}

// TestR484_SurfaceDefaults — legacy стримит по умолчанию, OpenAI-поверхность нет.
func TestR484_SurfaceDefaults(t *testing.T) {
	if !streamDefaultForSurfaceR484(surfaceLegacy) {
		t.Fatal("legacy-поверхность должна по умолчанию стримить (прежнее поведение)")
	}
	if streamDefaultForSurfaceR484(surfaceOpenAI) {
		t.Fatal("OpenAI-поверхность не должна стримить без явного stream:true")
	}
}

// TestR484_IsStreamingFromBodyKeepsExplicitOnly — транспорт llama.cpp уходит в
// стрим только по явному stream:true (поведение сохранено).
func TestR484_IsStreamingFromBodyKeepsExplicitOnly(t *testing.T) {
	if isStreamingFromBody("/api/chat", []byte(`{"model":"m"}`)) {
		t.Fatal("без поля stream non-stream транспорт llama.cpp — прежнее поведение")
	}
	if !isStreamingFromBody("/api/chat", []byte(`{"stream":true}`)) {
		t.Fatal("явный stream:true должен включать потоковый транспорт")
	}
	if isStreamingFromBody("/api/chat", []byte(`{"stream":false}`)) {
		t.Fatal("явный stream:false не должен включать потоковый транспорт")
	}
	if isStreamingFromBody("/api/chat", nil) {
		t.Fatal("пустое тело — не стрим")
	}
}

// TestR484_IsStreamingRequestFallbackDefaultTrue — исторический дефолт этого
// вызова (true) сохранён.
func TestR484_IsStreamingRequestFallbackDefaultTrue(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(`{"model":"m"}`)))
	if !p.isStreamingRequest(req) {
		t.Fatal("без поля stream isStreamingRequest по-прежнему возвращает true")
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(`{"stream":false}`)))
	if p.isStreamingRequest(req2) {
		t.Fatal("явный stream:false не должен считаться стримом")
	}
}
