package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// R-Image follow-up (2026-10-02): индекс GPU бэкенда.
//
// Поле нужно политике сосуществования image/text: ключ лока GPU = host +
// индекс, когда индекс ИЗВЕСТЕН у ОБЕИХ сторон, иначе — хост (консервативно).
// Здесь фиксируем резолвер, от которого зависит ключ, и JSON-контракт поля:
// nil = «неизвестно», &0 = «явно первая карта».

func TestBackend_EffectiveGPUIndex(t *testing.T) {
	cases := []struct {
		name      string
		backend   *Backend
		want      int
		wantKnown bool
	}{
		{"nil", nil, 0, false},
		{"не задан", &Backend{}, 0, false},
		{"явный индекс", &Backend{GPUIndex: GPUIndexPtr(3)}, 3, true},
		{
			// Главное, ради чего поле стало указателем: первая карта
			// объявляется явно, и лок сужается до host#gpu0.
			"ЯВНЫЙ GPU 0 известен",
			&Backend{GPUIndex: GPUIndexPtr(0)},
			0, true,
		},
		{
			"фолбэк на CppWorkerConfig.MainGPU",
			&Backend{CppWorkerConfig: &LlamaCppConfig{MainGPU: 2}},
			2, true,
		},
		{
			"явный индекс важнее MainGPU",
			&Backend{GPUIndex: GPUIndexPtr(1), CppWorkerConfig: &LlamaCppConfig{MainGPU: 2}},
			1, true,
		},
		{
			"явный GPU 0 важнее MainGPU=2",
			&Backend{GPUIndex: GPUIndexPtr(0), CppWorkerConfig: &LlamaCppConfig{MainGPU: 2}},
			0, true,
		},
		{
			// MainGPU=0 — это «авто/не задано», а не «явно карта 0»: сузить
			// лок по такой догадке нельзя, лок остаётся хостовым.
			"MainGPU=0 не считается индексом",
			&Backend{CppWorkerConfig: &LlamaCppConfig{MainGPU: 0}},
			0, false,
		},
		{
			// Отрицательный индекс смысла не имеет (валидация — на HTTP-границе);
			// состояние, попавшее в state.json правкой руками, тоже не должно
			// создавать ключ лока вида host#gpu-1.
			"отрицательный явный индекс = неизвестно",
			&Backend{GPUIndex: GPUIndexPtr(-1)},
			0, false,
		},
	}
	for _, c := range cases {
		got, known := c.backend.EffectiveGPUIndex()
		if got != c.want || known != c.wantKnown {
			t.Errorf("%s: EffectiveGPUIndex() = (%d, %v), want (%d, %v)",
				c.name, got, known, c.want, c.wantKnown)
		}
	}
}

// JSON-контракт поля: nil не засоряет state.json/API (omitempty), а &0
// ОБЯЗАН сериализоваться — иначе «явно GPU 0» терялся бы при записи
// state.json, и после рестарта балансера лок молча стал бы хостовым.
func TestBackend_GPUIndexJSONTags(t *testing.T) {
	raw, err := json.Marshal(Backend{ID: "b1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "gpuIndex") {
		t.Errorf("GPUIndex=nil обязан отсутствовать в JSON (omitempty): %s", raw)
	}

	raw, err = json.Marshal(Backend{ID: "b1", GPUIndex: GPUIndexPtr(0)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"gpuIndex":0`) {
		t.Errorf("GPUIndex=&0 обязан сериализоваться как gpuIndex:0 (иначе «явно GPU 0» "+
			"не переживёт перезапуск): %s", raw)
	}

	raw, err = json.Marshal(Backend{ID: "b1", GPUIndex: GPUIndexPtr(1)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"gpuIndex":1`) {
		t.Errorf("GPUIndex=&1 обязан сериализоваться как gpuIndex:1: %s", raw)
	}
}

// Round-trip через JSON: так поле живёт в state.json и в API. Ключа нет и
// null — оба дают nil («неизвестно»), 0 — указатель на 0 («явно GPU 0»).
func TestBackend_GPUIndexJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *int
	}{
		{"ключа нет", `{"id":"b1"}`, nil},
		{"null", `{"id":"b1","gpuIndex":null}`, nil},
		{"ноль", `{"id":"b1","gpuIndex":0}`, GPUIndexPtr(0)},
		{"двойка", `{"id":"b1","gpuIndex":2}`, GPUIndexPtr(2)},
	}
	for _, c := range cases {
		var b Backend
		if err := json.Unmarshal([]byte(c.in), &b); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		switch {
		case c.want == nil && b.GPUIndex != nil:
			t.Errorf("%s: GPUIndex = %d, want nil («неизвестно»)", c.name, *b.GPUIndex)
		case c.want != nil && b.GPUIndex == nil:
			t.Errorf("%s: GPUIndex = nil, want %d", c.name, *c.want)
		case c.want != nil && *b.GPUIndex != *c.want:
			t.Errorf("%s: GPUIndex = %d, want %d", c.name, *b.GPUIndex, *c.want)
		}
	}
}
