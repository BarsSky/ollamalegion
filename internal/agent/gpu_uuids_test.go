// gpu_uuids_test.go — R-Image follow-up (2026-10-07).
//
// ЗАЧЕМ ЭТОТ ТЕСТ. На одной машине подняты cppworker (текст) и imageworker
// (картинки). Оба агента читают nvidia-smi ОДНОЙ видеокарты, поэтому балансер
// обязан понимать, что это одна физическая GPU, — иначе он складывает её память
// дважды и показывает 16 GB на карте в 8 GB (живой дефект оператора).
//
// Признак «одна физическая карта» — UUID из nvidia-smi. Для image-воркера UUID
// попадал в метрики, а для llama.cpp-воркера ТЕРЯЛСЯ: там GPU-метрики берутся из
// cppworker (`/api/gpu`), где UUID нет, и маппинг собирал GPUMetrics без этого
// поля. Дедуп на балансере не срабатывал — то есть фикс «для одной машины»
// работал только наполовину.
//
// Тест держит именно это место: маппинг llama-метрик ОБЯЗАН донести UUID.
package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestMapLlamaGPUMetricsWithUUIDs_KeepsUUIDs — UUID не теряются при маппинге.
func TestMapLlamaGPUMetricsWithUUIDs_KeepsUUIDs(t *testing.T) {
	gpus := []LlamaGPUInfo{
		{VRAMTotalMB: 8192, VRAMFreeMB: 6800},
	}
	uuids := []string{"GPU-6f8d5f3b-1dde-cbcf-8c26-4a1d0dbead34"}

	got := mapLlamaGPUMetricsWithUUIDs(gpus, uuids)

	if got.MemoryTotal != 8192 {
		t.Errorf("MemoryTotal = %d, want 8192", got.MemoryTotal)
	}
	if got.MemoryUsed != 8192-6800 {
		t.Errorf("MemoryUsed = %d, want %d", got.MemoryUsed, 8192-6800)
	}
	if len(got.UUIDs) != 1 || got.UUIDs[0] != uuids[0] {
		t.Fatalf("UUIDs = %v, want %v — без UUID балансер не отличит "+
			"«одна машина, два бэкенда» от «две машины» и удвоит память одной карты",
			got.UUIDs, uuids)
	}
}

// TestMapLlamaGPUMetricsWithUUIDs_NoUUIDsIsUnknown — пустой список = «неизвестно».
//
// Поведение при неизвестном UUID обязано остаться прежним: балансер в этом
// случае группирует по имени хоста. Пустой срез (а не паника и не выдуманное
// значение) — это и есть контракт «неизвестно».
func TestMapLlamaGPUMetricsWithUUIDs_NoUUIDsIsUnknown(t *testing.T) {
	got := mapLlamaGPUMetricsWithUUIDs([]LlamaGPUInfo{{VRAMTotalMB: 8192, VRAMFreeMB: 1000}}, nil)
	if len(got.UUIDs) != 0 {
		t.Errorf("UUIDs = %v, want пусто (неизвестно)", got.UUIDs)
	}
	if got.MemoryTotal != 8192 {
		t.Errorf("MemoryTotal = %d, want 8192 — память считается и без UUID", got.MemoryTotal)
	}
}

// TestMapLlamaGPUMetricsWithUUIDs_EmptyGPUs — нет карт: нулевые метрики.
func TestMapLlamaGPUMetricsWithUUIDs_EmptyGPUs(t *testing.T) {
	got := mapLlamaGPUMetricsWithUUIDs(nil, []string{"GPU-x"})
	if got.MemoryTotal != 0 || len(got.UUIDs) != 0 {
		t.Errorf("пустой список карт обязан дать нулевые метрики без UUID, got %+v", got)
	}
}

// TestMapLlamaGPUMetrics_MultiGPU — несколько карт: память суммируется, UUID идут списком.
func TestMapLlamaGPUMetrics_MultiGPU(t *testing.T) {
	gpus := []LlamaGPUInfo{
		{VRAMTotalMB: 8192, VRAMFreeMB: 4000},
		{VRAMTotalMB: 8192, VRAMFreeMB: 6000},
	}
	uuids := []string{"GPU-a", "GPU-b"}

	got := mapLlamaGPUMetricsWithUUIDs(gpus, uuids)
	if got.MemoryTotal != 16384 {
		t.Errorf("MemoryTotal = %d, want 16384", got.MemoryTotal)
	}
	if got.MemoryUsed != (8192-4000)+(8192-6000) {
		t.Errorf("MemoryUsed = %d, want %d", got.MemoryUsed, (8192-4000)+(8192-6000))
	}
	if len(got.UUIDs) != 2 {
		t.Errorf("UUIDs = %v, want два идентификатора (машина с двумя картами)", got.UUIDs)
	}
}

// TestCollectGPUUUIDs_NoSmiNoPanic — без nvidia-smi функция молчит, а не падает.
//
// В окружении без GPU (CI, CPU-стенд) nvidia-smi отсутствует. Возврат nil — это
// «неизвестно»: балансер сгруппирует по хосту, то есть сохранит прежнее
// поведение. Паника здесь обрушила бы сбор метрик целиком.
func TestCollectGPUUUIDs_NoSmiNoPanic(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nvidia-smi точно не найдётся
	if got := collectGPUUUIDs(); len(got) != 0 {
		t.Errorf("без nvidia-smi ожидался пустой список, got %v", got)
	}
}

// TestGPUMetrics_UUIDsAreSerialized — UUID обязан уезжать в JSON метрик.
//
// Балансер получает метрики ровно JSON-ом (POST /agent/metrics), поэтому поле
// без json-тега или с omitempty-ловушкой молча потерялось бы на проводе — и
// дедуп не заработал бы даже при верном коде агента.
func TestGPUMetrics_UUIDsAreSerialized(t *testing.T) {
	m := types.GPUMetrics{UUIDs: []string{"GPU-abc"}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"uuids":["GPU-abc"]`) {
		t.Fatalf("в JSON нет uuids: %s", string(b))
	}
	// Пустой список не должен появляться в JSON: старые балансеры не увидят
	// лишнего поля, а «неизвестно» и «пусто» трактуются одинаково.
	empty, err := json.Marshal(types.GPUMetrics{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(empty), "uuids") {
		t.Fatalf("пустой UUID-список не должен попадать в JSON: %s", string(empty))
	}
}
