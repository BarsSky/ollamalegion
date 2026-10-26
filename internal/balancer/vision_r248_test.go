// vision_r248_test.go — R248, часть B1: признак vision у модели на балансере.
//
// Правило: изображения пропускаются ТОЛЬКО модели, которая объявила
// capabilities.vision; иначе — 415. Угадывать по имени модели нельзя (gemma-4
// бывает и текстовой, и vision-сборкой), иначе вернётся 200 с «не видел».
package balancer

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestR248_VisionDecision — чистая логика решения passthrough/415.
func TestR248_VisionDecision(t *testing.T) {
	cases := []struct {
		name    string
		capable bool
		images  int
		reject  bool
	}{
		{"текст без картинок", false, 0, false},
		{"текстовая модель + картинка", false, 1, true},
		{"vision-модель + картинка", true, 1, false},
		{"vision-модель без картинок", true, 0, false},
		{"две картинки, текстовая", false, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := visionShouldReject(tc.capable, tc.images); got != tc.reject {
				t.Errorf("visionShouldReject(%v,%d) = %v, ожидалось %v", tc.capable, tc.images, got, tc.reject)
			}
		})
	}
}

// TestR248_FetchParsesVisionCapability — capabilities.vision из /v1/models
// воркера доезжает до тега модели.
func TestR248_FetchParsesVisionCapability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"vlm-model","object":"model","capabilities":{"vision":true}},
			{"id":"text-model","object":"model","capabilities":{"vision":false}}
		]}`))
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(srv.URL[len("http://"):])
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	lr := &LlamaCppRouter{}
	tags, err := lr.fetchLlamaCppModels(host, port)
	if err != nil {
		t.Fatalf("fetchLlamaCppModels: %v", err)
	}
	got := map[string]bool{}
	for _, tag := range tags {
		got[tag.Name] = tag.Vision
	}
	if !got["vlm-model"] {
		t.Errorf("vision-модель не помечена: %+v", tags)
	}
	if got["text-model"] {
		t.Errorf("текстовая модель помечена как vision: %+v", tags)
	}
}

// TestR248_VisionCacheDefaultFalse — без опроса кэш «не знаем» = false
// (лучше честный 415, чем 200 с невидимой картинкой).
func TestR248_VisionCacheDefaultFalse(t *testing.T) {
	resetVisionCapabilityCache()
	lr := &LlamaCppRouter{}
	if lr.modelVisionCapable("gemma-4") {
		t.Error("по умолчанию модель не должна считаться vision-capable")
	}
}
