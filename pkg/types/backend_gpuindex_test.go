package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// R-Image follow-up (2026-10-02): индекс GPU бэкенда.
//
// Поле нужно политике сосуществования image/text: ключ лока GPU = host +
// gpuIndex, когда индекс задан у ОБЕИХ сторон, иначе — хост (консервативно).
// Здесь фиксируем резолвер, от которого зависит ключ.

func TestBackend_EffectiveGPUIndex(t *testing.T) {
	cases := []struct {
		name    string
		backend *Backend
		want    int
	}{
		{"nil", nil, 0},
		{"не задан", &Backend{}, 0},
		{"явный индекс", &Backend{GPUIndex: 3}, 3},
		{
			"фолбэк на CppWorkerConfig.MainGPU",
			&Backend{CppWorkerConfig: &LlamaCppConfig{MainGPU: 2}},
			2,
		},
		{
			"явный индекс важнее MainGPU",
			&Backend{GPUIndex: 1, CppWorkerConfig: &LlamaCppConfig{MainGPU: 2}},
			1,
		},
		{
			// 0 = «неизвестно» в ОБОИХ источниках: GPU 0 этим полем объявить
			// нельзя (нужен отдельный флаг), поэтому лок остаётся хостовым.
			"MainGPU=0 не считается индексом",
			&Backend{CppWorkerConfig: &LlamaCppConfig{MainGPU: 0}},
			0,
		},
	}
	for _, c := range cases {
		if got := c.backend.EffectiveGPUIndex(); got != c.want {
			t.Errorf("%s: EffectiveGPUIndex() = %d, want %d", c.name, got, c.want)
		}
	}
}

// JSON-контракт поля: omitempty (0 не засоряет state.json/API) и имя gpuIndex.
func TestBackend_GPUIndexJSONTags(t *testing.T) {
	raw, err := json.Marshal(Backend{ID: "b1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "gpuIndex") {
		t.Errorf("gpuIndex=0 обязан отсутствовать в JSON (omitempty): %s", raw)
	}
	raw, err = json.Marshal(Backend{ID: "b1", GPUIndex: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"gpuIndex":1`) {
		t.Errorf("gpuIndex=1 обязан сериализоваться как gpuIndex: %s", raw)
	}
}
