// contract_golden_r493_test.go — R493-11c (2026-11-12): GOLDEN-БАЙТЫ КОНТРАКТА
// балансер ↔ cppworker.
//
// ДОЛГ. `docs/architecture/balancer-cppworker-contract.ru.md` §12 обещал фазу 3:
// `testdata/cppworker_golden/*.json` + `contract_conformance_test.go`. Ни того, ни
// другого не существовало: контракт проверялся только зеркальными структурами
// (L1), которые сами могут разойтись с реальным JSON. Golden-файлы снимаются с
// ЖИВОГО воркера (происхождение — в testdata/cppworker_golden/README.md).
//
// ЧТО ЭТОТ ТЕСТ ЛОВИТ:
//   - переименование/удаление поля, которое балансер ЧИТАЕТ (разбор golden даёт
//     нули или неполные значения);
//   - появление в свежем golden ключа, о котором балансер не знает (покрытие
//     ключей заставляет принять решение: зеркалить или явно игнорировать);
//   - лишний/пропавший ключ в ЗАПРОСЕ балансера (cppworker декодирует строго —
//     живой R51.4 давал 400 «unknown field» на все preflight reload'ы).
package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ============================================================
// Зеркальные структуры ОТВЕТОВ: только то, что балансер читает.
// Теги — те же, что в production-коде (internal/balancer/llamacpp_metrics_poller.go,
// internal/agent/worker_identity_r493.go, cmd/cppworker handlers_health.go).
// ============================================================

// cppworkerModelsGolden — ответ /api/models (читается поллером метрик).
type cppworkerModelsGolden struct {
	Models             []cppworkerModelGolden `json:"models"`
	VramRemoteSource   string                 `json:"vram_remote_source"`
	Count              int                    `json:"count"`
	MaxVRAMNCtx        int                    `json:"max_vram_n_ctx"`
	ModelMaxContext    int                    `json:"model_max_context"`
	FeasibleMaxContext int                    `json:"feasible_max_context"`
	GGUFMaxContext     int                    `json:"gguf_max_context"`
	AvailableVRAMMB    uint64                 `json:"available_vram_mb"`
	TotalVRAMMB        uint64                 `json:"total_vram_mb"`
	VramLocalFreeMB    uint64                 `json:"vram_local_free_mb"`
	VramRemoteFreeMB   uint64                 `json:"vram_remote_free_mb"`
	VramRemoteNodes    int                    `json:"vram_remote_nodes"`
	RPCServersActive   []string               `json:"rpc_servers_active"`
	VramKnown          bool                   `json:"vram_known"`
	RPCEnabled         bool                   `json:"rpc_enabled"`
}

// cppworkerModelGolden — одна модель внутри /api/models.
type cppworkerModelGolden struct {
	Name                 string                 `json:"name"`
	Path                 string                 `json:"path"`
	State                string                 `json:"state"`
	Architecture         string                 `json:"architecture"`
	Quantization         string                 `json:"quantization"`
	LoadedAt             string                 `json:"loaded_at"`
	KvCacheType          string                 `json:"kv_cache_type"`
	DeviceLayersExact    []cppworkerLayerGolden `json:"device_layers_exact"`
	SizeBytes            int64                  `json:"size_bytes"`
	Size                 int64                  `json:"size"`
	SizeVRAM             uint64                 `json:"size_vram"`
	ContextSize          int                    `json:"context_size"`
	ContextPerSeq        int                    `json:"context_per_seq"`
	MaxSlots             int                    `json:"max_slots"`
	Parallel             int                    `json:"parallel"`
	BatchSize            int                    `json:"batch_size"`
	NLayers              int                    `json:"n_layers"`
	NEmbd                int                    `json:"n_embd"`
	GGUFContextLength    int                    `json:"gguf_context_length"`
	GPULayersActual      int                    `json:"gpu_layers_actual"`
	FeasibleMaxContext   int                    `json:"feasible_max_context"`
	GGUFMaxContext       int                    `json:"gguf_max_context"`
	ReasoningEnabled     *bool                  `json:"reasoning_enabled"`
	LayoutDegraded       bool                   `json:"layout_degraded"`
	LayoutDegradedReason string                 `json:"layout_degraded_reason"`
	VramKnown            bool                   `json:"vram_known"`
	// ПОЛЕЙ НИЖЕ В РЕАЛЬНОМ /api/models НЕТ (проверено golden-снимком):
	//   n_kv_heads        — добирается из /api/show (arch_enrich_r230), иначе
	//                       AutoTune считал бы KV-cache без числа голов;
	//   flash_attn_type   — приходит в /api/info (build/флаги), не в списке моделей;
	//   use_mmap          — то же: параметр загрузки, а не поле метрик.
	// Держать их в зеркале значило бы утверждать про протокол то, чего в нём нет.
}

// cppworkerLayerGolden — раскладка слоёв (R118.16).
type cppworkerLayerGolden struct {
	Device string `json:"device"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Count  int    `json:"count"`
	Node   string `json:"node"`
}

// cppworkerInfoGolden — ответ /api/info: то, что читают агент (12i) и диагностика.
type cppworkerInfoGolden struct {
	BuildVersion   string   `json:"build_version"`
	BuildCommit    string   `json:"build_commit"`
	BuildDate      string   `json:"build_date"`
	BuildImageTag  string   `json:"build_image_tag"`
	BuildService   string   `json:"build_service"`
	LlamaCommit    string   `json:"llama_commit"`
	LlamaDate      string   `json:"llama_date"`
	RPCProto       string   `json:"rpc_proto"`
	CudaArchBuilt  []string `json:"cuda_arch_built"`
	CudaArchOK     bool     `json:"cuda_arch_ok"`
	ModelsLoaded   int      `json:"modelsLoaded"`
	ModelsUnloaded int      `json:"modelsUnloaded"`
	Version        string   `json:"version"`
}

// cppworkerHealthGolden — ответ /api/health.
type cppworkerHealthGolden struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ============================================================
// Ключи golden, которые балансер сознательно НЕ читает.
// Каждый обязан иметь причину: иначе «неизвестный ключ» — это решение, а не мусор.
// ============================================================

var ignoredModelsKeysR493 = map[string]string{
	"available_ram_mb":                "RAM узла для размещения — решает воркер, балансер берёт VRAM",
	"can_load_own_models":             "роль узла: балансер читает own_copies/rpc_layout отдельно",
	"donor":                           "роль донора — операторское решение в конфиге балансера",
	"donor_reason":                    "текст для человека, машиночитаемого смысла нет",
	"gpu_count":                       "число GPU берётся из метрик агента",
	"max_ram_n_ctx":                   "оценка RAM-окна — внутри воркера",
	"own_copies":                      "раскладка копий читается через /api/v1/backends, не здесь",
	"role":                            "роль узла — из конфига балансера (R304)",
	"rpc_excluded":                    "исключения RPC — диагностика воркера",
	"rpc_layout":                      "состояние раскладки — отдельный путь R383",
	"rpc_server":                      "параметры rpc-server нужны только RPC-узлам (R115)",
	"total_ram_mb":                    "RAM — не участвует в выборе узла (VRAM участвует)",
	"vram_layout_free_mb":             "агрегаты раскладки — читаются из local/remote полей",
	"vram_layout_total_mb":            "агрегаты раскладки — читаются из local/remote полей",
	"vram_local_total_mb":             "знаменатель для free; решает воркер",
	"vram_remote_total_mb":            "знаменатель для free; решает воркер",
	"vram_remote_last_known_age_sec":  "протухание кэша VRAM — внутри воркера",
	"vram_remote_last_known_free_mb":  "снимок прошлого опроса — для диагностики",
	"vram_remote_last_known_nodes":    "снимок прошлого опроса — для диагностики",
	"vram_remote_last_known_total_mb": "снимок прошлого опроса — для диагностики",
}

var ignoredModelKeysR493 = map[string]string{
	"active_queries":      "счётчик воркера; занятость балансер считает сам (слоты)",
	"batched_parallel":    "режим планировщика — читается при регистрации, не из метрик",
	"gpu_count":           "число GPU на модель — не влияет на выбор",
	"max_exact_fit_n_ctx": "оценка точного fit — внутренняя арифметика воркера",
	"max_vram_n_ctx":      "потолок окна по VRAM — балансер берёт feasible_max_context",
	"n_ctx_degraded":      "флаг деградации окна — текст для человека",
	"n_ctx_headroom":      "запас окна — арифметика воркера",
	"n_vocab":             "словарь — балансеру не нужен",
	"offload_degraded":    "флаг деградации оффлоада — текст для человека",
	"offload_note":        "пояснение к деградации — текст для человека",
}

// ============================================================
// Тесты
// ============================================================

func readGoldenR493(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "cppworker_golden", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s не читается: %v (файлы контракта — обязательная часть репозитория)", name, err)
	}
	return raw
}

// TestGolden_ModelsLoaded — /api/models разбирается и даёт те значения, на
// которых держится выбор узла и окна.
func TestGolden_ModelsLoaded(t *testing.T) {
	raw := readGoldenR493(t, "models_loaded.json")
	var resp cppworkerModelsGolden
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("golden не разобран нашими структурами: %v", err)
	}
	if resp.Count != 1 || len(resp.Models) != 1 {
		t.Fatalf("count=%d models=%d, ожидалось 1/1", resp.Count, len(resp.Models))
	}
	m := resp.Models[0]
	if m.Name != "Qwen3-4B-Instruct-2507-Q4_K_M" {
		t.Fatalf("name = %q", m.Name)
	}
	if m.ContextSize != 8192 || m.ContextPerSeq != 8192 {
		t.Fatalf("окно: context_size=%d context_per_seq=%d, ожидалось 8192/8192 "+
			"(иначе клиент и гейт R459 считают по разным числам)", m.ContextSize, m.ContextPerSeq)
	}
	if m.MaxSlots != 1 || m.Parallel != 1 {
		t.Fatalf("слоты: max_slots=%d parallel=%d", m.MaxSlots, m.Parallel)
	}
	if m.GGUFContextLength != 262144 {
		t.Fatalf("gguf_context_length = %d: без него 12e не отдаст физический предел", m.GGUFContextLength)
	}
	if m.FeasibleMaxContext <= 0 {
		t.Fatalf("feasible_max_context = %d: preflight не сможет решить, можно ли растить окно",
			m.FeasibleMaxContext)
	}
	if m.Architecture != "qwen3" || m.Quantization != "Q4_K_M" {
		t.Fatalf("архитектура/квант: %q/%q", m.Architecture, m.Quantization)
	}
	if m.SizeBytes <= 0 || m.SizeVRAM <= 0 {
		t.Fatalf("размер/VRAM модели: %d/%d — R248 считает fit по ним", m.SizeBytes, m.SizeVRAM)
	}
	if len(m.DeviceLayersExact) == 0 {
		t.Fatal("device_layers_exact пуст: R118.16 не покажет раскладку")
	}
	if !resp.VramKnown || resp.AvailableVRAMMB == 0 || resp.TotalVRAMMB == 0 {
		t.Fatalf("VRAM: known=%v free=%d total=%d", resp.VramKnown, resp.AvailableVRAMMB, resp.TotalVRAMMB)
	}
	if resp.FeasibleMaxContext <= 0 || resp.GGUFMaxContext <= 0 {
		t.Fatalf("top-level feasible/gguf: %d/%d", resp.FeasibleMaxContext, resp.GGUFMaxContext)
	}
}

// TestGolden_Info — /api/info: его build_* поля теперь источник идентичности
// воркера (12i), поэтому контракт на них обязателен.
func TestGolden_Info(t *testing.T) {
	raw := readGoldenR493(t, "info.json")
	var info cppworkerInfoGolden
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("golden не разобран: %v", err)
	}
	if info.BuildService != "cppworker" {
		t.Fatalf("build_service = %q", info.BuildService)
	}
	if !isHexSHA493(info.BuildCommit) {
		t.Fatalf("build_commit = %q: идентичность воркера должна быть SHA (иначе 12i снова начнёт врать)",
			info.BuildCommit)
	}
	if info.BuildVersion == "" || info.BuildImageTag == "" || info.BuildDate == "" {
		t.Fatalf("build-поля неполные: version=%q image=%q date=%q",
			info.BuildVersion, info.BuildImageTag, info.BuildDate)
	}
	if !isVersionTriple493(info.RPCProto) {
		t.Fatalf("rpc_proto = %q: контракт раскладки требует X.Y.Z", info.RPCProto)
	}
	if len(info.CudaArchBuilt) == 0 {
		t.Fatal("cuda_arch_built пуст: гард R135 не сможет предупредить о несовместимой карте")
	}
	if info.LlamaCommit == "" || info.LlamaDate == "" {
		t.Fatalf("llama_commit/date пусты: совместимость сторон RPC (R115) не проверить")
	}
}

// TestGolden_Health — минимальный, но обязательный ответ.
func TestGolden_Health(t *testing.T) {
	raw := readGoldenR493(t, "health.json")
	var h cppworkerHealthGolden
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("golden не разобран: %v", err)
	}
	if h.Status != "ok" {
		t.Fatalf("status = %q, ожидалось ok", h.Status)
	}
}

// TestGolden_KeyCoverage — каждый ключ golden зеркалится или явно игнорируется.
func TestGolden_KeyCoverage(t *testing.T) {
	t.Run("models", func(t *testing.T) {
		assertKeysCoveredR493(t, readGoldenR493(t, "models_loaded.json"),
			reflect.TypeOf(cppworkerModelsGolden{}), ignoredModelsKeysR493)
		// Вложенная модель: ключи внутри models[0].
		raw := readGoldenR493(t, "models_loaded.json")
		var top struct {
			Models []json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(raw, &top); err != nil || len(top.Models) == 0 {
			t.Fatalf("не достать models[0]: %v", err)
		}
		assertKeysCoveredR493(t, top.Models[0], reflect.TypeOf(cppworkerModelGolden{}), ignoredModelKeysR493)
	})
	t.Run("info", func(t *testing.T) {
		assertKeysCoveredR493(t, readGoldenR493(t, "info.json"),
			reflect.TypeOf(cppworkerInfoGolden{}), map[string]string{
				"activeModels":         "счётчик воркера — балансер считает сам",
				"activeQueries":        "счётчик воркера — балансер считает сам",
				"activeRequests":       "счётчик воркера — балансер считает сам",
				"avgDurationMs":        "метрика воркера для WebUI, не для решений балансера",
				"build":                "строка для человека; машиночитаемое — build_*",
				"build_author":         "автор сборки — для человека",
				"build_commit_date":    "дата коммита — для человека",
				"build_commit_full":    "полный SHA — для отчётов, короткого хватает",
				"cancel_supported":     "возможность отмены — проверяется по факту вызова",
				"gpuCount":             "число GPU берётся из метрик агента",
				"gpuDevices":           "детализация GPU — из агента (NVML)",
				"gpuMemoryTotalMb":     "устаревшее поле (есть gpuDevices)",
				"gpuMemoryUsedMb":      "устаревшее поле (есть gpuDevices)",
				"llama_build":          "строкой для человека; машиночитаемое — llama_commit",
				"llama_build_date":     "дата сборки llama.cpp — она же в llama_date и в строке llama_build",
				"llama_build_env":      "источник (env) — для разбора, не для решений",
				"llama_build_mismatch": "флаг для человека; сравнение делается по коммиту",
				"load_stale_after_sec": "порог воркера для собственной диагностики",
				"load_wait_limit_sec":  "порог воркера; балансер свои таймауты держит у себя",
				"loadedModels":         "счётчики воркера для WebUI",
				"models":               "список читается поллером из /api/models",
				"net":                  "сетевые детали — диагностика R118",
				"tokenLatencyMs":       "метрика для монитора",
				"totalErrors":          "счётчик воркера",
				"totalModels":          "счётчик воркера",
				"totalQueries":         "счётчик воркера",
				"totalRequests":        "счётчик воркера",
				"totalTokens":          "счётчик воркера",
				"uptimeSeconds":        "для монитора",
				"version":              "версия моста llama.cpp — отображается, решений не принимает",
				"modelsUnloaded":       "счётчик воркера для WebUI",
			})
	})
	t.Run("health", func(t *testing.T) {
		assertKeysCoveredR493(t, readGoldenR493(t, "health.json"),
			reflect.TypeOf(cppworkerHealthGolden{}), nil)
	})
}

// TestGolden_RequestStrict — запросы разбираются строго (как cppworker) и
// содержат ровно те ключи, что знает его структура.
func TestGolden_RequestStrict(t *testing.T) {
	t.Run("load", func(t *testing.T) {
		raw := readGoldenR493(t, "load_request.json")
		var req CppworkerLoadRequest
		strictDecodeR493(t, raw, &req, "cppworker loadModelRequest")
		if req.Name == "" {
			t.Fatal("name пуст: cppworker не поймёт, какую модель грузить")
		}
		if req.ContextSize == nil || *req.ContextSize <= 0 {
			t.Fatal("contextSize не передан: окно загрузки не задано")
		}
	})
	t.Run("reload", func(t *testing.T) {
		raw := readGoldenR493(t, "reload_request.json")
		var req CppworkerReloadRequest
		strictDecodeR493(t, raw, &req, "cppworker reloadModelRequest")
		if req.Name == "" {
			t.Fatal("name пуст")
		}
	})
}

// strictDecodeR493 — разбор с запретом незнакомых полей: так делает cppworker.
func strictDecodeR493(t *testing.T, raw []byte, dst interface{}, what string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("%s не принял golden: %v (это ровно тот класс, что R51.4: «unknown field» на все reload'ы)", what, err)
	}
}

// assertKeysCoveredR493 — каждый ключ JSON должен быть либо зеркалён структурой,
// либо явно записан в ignored с причиной. И наоборот: каждый зеркалируемый ключ
// обязан присутствовать в golden (иначе golden описывает не тот протокол).
func assertKeysCoveredR493(t *testing.T, raw []byte, structType reflect.Type, ignored map[string]string) {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("golden не разобран как объект: %v", err)
	}
	mirrored := mirroredKeysR493(structType)
	for key := range got {
		if mirrored[key] {
			continue
		}
		if reason, ok := ignored[key]; ok && strings.TrimSpace(reason) != "" {
			continue
		}
		t.Fatalf("ключ %q есть в golden, но ни зеркалится, но описан как игнорируемый: "+
			"либо добавьте поле в %s, либо запишите причину в ignored-карту", key, structType.Name())
	}
	for key := range mirrored {
		if _, ok := got[key]; !ok {
			t.Fatalf("поле %q зеркалится, но в golden его нет: golden снят с другой версии "+
				"протокола — переснимите его или уберите поле из структуры", key)
		}
	}
	if len(mirrored) == 0 {
		t.Fatalf("%s не зеркалирует ни одного ключа: контракт проверять нечем", structType.Name())
	}
}

// mirroredKeysR493 — набор JSON-имён полей структуры (первое имя из тега).
func mirroredKeysR493(structType reflect.Type) map[string]bool {
	out := make(map[string]bool, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		tag := structType.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// isHexSHA493 — короткий или полный SHA.
func isHexSHA493(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// isVersionTriple493 — X.Y.Z (контракт ggml-rpc).
func isVersionTriple493(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ensureFormattingUsedR493 — fmt используется в сообщениях выше; держим импорт
// явно, чтобы правка сообщений не ломала сборку неожиданно.
var _ = fmt.Sprintf
