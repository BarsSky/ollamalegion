//go:build llama_stub

// kv_plan_feasible_r707_test.go — R707 (2026-11-25): ComputeFeasible считает KV
// ПОСЛОЙНО, а не «все слои × ширина».
//
// ПРЕДМЕТ. До этой правки и путь загруженной модели, и header-путь считали KV как
// (headDimK + headDimV) · NLayers · NKvHeads · 2, то есть по ВСЕМ слоям. Для
// архитектур, где кэш живёт не во всех слоях, это завышение:
//
//	gemma-4-E4B:   42 блока, KV держат 24 слоя (4 глобальных шириной 512 и 20
//	               оконных шириной 256 с окном 512) → f16 57 344 вместо 172 032;
//	Qwen3.8-9B:    33 блока, attention-слоёв 8 (остальные — линейный attention)
//	               → f16 32 768 вместо 135 168;
//	Inkling-2-Mini: 48 слотов кэша, из них 8 глобальных (окно 1024 у 40);
//	Qwen3-4B:      плоская формула верна — контроль, числа не меняются.
//
// НЕ-ВАКУУМНОСТЬ. Ожидаемые байты записаны по формуле вручную (класс × слои ×
// головы × (K+V) × байт-на-элемент), а рядом отдельная проверка требует, чтобы
// значение ОТЛИЧАЛОСЬ от прежней плоской формулы (42 × 512, 33 × 256 …). Тесты
// красные на 4067bc25: там KVPlanSource/полей раскладки нет вовсе.
//
// Классы, ширины и окно берутся из тех же ключей GGUF, по которым llama.cpp
// строит is_swa/has_kv: llama-hparams.cpp:142-153 (ширины по классам), :194
// (n_swa), :305 is_swa, :336 has_kv; gemma4.attention.shared_kv_layers — начало
// кэша; inkling2-loop.cpp:16-30 — визиты loop-схемы.
package cppbackend

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// writeGGUFR707 — минимальный GGUF-заголовок: строки, uint32 и массивы (bool /
// uint32). Нужен для ключей, которые прежний writeFakeGGUF не умеет:
// sliding_window_pattern и head_count_kv массивом.
func writeGGUFR707(t *testing.T, dir, name string, kv map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) {
		must(binary.Write(f, w, uint64(len(s))))
		_, err := f.Write([]byte(s))
		must(err)
	}
	// Ключи в стабильном порядке: general.architecture первым, дальше — как дали.
	keys := make([]string, 0, len(kv))
	if _, ok := kv["general.architecture"]; ok {
		keys = append(keys, "general.architecture")
	}
	for k := range kv {
		if k != "general.architecture" {
			keys = append(keys, k)
		}
	}
	// сортировка не нужна, но фиксируем: без неё map-порядок всё равно валиден
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))         // tensorCount
	must(binary.Write(f, w, uint64(len(keys)))) // metadataKvCount

	for _, k := range keys {
		str(k)
		switch v := kv[k].(type) {
		case string:
			must(binary.Write(f, w, uint32(8)))
			str(v)
		case uint32:
			must(binary.Write(f, w, uint32(4)))
			must(binary.Write(f, w, v))
		case int:
			must(binary.Write(f, w, uint32(4)))
			must(binary.Write(f, w, uint32(v)))
		case []bool:
			must(binary.Write(f, w, uint32(9))) // array
			must(binary.Write(f, w, uint32(7))) // bool
			must(binary.Write(f, w, uint64(len(v))))
			for _, b := range v {
				var x uint8
				if b {
					x = 1
				}
				must(binary.Write(f, w, x))
			}
		case []uint32:
			must(binary.Write(f, w, uint32(9)))
			must(binary.Write(f, w, uint32(4))) // uint32
			must(binary.Write(f, w, uint64(len(v))))
			for _, x := range v {
				must(binary.Write(f, w, x))
			}
		default:
			t.Fatalf("writeGGUFR707: тип %T для ключа %q не поддержан", v, k)
		}
	}
	return path
}

// feasibleBackendR707 — backend над каталогом с fake GGUF и «видимой» картой.
//
// freeMB/totalMB инжектируются в gpuDevices: в stub-сборке NVML нет, а
// ComputeFeasible берёт свободную VRAM именно оттуда (getFreeMemoryMB).
func feasibleBackendR707(t *testing.T, dir, kvType string, freeMB, totalMB uint64) *Backend {
	t.Helper()
	b := NewBackend(Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: kvType,
	})
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	b.mu.Lock()
	b.gpuDevices = append(b.gpuDevices, bridge.GPUDevice{VRAMFreeMB: freeMB, VRAMTotalMB: totalMB})
	b.gpuCount = 1
	b.mu.Unlock()
	return b
}

// budgetBytesR707 — сколько байт остаётся под KV при заданной свободной VRAM.
// Считается тем же хелпером, что и в ComputeFeasible, поэтому тест не дублирует
// арифметику резервов.
func budgetBytesR707(t *testing.T, freeMB, weightsMB, reserveMB uint64) int64 {
	t.Helper()
	avail, ok := kvBudgetAfterWeightsMB(freeMB, weightsMB, reserveMB)
	if !ok {
		t.Fatalf("бюджета нет: free=%d weights=%d reserve=%d", freeMB, weightsMB, reserveMB)
	}
	return int64(avail) * 1024 * 1024
}

// gemma4KeysR707 — ключи реального gemma-4-E4B-it-Q4_K_M (сняты с файла стенда).
func gemma4KeysR707() map[string]interface{} {
	pattern := make([]bool, 0, 42)
	for i := 0; i < 7; i++ {
		pattern = append(pattern, true, true, true, true, true, false)
	}
	return map[string]interface{}{
		"general.architecture":                    "gemma4",
		"gemma4.block_count":                      uint32(42),
		"gemma4.attention.head_count":             uint32(8),
		"gemma4.attention.head_count_kv":          uint32(2),
		"gemma4.embedding_length":                 uint32(2560),
		"gemma4.context_length":                   uint32(131072),
		"gemma4.attention.key_length":             uint32(512),
		"gemma4.attention.value_length":           uint32(512),
		"gemma4.attention.key_length_swa":         uint32(256),
		"gemma4.attention.value_length_swa":       uint32(256),
		"gemma4.attention.shared_kv_layers":       uint32(18),
		"gemma4.attention.sliding_window":         uint32(512),
		"gemma4.attention.sliding_window_pattern": pattern,
	}
}

// TestR707_Gemma4KVByLayerPlan — KV gemma-4 считается по классам слоёв, а не 42×512.
func TestR707_Gemma4KVByLayerPlan(t *testing.T) {
	dir := t.TempDir()
	writeGGUFR707(t, dir, "gemma-4-E4B-it-Q4_K_M.gguf", gemma4KeysR707())

	b := feasibleBackendR707(t, dir, "f16", 7097, 8191)
	info, err := b.ComputeFeasible("gemma-4-E4B-it-Q4_K_M")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}

	// Раскладка: 24 слоя с кэшем = 4 глобальных + 20 оконных, окно 512.
	if info.KVPlanSource != KVPlanSourcePlan || !info.KVPlanExact {
		t.Fatalf("KVPlanSource = %q exact = %v, want plan/true", info.KVPlanSource, info.KVPlanExact)
	}
	if info.KVLayers != 24 || info.KVGlobalLayers != 4 || info.KVSWALayers != 20 {
		t.Errorf("слои: всего %d, глобальных %d, оконных %d — want 24/4/20",
			info.KVLayers, info.KVGlobalLayers, info.KVSWALayers)
	}
	if info.KVSWAWindow != 512 || info.KVGlobalHeadDim != 512 || info.KVSWAHeadDim != 256 || info.KVSWAHeadDimV != 256 {
		t.Errorf("ширины/окно: win=%d gK=%d sK=%d sV=%d, want 512/512/256/256",
			info.KVSWAWindow, info.KVGlobalHeadDim, info.KVSWAHeadDim, info.KVSWAHeadDimV)
	}

	// KV на токен f16 = 4·2·(512+512)·2 + 20·2·(256+256)·2 = 16 384 + 40 960 = 57 344.
	const wantF16 = 4*2*(512+512)*2 + 20*2*(256+256)*2
	if info.KVPerToken != wantF16 {
		t.Fatalf("KVPerToken = %d, want %d", info.KVPerToken, wantF16)
	}
	// НЕ-ВАКУУМНОСТЬ: плоская формула «42 слоя × 512» дала бы 172 032.
	const flat = (512 + 512) * 42 * 2 * 2
	if info.KVPerToken == flat {
		t.Fatalf("KVPerToken = %d — считается по всем 42 слоям, план не применён", flat)
	}
	if info.KVPerToken*3 > flat {
		t.Errorf("KV = %d не меньше трети плоского %d — раскладка подозрительно слабая", info.KVPerToken, flat)
	}

	// Потолок окна: бюджет минут оконную часть на 512 токенов, дальше делят только
	// глобальные слои → упирается в обучающий контекст 131072.
	budget := budgetBytesR707(t, 7097, 0, computeBufferReserveMB(0))
	flatCtx := int(budget) / flat
	if info.MaxVRAMCtx != 131072 {
		t.Fatalf("MaxVRAMCtx = %d, want 131072 (gguf-потолок при SWA-ограничении)", info.MaxVRAMCtx)
	}
	if info.MaxVRAMCtx < flatCtx*3 {
		t.Errorf("MaxVRAMCtx = %d не вырос относительно плоской оценки %d — окно не учтено",
			info.MaxVRAMCtx, flatCtx)
	}

	// Оконные слои не растут после окна: полный KV на 65536 << kvPerToken × 65536.
	plan := b
	_ = plan
	fullAtCtx := int64(info.KVGlobalLayers)*2*int64(512+512)*2*65536 +
		int64(info.KVSWALayers)*2*int64(256+256)*2*512
	naive := int64(flat) * 65536
	if fullAtCtx*5 > naive {
		t.Errorf("KV на 65536 (%d) должен быть заметно меньше наивного (%d)", fullAtCtx, naive)
	}
}

// TestR707_Qwen38OnlyAttentionLayers — у Qwen3.8-9B (qwen35) KV держат только
// attention-слои: 8 из 33 блоков, а не все 33.
func TestR707_Qwen38OnlyAttentionLayers(t *testing.T) {
	dir := t.TempDir()
	writeGGUFR707(t, dir, "qwen38-9b.gguf", map[string]interface{}{
		"general.architecture":           "qwen35",
		"qwen35.block_count":             uint32(33),
		"qwen35.attention.head_count":    uint32(16),
		"qwen35.attention.head_count_kv": uint32(4),
		"qwen35.embedding_length":        uint32(4096),
		"qwen35.context_length":          uint32(262144),
		"qwen35.attention.key_length":    uint32(256),
		"qwen35.attention.value_length":  uint32(256),
		"qwen35.nextn_predict_layers":    uint32(1),
		"qwen35.full_attention_interval": uint32(4),
	})

	b := feasibleBackendR707(t, dir, "f16", 7097, 8191)
	info, err := b.ComputeFeasible("qwen38-9b")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}

	if info.KVPlanSource != KVPlanSourcePlan {
		t.Fatalf("KVPlanSource = %q, want plan", info.KVPlanSource)
	}
	if info.KVLayers != 8 || info.KVGlobalLayers != 8 || info.KVSWALayers != 0 {
		t.Fatalf("KV-слоёв %d (глобальных %d, оконных %d), want 8/8/0",
			info.KVLayers, info.KVGlobalLayers, info.KVSWALayers)
	}
	const wantF16 = 8 * 4 * (256 + 256) * 2
	if info.KVPerToken != wantF16 {
		t.Fatalf("KVPerToken = %d, want %d (8 attention-слоёв)", info.KVPerToken, wantF16)
	}
	// Плоская формула по 33 блокам: 135 168 — именно она стояла на 4067bc25.
	const flat = (256 + 256) * 33 * 4 * 2
	if info.KVPerToken == flat {
		t.Fatalf("KVPerToken = %d — учитываются все 33 блока, а не attention-слои", flat)
	}

	budget := budgetBytesR707(t, 7097, 0, computeBufferReserveMB(0))
	if want := int(budget) / wantF16; info.MaxVRAMCtx != want {
		t.Errorf("MaxVRAMCtx = %d, want %d (бюджет / KV attention-слоёв)", info.MaxVRAMCtx, want)
	}
	if flatMax := int(budget) / flat; info.MaxVRAMCtx < flatMax*4 {
		t.Errorf("MaxVRAMCtx = %d не вырос вчетверо относительно плоской оценки %d",
			info.MaxVRAMCtx, flatMax)
	}
}

// TestR707_Qwen3ControlFlatUnchanged — контроль: у Qwen3-4B KV живёт во всех
// слоях, плоская формула верна, и числа обязаны остаться прежними.
func TestR707_Qwen3ControlFlatUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeGGUFR707(t, dir, "qwen3-4b.gguf", map[string]interface{}{
		"general.architecture":          "qwen3",
		"qwen3.block_count":             uint32(36),
		"qwen3.attention.head_count":    uint32(32),
		"qwen3.attention.head_count_kv": uint32(8),
		"qwen3.embedding_length":        uint32(2560),
		"qwen3.context_length":          uint32(262144),
		"qwen3.attention.key_length":    uint32(128),
		"qwen3.attention.value_length":  uint32(128),
	})

	b := feasibleBackendR707(t, dir, "f16", 7097, 8191)
	info, err := b.ComputeFeasible("qwen3-4b")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	if info.KVPlanSource != KVPlanSourceFlat {
		t.Fatalf("KVPlanSource = %q, want flat (послойных данных нет)", info.KVPlanSource)
	}
	if info.KVGlobalLayers != 36 || info.KVSWALayers != 0 {
		t.Errorf("слои: глобальных %d, оконных %d — want 36/0", info.KVGlobalLayers, info.KVSWALayers)
	}
	// Ровно прежняя формула: (128 + 128) · 36 · 8 · 2 = 147 456.
	const wantKV = (128 + 128) * 36 * 8 * 2
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (контроль: числа не должны меняться)", info.KVPerToken, wantKV)
	}
	// Потолок окна — тоже прежний: бюджет / KV на токен.
	budget := budgetBytesR707(t, 7097, 0, computeBufferReserveMB(0))
	if want := int(budget) / wantKV; info.MaxVRAMCtx != want {
		t.Errorf("MaxVRAMCtx = %d, want %d (прежняя формула)", info.MaxVRAMCtx, want)
	}
}

// TestR707_UnknownArchFallsBackToFlat — данных по слоям нет (нестандартная
// модель): KV считается прежней плоской формулой, и это ВИДНО в источнике.
func TestR707_UnknownArchFallsBackToFlat(t *testing.T) {
	dir := t.TempDir()
	writeGGUFR707(t, dir, "plain.gguf", map[string]interface{}{
		"general.architecture":          "llama",
		"llama.block_count":             uint32(32),
		"llama.attention.head_count":    uint32(32),
		"llama.attention.head_count_kv": uint32(8),
		"llama.embedding_length":        uint32(2560),
		"llama.context_length":          uint32(32768),
	})

	info, err := feasibleBackendR707(t, dir, "f16", 7097, 8191).ComputeFeasible("plain")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	if info.KVPlanSource != KVPlanSourceFlat {
		t.Fatalf("KVPlanSource = %q, want %q", info.KVPlanSource, KVPlanSourceFlat)
	}
	if info.KVPlanExact {
		t.Error("KVPlanExact = true для плоского fallback — оценка выдаётся за факт")
	}
	const wantKV = 2 * 32 * 8 * (2560 / 32) * 2
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (плоская формула)", info.KVPerToken, wantKV)
	}
	if info.KVLayers != 32 {
		t.Errorf("KVLayers = %d, want 32 (все слои)", info.KVLayers)
	}
}

// TestR707_InklingVisitsAndSWAWindow — Inkling-2-Mini: 48 слотов кэша, из них 8
// глобальных (окно 1024 у остальных 40). Ширины берутся по физическому слою.
func TestR707_InklingVisitsAndSWAWindow(t *testing.T) {
	pattern := make([]bool, 0, 48)
	for i := 0; i < 8; i++ {
		pattern = append(pattern, true, true, true, true, true, false)
	}
	kvHeads := make([]uint32, 48)
	for i := range kvHeads {
		kvHeads[i] = 8
	}
	dir := t.TempDir()
	writeGGUFR707(t, dir, "inkling.gguf", map[string]interface{}{
		"general.architecture":                      "inkling2",
		"inkling2.block_count":                      uint32(48),
		"inkling2.attention.head_count":             uint32(24),
		"inkling2.attention.head_count_kv":          kvHeads,
		"inkling2.embedding_length":                 uint32(2304),
		"inkling2.context_length":                   uint32(1048576),
		"inkling2.attention.key_length":             uint32(128),
		"inkling2.attention.value_length":           uint32(128),
		"inkling2.attention.key_length_swa":         uint32(128),
		"inkling2.attention.value_length_swa":       uint32(128),
		"inkling2.attention.sliding_window":         uint32(1024),
		"inkling2.attention.sliding_window_pattern": pattern,
	})

	info, err := feasibleBackendR707(t, dir, "f16", 7097, 8191).ComputeFeasible("inkling")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	if info.KVPlanSource != KVPlanSourcePlan || !info.KVPlanExact {
		t.Fatalf("KVPlanSource = %q exact = %v, want plan/true", info.KVPlanSource, info.KVPlanExact)
	}
	if info.KVLayers != 48 || info.KVGlobalLayers != 8 || info.KVSWALayers != 40 || info.KVSWAWindow != 1024 {
		t.Fatalf("слои: всего %d g=%d swa=%d win=%d, want 48/8/40/1024",
			info.KVLayers, info.KVGlobalLayers, info.KVSWALayers, info.KVSWAWindow)
	}
	if info.KVGlobalKVHeads != 8 || info.KVSWAKVHeads != 8 {
		t.Errorf("KV-головы: g=%d swa=%d, want 8/8", info.KVGlobalKVHeads, info.KVSWAKVHeads)
	}
	// KV на токен совпадает с плоским (ширины классов равны), но ПОТОЛОК окна — нет.
	if want := (128 + 128) * 48 * 8 * 2; info.KVPerToken != want {
		t.Fatalf("KVPerToken = %d, want %d", info.KVPerToken, want)
	}
	budget := budgetBytesR707(t, 7097, 0, computeBufferReserveMB(0))
	globalPerToken := int64(8 * 8 * (128 + 128) * 2)
	fixed := int64(40*8*(128+128)*2) * 1024
	want := int((budget - fixed) / globalPerToken)
	if info.MaxVRAMCtx != want {
		t.Fatalf("MaxVRAMCtx = %d, want %d (после окна растут только 8 глобальных слоёв)", info.MaxVRAMCtx, want)
	}
	if flatMax := int(budget) / ((128 + 128) * 48 * 8 * 2); info.MaxVRAMCtx < flatMax*5 {
		t.Errorf("MaxVRAMCtx = %d не вырос относительно плоской оценки %d", info.MaxVRAMCtx, flatMax)
	}
}

// TestR707_PlanFieldsOnWire — поля раскладки для /api/models и `-feasible`.
//
// Проверяется И содержимое (карта имён провода), И то, что обе ручки его
// используют: переименование/забытое поле молча вернёт балансер к плоской
// формуле — ровно этот класс дефекта уже был в R660 (геометрия считалась, но не
// публиковалась) и R45 (per-model feasible парсился и выбрасывался).
func TestR707_PlanFieldsOnWire(t *testing.T) {
	dir := t.TempDir()
	writeGGUFR707(t, dir, "gemma-4-E4B-it-Q4_K_M.gguf", gemma4KeysR707())
	b := feasibleBackendR707(t, dir, "f16", 7097, 8191)

	plan, ok := b.KVPlanForModel("gemma-4-E4B-it-Q4_K_M")
	if !ok {
		t.Fatal("KVPlanForModel не нашёл раскладку по метаданным")
	}
	fields := plan.WireKVPlanFieldsR707()
	for k, want := range map[string]interface{}{
		"kv_plan_source":   "plan",
		"kv_plan_exact":    true,
		"kv_layers":        24,
		"kv_global_layers": 4,
		"kv_swa_layers":    20,
		"kv_swa_window":    512,
		"head_dim_swa":     256,
		"head_dim_swa_v":   256,
		"n_kv_heads_swa":   2,
	} {
		if got, ok := fields[k]; !ok || got != want {
			t.Errorf("поле %q = %v (есть=%v), want %v", k, got, ok, want)
		}
	}

	// Сторож провода: воркер публикует, CLI печатает, балансерный поллер читает.
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"../../cmd/cppworker/handlers_model_list.go", []string{"backend.KVPlanForModel(m.Name)", "WireKVPlanFieldsR707"}},
		{"../../cmd/cppworker/cli_feasible.go", []string{"feasibleKVPlanLineR707", "KV plan:"}},
		{"../../internal/balancer/llamacpp_metrics_poller.go", []string{
			`kv_plan_source`, `kv_global_layers`, `kv_swa_layers`, `kv_swa_window`,
			`head_dim_swa`, `head_dim_swa_v`, `n_kv_heads_swa`,
		}},
	} {
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("не читается %s: %v", tc.path, err)
		}
		flat := collapseSpacesR707(string(raw))
		for _, want := range tc.want {
			if !strings.Contains(flat, want) {
				t.Errorf("%s: нет %q — поле раскладки потеряется на проводе", tc.path, want)
			}
		}
	}
}

// collapseSpacesR707 — схлопнуть пробелы/переводы строк для поиска подстрок.
func collapseSpacesR707(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestR707_LoopSchemaExpandsVisitsToPhysicalLayers — loop-схема: физических
// блоков block_count, а слотов кэша больше (визиты цикла), и параметры каждого
// визита берутся по физическому слою.
//
// Правило llama.cpp: n_slot = n_phys + (n_passes−1)·(core_end−core_start), а
// layer_phys(il) = il при il < n_phys иначе core_start + (il−n_phys) % (core_end−core_start)
// (c/llama.cpp/src/models/inkling2-loop.cpp:3-5,16-30; каждый визит держит СВОЙ KV).
func TestR707_LoopSchemaExpandsVisitsToPhysicalLayers(t *testing.T) {
	pattern := make([]bool, 0, 24)
	for i := 0; i < 4; i++ {
		pattern = append(pattern, true, true, true, true, true, false)
	}
	dir := t.TempDir()
	writeGGUFR707(t, dir, "inkling-loop.gguf", map[string]interface{}{
		"general.architecture":                           "inkling2_loop",
		"inkling2_loop.block_count":                      uint32(24),
		"inkling2_loop.attention.head_count":             uint32(24),
		"inkling2_loop.attention.head_count_kv":          uint32(8),
		"inkling2_loop.embedding_length":                 uint32(2304),
		"inkling2_loop.context_length":                   uint32(1048576),
		"inkling2_loop.attention.key_length":             uint32(128),
		"inkling2_loop.attention.value_length":           uint32(128),
		"inkling2_loop.attention.sliding_window":         uint32(1024),
		"inkling2_loop.attention.sliding_window_pattern": pattern,
		"inkling2_loop.num_loops":                        uint32(3),
		"inkling2_loop.loop.core_start":                  uint32(6),
		"inkling2_loop.loop.core_end":                    uint32(18),
	})

	h, err := ReadGGUFHeader(filepath.Join(dir, "inkling-loop.gguf"))
	if err != nil {
		t.Fatalf("ReadGGUFHeader: %v", err)
	}
	plan := h.KVPlan()
	// 24 + (3−1)·(18−6) = 48 слотов; глобальные визиты — те, чей физический слой
	// глобальный (индексы 5, 11, 17, 23 → 4 физических, 8 визитов).
	if h.NLayers != 24 {
		t.Fatalf("NLayers = %d, want 24 (физические блоки)", h.NLayers)
	}
	if plan.KVLayers() != 48 {
		t.Fatalf("KVLayers = %d, want 48 визитов", plan.KVLayers())
	}
	if plan.GlobalLayers != 8 || plan.SWALayers != 40 {
		t.Errorf("классы: глобальных %d, оконных %d — want 8/40 (по физическим слоям)",
			plan.GlobalLayers, plan.SWALayers)
	}
	if plan.Source != KVPlanSourcePlan || !plan.Exact {
		t.Errorf("Source = %q exact = %v, want plan/true", plan.Source, plan.Exact)
	}
	// Визитов вдвое больше физических блоков, поэтому и KV на токен вдвое больше
	// плоской оценки по 24 блокам: 48 · 8 · (128+128) · 2 = 196 608 против 98 304.
	if got, want := plan.KVPerTokenBytes("f16"), int64((128+128)*48*8*2); got != want {
		t.Fatalf("KV на токен = %d, want %d (48 визитов)", got, want)
	}
	if flat := int64((128 + 128) * 24 * 8 * 2); plan.KVPerTokenBytes("f16") != flat*2 {
		t.Errorf("KV на токен = %d, want ровно вдвое больше плоского %d — визиты потеряны",
			plan.KVPerTokenBytes("f16"), flat)
	}
}
