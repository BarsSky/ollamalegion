// gguf_backends_parameters_r119_test.go — R119 (2026-10-15), пункт 4.
//
// Страница GGUF рисует карточку загруженной модели из /api/v1/gguf/backends.
// Пункт 4 требует, чтобы рядом со значением был ВИДЕН источник (профиль / запрос /
// дефолт / cppworker), иначе оператор не понимает, «применились ли настройки».
//
// Здесь проверяем контракт ответа (поле parameters доезжает до JSON) и то, что
// обработчик действительно спрашивает provenance у балансера. Сквозная проверка
// «загрузка → источник в ответе» делается живьём: она требует реальной загрузки
// модели через балансер (см. CHANGELOG 0.7.115).
package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestR119_GgufBackendModel_CarriesParameters — поле parameters с источником
// доезжает до JSON-ответа (иначе UI нечего рисовать).
func TestR119_GgufBackendModel_CarriesParameters(t *testing.T) {
	info := ggufBgModelInfo{
		Name:        "gemma-4-E4B-it-Q4_K_M",
		Status:      "loaded",
		ContextSize: 8192,
		Parameters: map[string]interface{}{
			"contextLength": map[string]interface{}{"value": 8192, "source": "profile"},
			"parallel":      map[string]interface{}{"value": 2, "source": "profile"},
			"batchSize":     map[string]interface{}{"value": 512, "source": "default_profile"},
			"gpuLayers":     map[string]interface{}{"value": nil, "source": "backend"},
		},
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	for _, want := range []string{`"parameters"`, `"source":"profile"`, `"source":"default_profile"`, `"source":"backend"`, `"value":8192`} {
		if !strings.Contains(js, want) {
			t.Errorf("в JSON модели нет %s: %s", want, js)
		}
	}

	// Модель без provenance не должна обрастать пустым объектом.
	bare, _ := json.Marshal(ggufBgModelInfo{Name: "m", Status: "loaded"})
	if strings.Contains(string(bare), "parameters") {
		t.Errorf("omitempty не работает: %s", bare)
	}
}

// TestR119_GgufBackends_HandlerConsultsProvenance — защита от отката: обработчик
// обязан приписать provenance каждой загруженной модели.
func TestR119_GgufBackends_HandlerConsultsProvenance(t *testing.T) {
	raw, err := os.ReadFile("gguf_backends_handler.go")
	if err != nil {
		t.Fatalf("read gguf_backends_handler.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "LoadProvenanceJSON(bm.ID, info.Models[i].Name)") {
		t.Error("handleGgufBackends больше не приписывает provenance моделям — " +
			"UI снова покажет только значения без источника")
	}
	if !strings.Contains(src, "info.Models[i].Parameters = prov") {
		t.Error("provenance получен, но не положен в модель ответа")
	}
}
