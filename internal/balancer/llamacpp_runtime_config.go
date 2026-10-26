package balancer

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// handleRuntimeConfig — агрегирует реальные параметры загруженных моделей
// со всех llama.cpp бэкендов через cppworker-эндпоинт
// `/api/v1/cppworker/config/runtime`.
//
// Назначение: WebUI на вкладке GGUF Models показывает default-значения
// (из /api/v1/cppworker/config) и runtime-значения (из этого endpoint'а)
// рядом: "default: 8192, runtime: 32768 (gemma-4)". Это позволяет оператору
// видеть, что модель реально загружена с другими параметрами (например,
// после auto-reload через balancer), и при необходимости применить новые
// defaults через WebUI.
//
// Если бэкендов нет — возвращаем 200 с пустым массивом (не 503) —
// это состояние "всё ok, просто ничего не загружено".
func (lr *LlamaCppRouter) handleRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"loaded_models": []interface{}{},
			"count":         0,
			"backends":      []string{},
		})
		return
	}

	// Структура одного элемента runtime-конфига вынесена в область пакета
	// (runtimeModelR118) — чтобы регрессию декодирования tensor_split можно было
	// закрыть тестом: cppworker отдаёт ЕГО МАССИВОМ, и до R118 тип
	// map[string]interface{} ломал декодирование всего ответа.
	type runtimeModel = runtimeModelR118

	// Параллельно опрашиваем все llama.cpp бэкенды.
	type backendResult struct {
		backendID string
		models    []runtimeModel
		err       error
	}

	results := make(chan backendResult, len(backends))
	var wg sync.WaitGroup
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, b := range backends {
		wg.Add(1)
		go func(bi backendInfo) {
			defer wg.Done()
			url := fmt.Sprintf("http://%s:%d/api/v1/cppworker/config/runtime", bi.host, bi.port)
			resp, err := httpClient.Get(url)
			if err != nil {
				results <- backendResult{backendID: bi.id, err: fmt.Errorf("http get: %w", err)}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				results <- backendResult{
					backendID: bi.id,
					err:       fmt.Errorf("status %d", resp.StatusCode),
				}
				return
			}
			var parsed struct {
				LoadedModels []runtimeModel `json:"loaded_models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				results <- backendResult{backendID: bi.id, err: fmt.Errorf("decode: %w", err)}
				return
			}
			// Проставляем backend в каждой модели для агрегации.
			for i := range parsed.LoadedModels {
				parsed.LoadedModels[i].Backend = bi.id
			}
			results <- backendResult{backendID: bi.id, models: parsed.LoadedModels}
		}(b)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Собираем и дедуплицируем по path (если модель загружена на нескольких
	// бэкендах с одинаковым path — берём первый, кто ответил).
	merged := make([]runtimeModel, 0, len(backends)*2)
	seen := make(map[string]bool)
	backendIDs := make([]string, 0, len(backends))
	failedBackends := make(map[string]error)

	for res := range results {
		backendIDs = append(backendIDs, res.backendID)
		if res.err != nil {
			failedBackends[res.backendID] = res.err
			logger.Get().Warnw("handleRuntimeConfig: fetch failed",
				"backend", res.backendID, "error", res.err)
			continue
		}
		for _, m := range res.models {
			key := m.Path
			if key == "" {
				key = m.Name + "@" + res.backendID
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, m)
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Backend != merged[j].Backend {
			return merged[i].Backend < merged[j].Backend
		}
		return merged[i].Name < merged[j].Name
	})

	logger.Get().Infow("handleRuntimeConfig: returning aggregated runtime config",
		"models", len(merged),
		"backends_queried", len(backendIDs),
		"backends_failed", len(failedBackends),
	)

	resp := map[string]interface{}{
		"loaded_models": merged,
		"count":         len(merged),
		"backends":      backendIDs,
	}
	if len(failedBackends) > 0 {
		failList := make([]map[string]string, 0, len(failedBackends))
		for id, err := range failedBackends {
			failList = append(failList, map[string]string{
				"backend": id,
				"error":   err.Error(),
			})
		}
		resp["failed_backends"] = failList
	}

	// R118: раскладка слоёв + состояние канала — чтобы WebUI мог показать
	// «раскладка на донора: узел X, модель Y, слои Z».
	layout := lr.rpcLayoutInfo()
	resp["layout"] = layout
	for i := range merged {
		m := &merged[i]
		// R118.16: сначала ФАКТ из лога llama.cpp (его отдаёт cppworker), и
		// только если его нет — расчёт. null в device_layers для оператора
		// означает «неизвестно», и это честнее выдуманного плана.
		if plan, ok := layerPlanFromExactR118(m.ExactDeviceLayers, m.NLayers); ok {
			m.DeviceLayers = &plan
			continue
		}
		if !layout.Active {
			continue
		}
		plan := deriveLayerPlanR118(m.NLayers, m.GPULayers, m.TensorSplit, m.GPUCount, layout.Nodes)
		m.DeviceLayers = &plan
	}
	// План зависит от layout.Nodes, поэтому заполняем после сборки layout.
	resp["loaded_models"] = merged

	writeJSON(w, http.StatusOK, resp)
}

// runtimeModelR118 — один элемент runtime-конфига: то, что отдаёт cppworker
// `/api/v1/cppworker/config/runtime`, плюс имя бэкенда и раскладка слоёв.
//
// ВАЖНО про TensorSplit: cppworker отдаёт его МАССИВОМ чисел (доли по
// устройствам). До R118 здесь стоял map[string]interface{} — декодирование
// падало на первом же загруженном с раскладкой модели, и весь агрегированный
// ответ терял модели (WebUI показывал пустую runtime-строку). Регрессию держит
// TestR118_RuntimeModelDecodesTensorSplitArray.
type runtimeModelR118 struct {
	Name              string    `json:"name"`
	Path              string    `json:"path,omitempty"`
	State             string    `json:"state,omitempty"`
	Architecture      string    `json:"architecture,omitempty"`
	NLayers           int       `json:"n_layers"`
	NHeads            int       `json:"n_heads"`
	NKvHeads          int       `json:"n_kv_heads"`
	NEmbd             int       `json:"n_embd"`
	NVocab            int       `json:"n_vocab"`
	ContextSize       int       `json:"context_size"`
	GGUFContextLength int       `json:"gguf_context_length,omitempty"`
	SizeBytes         int64     `json:"size_bytes"`
	LoadedAt          string    `json:"loaded_at,omitempty"`
	GPUCount          int       `json:"gpu_count,omitempty"`
	GPULayers         int       `json:"gpu_layers"`
	TensorSplit       []float64 `json:"tensor_split,omitempty"`
	BatchSize         int       `json:"batch_size,omitempty"`
	FlashAttnType     int       `json:"flash_attn_type,omitempty"`
	NUMA              bool      `json:"numa,omitempty"`
	UseMmap           bool      `json:"use_mmap,omitempty"`
	// R134 (2026-10-16): ФАКТИЧЕСКИЕ параметры загрузки, которые cppworker уже
	// отдаёт (handlers_config.go, ключи ниже), но балансер их ТЕРЯЛ: он декодирует
	// ответ воркера в эту структуру и пересобирает свой ответ для WebUI, поэтому
	// поля, которых здесь нет, до интерфейса не доезжают.
	//
	// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА, которую это закрывает: «у загруженной модели стоит,
	// что размышление включено, но открыв карточку для изменения параметров, оно
	// стоит выключенным» — карточка брала намерение (профиль), потому что факта
	// загрузки в данных не было вовсе. R83/v67 добавил эти ключи в воркер именно
	// для того, чтобы оператор видел применённые настройки, но до WebUI они не
	// доходили из-за этой структуры.
	KVCacheType     string `json:"kv_cache_type,omitempty"`
	Parallel        int    `json:"parallel,omitempty"`
	ContextPerSeq   int    `json:"context_per_seq,omitempty"`
	BatchedParallel bool   `json:"batched_parallel,omitempty"`
	// FlashAttn — тип Flash Attention, как его отдаёт воркер вторым ключом
	// (`flash_attn`); -1 = авто, 0 = выключено, 1 = включено.
	FlashAttn int `json:"flash_attn,omitempty"`
	// ReasoningEnabled — режим reasoning ЗАГРУЖЕННОГО инстанса. Указатель, чтобы
	// «выключено» (false) отличалось от «неизвестно» (nil): размышления выключены —
	// это ЗНАЧЕНИЕ, и карточка обязана показать именно его.
	ReasoningEnabled *bool `json:"reasoning_enabled,omitempty"`
	ActiveQueries     int       `json:"active_queries,omitempty"`
	TotalQueries      int64     `json:"total_queries,omitempty"`
	LastUsedAt        string    `json:"last_used_at,omitempty"`
	Backend           string    `json:"backend"`
	// R118 (2026-10-14): раскладка слоёв по устройствам — «какие слои на доноре».
	//
	// R118.16 (2026-10-14): это ФАКТ, если cppworker отдал точный список
	// (device_layers_exact = строки `load_tensors: layer N assigned to device X`
	// из лога llama.cpp) — тогда Estimated=false. Расчёт по
	// n_layers/gpu_layers/tensor_split остаётся запасным вариантом для старых
	// сборок cppworker: точный список знает только llama.cpp.
	DeviceLayers *layerPlan `json:"device_layers,omitempty"`

	// ExactDeviceLayers — R118.16: точная раскладка от cppworker (факт из лога).
	// Отдельное имя ключа (`device_layers_exact`) — потому что эта же структура
	// ОТВЕЧАЕТ WebUI, а там `device_layers` занят объектом-планом выше: два поля
	// с одним json-тегом молча конфликтуют.
	ExactDeviceLayers []exactDeviceRangeR118 `json:"device_layers_exact,omitempty"`
}

// exactDeviceRangeR118 — R118.16: диапазон слоёв на устройстве, как его напечатал
// llama.cpp. Нумерация слоёв — как в логе: 0…n_layer, где последний номер —
// выходной слой (llama.cpp зовёт get_layer_buft_list для 0..n_layer_all).
type exactDeviceRangeR118 struct {
	Device string `json:"device"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Count  int    `json:"count"`
	Node   string `json:"node,omitempty"`
}

// deviceLayerRange — R118: сколько слоёв и какой диапазон получило устройство.
type deviceLayerRange struct {
	Device string `json:"device"`
	Kind   string `json:"kind"`           // local | rpc
	Node   string `json:"node,omitempty"` // адрес узла раскладки при kind=rpc
	Count  int    `json:"count"`
	From   int    `json:"from,omitempty"` // первый слой диапазона (1-based)
	To     int    `json:"to,omitempty"`   // последний слой диапазона (1-based)
}

// layerPlan — R118: раскладка слоёв модели по устройствам и на CPU.
type layerPlan struct {
	Devices   []deviceLayerRange `json:"devices,omitempty"`
	Total     int                `json:"total_layers"`
	GPU       int                `json:"gpu_layers"`
	CPU       int                `json:"cpu_layers"`
	Estimated bool               `json:"estimated"`
	Note      string             `json:"note,omitempty"`
}

// deriveLayerPlanR118 — R118 (2026-10-14): какие слои ушли на какое устройство.
//
// ПОЧЕМУ РАСЧЁТ, А НЕ ГОТОВЫЙ СПИСОК. Точную раскладку печатает сам llama.cpp
// в лог процесса (`load_tensors: layer N assigned to device X`), в API её нет.
// Здесь повторены его правила, и они СВЕРЕНЫ с живыми прогонами стенда:
//
//	42 слоя, tensor_split [0.537, 0.463], все слои на GPU →
//	    CUDA0 = 23, RPC0 = 19, CPU = 1          (наблюдалось: 23/19/1)
//	42 слоя, tensor_split [0.537, 0.463], gpu_layers = 10 →
//	    CUDA0 = 6 (33–38), RPC1 = 4 (39–42), CPU = 33   (наблюдалось: 6/4/33)
//
// Правила: GPU получает ПОСЛЕДНИЕ gpuLayers слоёв, они делятся между
// устройствами подряд по долям tensor_split с НАКОПИТЕЛЬНЫМ округлением вверх
// (cum_i = ceil(gpuLayers * Σp), count_i = cum_i − cum_{i−1}); на CPU остаётся
// nLayers − gpuLayers ПЛЮС выходной/нормализующий слой (+1 — именно поэтому в
// логе 33, а не 32). Порядок устройств: локальные GPU (CUDA0…), затем узлы
// раскладки (RPC0…, в порядке rpcServers).
//
// gpuLayers < 0 означает «все слои на GPU» (конвенция llama.cpp: -1).
func deriveLayerPlanR118(nLayers, gpuLayers int, split []float64, localGPUCount int, rpcNodes []string) layerPlan {
	if nLayers <= 0 {
		return layerPlan{Estimated: true, Note: "n_layers неизвестно"}
	}
	if gpuLayers < 0 {
		gpuLayers = nLayers
	}
	if gpuLayers > nLayers {
		gpuLayers = nLayers
	}

	plan := layerPlan{Total: nLayers, GPU: gpuLayers, Estimated: true}
	if gpuLayers == 0 {
		plan.CPU = nLayers
		plan.Note = "все слои на CPU (gpu_layers=0)"
		return plan
	}
	plan.CPU = nLayers - gpuLayers + 1 // +1 — выходной/нормализующий слой
	plan.Note = "расчёт по n_layers/gpu_layers/tensor_split; точный список — в логе cppworker " +
		"(load_tensors: layer N assigned to device X)"

	// Список устройств: локальные GPU, затем узлы раскладки.
	type dev struct {
		name string
		kind string
		node string
	}
	devs := make([]dev, 0, localGPUCount+len(rpcNodes))
	nLocal := localGPUCount
	if nLocal <= 0 {
		nLocal = 1
	}
	if len(rpcNodes) == 0 && len(devs) == 0 {
		// Локальный режим: одно логическое CUDA-устройство.
		devs = append(devs, dev{name: "CUDA0", kind: "local"})
	} else {
		for i := 0; i < nLocal; i++ {
			devs = append(devs, dev{name: fmt.Sprintf("CUDA%d", i), kind: "local"})
		}
	}
	for i, node := range rpcNodes {
		devs = append(devs, dev{name: fmt.Sprintf("RPC%d", i), kind: "rpc", node: node})
	}

	// Доли: tensor_split, а при его отсутствии — поровну.
	props := make([]float64, len(devs))
	sum := 0.0
	for i := range devs {
		if i < len(split) && split[i] > 0 {
			props[i] = split[i]
		}
		sum += props[i]
	}
	if sum <= 0 {
		for i := range props {
			props[i] = 1
		}
		sum = float64(len(props))
	}

	// Первый слой, попавший на GPU (нумерация 1-based, GPU — последние слои).
	firstGPU := nLayers - gpuLayers + 1
	assigned := 0
	prevCum := 0
	for i, d := range devs {
		cum := prevCum
		if i == len(devs)-1 {
			cum = gpuLayers // остаток — последнему устройству
		} else {
			acc := 0.0
			for k := 0; k <= i; k++ {
				acc += props[k]
			}
			cum = int(math.Ceil(float64(gpuLayers) * acc / sum))
		}
		if cum < prevCum {
			cum = prevCum
		}
		count := cum - prevCum
		prevCum = cum
		if count <= 0 && i != len(devs)-1 {
			continue
		}
		assigned += count
		plan.Devices = append(plan.Devices, deviceLayerRange{
			Device: d.name,
			Kind:   d.kind,
			Node:   d.node,
			Count:  count,
			From:   firstGPU + assigned - count,
			To:     firstGPU + assigned - 1,
		})
	}
	return plan
}

// layerPlanFromExactR118 — R118.16 (2026-10-14): план из ФАКТА — строк лога
// llama.cpp, которые перехватил мост cppworker'а.
//
// ЗАЧЕМ. Расчёт (deriveLayerPlanR118) сверен с логом, но остаётся расчётом: он
// не знает ни реальных долей tensor_split, которые применил llama.cpp, ни того,
// что выходной слой уехал на GPU. Оператор просил видеть ФАКТ и сверять его с
// `docker logs`.
//
// ПРОВЕРКА ЦЕЛОСТНОСТИ. llama.cpp печатает ровно n_layer + 1 назначение
// (llama-model.cpp: цикл по 0..n_layer_all плюс выходной слой). Если сумма
// диапазонов другая — это либо не наша модель (при двух одновременных загрузках
// план в мосте один на процесс), либо llama.cpp сменил формат. В обоих случаях
// честнее вернуть false и показать расчёт, чем чужой факт.
//
// ВАЖНО про Total: здесь это число назначений из лога = n_layer + 1, потому что
// GPU и CPU в сумме дают ровно его. У расчётного плана Total — число блоков
// модели (CPU-слой в него не входит), поэтому Total у них может отличаться на 1.
func layerPlanFromExactR118(ranges []exactDeviceRangeR118, nLayers int) (layerPlan, bool) {
	if len(ranges) == 0 || nLayers <= 0 {
		return layerPlan{}, false
	}
	total := 0
	for _, r := range ranges {
		if r.Device == "" || r.Count <= 0 {
			return layerPlan{}, false
		}
		total += r.Count
	}
	if total != nLayers+1 {
		return layerPlan{}, false
	}

	plan := layerPlan{
		Total:     total,
		Estimated: false,
		Note: "точный список из лога llama.cpp (load_tensors: layer N assigned to device X); " +
			"нумерация слоёв как в логе — 0…n_layer, последний номер это выходной слой",
	}
	for _, r := range ranges {
		if strings.EqualFold(r.Device, "CPU") {
			plan.CPU += r.Count
			continue
		}
		kind := "local"
		if strings.HasPrefix(strings.ToUpper(r.Device), "RPC") {
			kind = "rpc"
		}
		plan.Devices = append(plan.Devices, deviceLayerRange{
			Device: r.Device,
			Kind:   kind,
			Node:   r.Node,
			Count:  r.Count,
			From:   r.From,
			To:     r.To,
		})
	}
	plan.GPU = plan.Total - plan.CPU
	return plan, true
}

// rpcLayoutInfo — R118: состояние раскладки слоёв и канала до узлов.
//
// Берётся из метрик бэкендов (их каждые 30 с наполняет poller из /api/models
// cppworker: rpc_enabled, rpc_servers_active, vram_*), поэтому отдельного
// опроса не требуется. Пропускная способность канала — ЗАМЕР ОПЕРАТОРА
// (scripts/measure-network.ps1): задаётся LB_RPC_LINK_MBPS, потому что
// измерить её из процесса можно только отдельной пробой, а не догадкой.
func (lr *LlamaCppRouter) rpcLayoutInfo() rpcLayout {
	out := rpcLayout{LinkSource: strings.TrimSpace(os.Getenv("LB_RPC_LINK_SOURCE"))}
	if out.LinkSource == "" {
		out.LinkSource = "scripts/measure-network.ps1"
	}
	if v := strings.TrimSpace(os.Getenv("LB_RPC_LINK_MBPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.LinkMbps = n
		}
	}
	out.LinkMeasuredAt = strings.TrimSpace(os.Getenv("LB_RPC_LINK_MEASURED_AT"))

	if lr == nil || lr.proxy == nil {
		return out
	}
	for _, b := range lr.getLlamaCppBackends() {
		// Раскладка живёт в llamaMetrics (её наполняет poller из /api/models),
		// а не в BackendMetrics.
		//
		// R245: читаем СНАПШОТ. Раньше поля читались после RUnlock по живому
		// указателю, пока поллер переписывал их под Lock — data race (-race в CI).
		m, ok := lr.proxy.metricsMgr.SnapshotLlamaCppMetrics(b.id)
		if !ok || m == nil || !m.RPCEnabled {
			continue
		}
		out.Active = true
		out.Nodes = append(out.Nodes, m.RPCServersActive...)
		out.RemoteNodes = m.VramRemoteNodes
		out.VramLocalFreeMB = m.VramLocalFreeMB
		out.VramRemoteFreeMB = m.VramRemoteFreeMB
		// cppworker отдаёт vram_layout_free_mb как сумму локальной и узлов
		// (проверено живьём: 7097 + 7033 = 14130) — считаем так же.
		out.VramLayoutFreeMB = m.VramLocalFreeMB + m.VramRemoteFreeMB
	}
	sort.Strings(out.Nodes)
	return out
}

// rpcLayout — R118: блок «раскладка» для WebUI.
type rpcLayout struct {
	Nodes            []string `json:"nodes,omitempty"`
	LinkMeasuredAt   string   `json:"link_measured_at,omitempty"`
	LinkSource       string   `json:"link_source,omitempty"`
	VramLocalFreeMB  uint64   `json:"vram_local_free_mb,omitempty"`
	VramRemoteFreeMB uint64   `json:"vram_remote_free_mb,omitempty"`
	VramLayoutFreeMB uint64   `json:"vram_layout_free_mb,omitempty"`
	LinkMbps         int      `json:"link_mbps,omitempty"`
	RemoteNodes      int      `json:"remote_nodes,omitempty"`
	Active           bool     `json:"active"`
}
