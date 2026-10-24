package bridge

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// layers_plan.go — R118.16 (2026-10-14): ТОЧНАЯ раскладка слоёв по устройствам.
//
// ЗАЧЕМ. До этой правки WebUI показывал раскладку РАСЧЁТОМ: слои выводились из
// n_layers/gpu_layers/tensor_split по формуле, сверенной с логом llama.cpp на трёх
// живых прогонах (и помеченной в интерфейсе как «расчёт»). Точный список знает
// только сам llama.cpp — он печатает его в лог при загрузке:
//
//	load_tensors: layer  33 assigned to device CUDA0, is_swa = 1
//	load_tensors: layer  39 assigned to device RPC0, is_swa = 0
//	load_tensors: layer  42 assigned to device CPU, is_swa = 0
//
// Теперь эти строки перехватываются мостом (см. bridge.go: goBridgeLogLine) и
// складываются в план: устройство → непрерывный диапазон слоёв. Оператор видит
// ФАКТ, а не оценку.
//
// Файл намеренно БЕЗ build-тега: разбор строки — чистая функция, и она должна
// тестироваться обычным `go test -tags llama_stub` (без llama.cpp в окружении).

// deviceLayerRange — непрерывный диапазон слоёв на одном устройстве.
type deviceLayerRange struct {
	Device string `json:"device"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Count  int    `json:"count"`
	// Node — адрес узла раскладки для устройства RPC0/RPC1 (из строки
	// `load_tensors: RPC0[192.0.2.10:50052] model buffer size = ...`).
	// Пусто у локальных GPU и CPU. Заполняется фактом, а не порядком узлов:
	// список узлов балансер сортирует, и позиция RPC0 в нём — догадка.
	Node string `json:"node,omitempty"`
}

// DeviceLayerRange — то же самое для внешних пакетов (cppworker отдаёт это в
// /api/models и /config/runtime, балансер — в WebUI).
type DeviceLayerRange = deviceLayerRange

// ResetDeviceLayers — сбросить план перед загрузкой модели и поставить перехват
// лог-строк llama.cpp (перехват идемпотентен, см. bridge_install_log_capture).
//
// Порядок обязателен: сначала сброс, потом загрузка — иначе в план попадут слои
// ПРЕДЫДУЩЕЙ модели, и оператор увидит чужую раскладку.
func ResetDeviceLayers() {
	resetDeviceLayers()
	installLogCapture()
}

// DeviceLayers — план, собранный из лога ПОСЛЕДНЕЙ загрузки. Пустой результат
// означает «llama.cpp ещё не сообщил» (или сборка с llama_stub), а не «слоёв
// нет»: вызывающий сам решает, чем заменить неизвестное.
func DeviceLayers() []DeviceLayerRange {
	return deviceLayersSnapshot()
}


var (
	deviceLayersMu sync.Mutex
	// deviceLayersByLayer — слой → устройство (как сообщил llama.cpp).
	deviceLayersByLayer = map[int]string{}
	// deviceNodeByDevice — RPC0 → 192.0.2.10:50052 (адрес узла из лога).
	deviceNodeByDevice = map[string]string{}
)

// parseLayerAssignment — разобрать строку llama.cpp.
//
// Принимает варианты форматирования:
//
//	"load_tensors: layer  33 assigned to device CUDA0, is_swa = 1"
//	"load_tensors: layer 33 assigned to device RPC0"
//	"[bridge] load_tensors: layer 42 assigned to device CPU, is_swa = 0"
//
// Возвращает (номер слоя, устройство, ok).
func parseLayerAssignment(line string) (int, string, bool) {
	idx := strings.Index(line, "assigned to device")
	if idx < 0 {
		return 0, "", false
	}
	head := line[:idx]
	li := strings.LastIndex(head, "layer")
	if li < 0 {
		return 0, "", false
	}
	numStr := strings.TrimSpace(head[li+len("layer"):])
	num, err := strconv.Atoi(strings.TrimSpace(strings.TrimRight(numStr, " ,")))
	if err != nil {
		return 0, "", false
	}
	rest := strings.TrimSpace(line[idx+len("assigned to device"):])
	if rest == "" {
		return 0, "", false
	}
	// Устройство — до запятой/пробела с параметрами ("CUDA0, is_swa = 1").
	dev := rest
	if i := strings.IndexAny(dev, ", \t"); i >= 0 {
		dev = dev[:i]
	}
	dev = strings.TrimSpace(dev)
	if dev == "" {
		return 0, "", false
	}
	return num, dev, true
}

// parseRPCEndpoint — разобрать строку llama.cpp с адресом узла раскладки:
//
//	"load_tensors: RPC0[192.0.2.10:50052] model buffer size = 1251.07 MiB"
//
// Возвращает (устройство, адрес, ok). Адрес нужен, чтобы показать оператору
// «слои ушли на узел X» ФАКТОМ: список узлов в API отсортирован, и связывать
// RPC0 с первым узлом списка было бы догадкой.
func parseRPCEndpoint(line string) (string, string, bool) {
	open := strings.Index(line, "[")
	if open <= 0 {
		return "", "", false
	}
	rel := strings.Index(line[open:], "]")
	if rel <= 0 {
		return "", "", false
	}
	head := strings.TrimSpace(line[:open])
	if i := strings.LastIndexAny(head, " \t"); i >= 0 {
		head = head[i+1:]
	}
	if !strings.HasPrefix(strings.ToUpper(head), "RPC") {
		return "", "", false
	}
	endpoint := strings.TrimSpace(line[open+1 : open+rel])
	if head == "" || endpoint == "" {
		return "", "", false
	}
	return head, endpoint, true
}

// recordLayerAssignment — запомнить строку лога (вызывается из C-колбэка).
// Одна и та же функция разбирает и назначение слоя, и адрес узла: строки идут
// одним потоком, а лишний проход по строкам в потоке загрузки модели не нужен.
func recordLayerAssignment(line string) {
	if dev, endpoint, ok := parseRPCEndpoint(line); ok {
		deviceLayersMu.Lock()
		deviceNodeByDevice[dev] = endpoint
		deviceLayersMu.Unlock()
		return
	}
	layer, dev, ok := parseLayerAssignment(line)
	if !ok {
		return
	}
	deviceLayersMu.Lock()
	deviceLayersByLayer[layer] = dev
	deviceLayersMu.Unlock()
}

// resetDeviceLayers — сбросить план перед новой загрузкой модели.
func resetDeviceLayers() {
	deviceLayersMu.Lock()
	deviceLayersByLayer = map[int]string{}
	deviceNodeByDevice = map[string]string{}
	deviceLayersMu.Unlock()
}

// deviceLayersSnapshot — план в виде непрерывных диапазонов по устройствам,
// отсортированный по первому слою (порядок llama.cpp: локальные, затем RPC, затем
// CPU — но полагаться на порядок строк не будем).
func deviceLayersSnapshot() []deviceLayerRange {
	deviceLayersMu.Lock()
	cp := make(map[int]string, len(deviceLayersByLayer))
	for k, v := range deviceLayersByLayer {
		cp[k] = v
	}
	nodes := make(map[string]string, len(deviceNodeByDevice))
	for k, v := range deviceNodeByDevice {
		nodes[k] = v
	}
	deviceLayersMu.Unlock()
	if len(cp) == 0 {
		return nil
	}

	layers := make([]int, 0, len(cp))
	for l := range cp {
		layers = append(layers, l)
	}
	sort.Ints(layers)

	out := make([]deviceLayerRange, 0, 4)
	var cur deviceLayerRange
	for i, l := range layers {
		dev := cp[l]
		if i == 0 {
			cur = deviceLayerRange{Device: dev, From: l, To: l, Count: 1, Node: nodes[dev]}
			continue
		}
		if dev == cur.Device && l == cur.To+1 {
			cur.To = l
			cur.Count++
			continue
		}
		out = append(out, cur)
		cur = deviceLayerRange{Device: dev, From: l, To: l, Count: 1, Node: nodes[dev]}
	}
	out = append(out, cur)
	return out
}
