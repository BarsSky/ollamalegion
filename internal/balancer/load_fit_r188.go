// load_fit_r188.go — R188 (2026-10-20): ОТКАЗ ДО ЗАГРУЗКИ, если модель заведомо
// не влезает в свободную память бэкенда.
//
// ЖИВОЙ РАЗБОР (заметки оператора). Балансер ставил тяжёлую модель на бэкенд, где
// уже жила другая модель, и запускал загрузку, которая заведомо не могла
// поместиться. Снаружи это выглядело так:
//   - «загрузил модель к бэкенду с загруженной моделью»;
//   - «попытался поднять на другом бэкенде тяжёлую модель, но из-за места не смог»;
//   - слот и очередь занимались на десятки минут, клиент (Cline) терял ход.
//
// Причина: выбор бэкенда (llamacpp_backend_helpers.go, шаг «файл модели есть на
// диске») не смотрит на свободную память, а сама загрузка стартует без
// предварительной проверки. Здесь — проверка ПЕРЕД загрузкой по данным, которые
// воркер уже отдаёт: /api/models сообщает available_ram_mb / available_vram_mb и
// (если раскладка включена) свободную память узлов.
//
// Правило: если размер модели больше суммарной свободной памяти — загрузку НЕ
// начинаем, отдаём терминальный отказ с числами и что делать. Всё, что не
// доказано (нет размера модели, нет ответа воркера, size=0) — НЕ блокируем:
// предпроверка не должна становиться новым источником отказов.
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// loadFitProbeTimeout — предпроверка, а не полезная работа.
const loadFitProbeTimeout = 2500 * time.Millisecond

// workerCapacity — то, что нам нужно из /api/models про свободную память.
type workerCapacity struct {
	AvailableRAMMB     int64    `json:"available_ram_mb"`
	AvailableVRAMMB    int64    `json:"available_vram_mb"`
	TotalRAMMB         int64    `json:"total_ram_mb"`
	TotalVRAMMB        int64    `json:"total_vram_mb"`
	VRAMLayoutFreeMB   int64    `json:"vram_layout_free_mb"`
	VRAMLocalFreeMB    int64    `json:"vram_local_free_mb"`
	VRAMRemoteFreeMB   int64    `json:"vram_remote_free_mb"`
	VRAMRemoteNodes    int64    `json:"vram_remote_nodes"`
	VRAMLayoutTotalMB  int64    `json:"vram_layout_total_mb"`
	RPCEnabled         bool     `json:"rpc_enabled"`
	RPCServersActive   []string `json:"rpc_servers_active"`
	AvailableRAMKnown  bool     `json:"-"`
	AvailableVRAMKnown bool     `json:"-"`
}

// capacityForLoad — свободная память бэкенда по его же /api/models.
//
// ok=false, если ответа нет или воркер старый (полей нет) — тогда проверку
// влезания не делаем.
func (p *Proxy) capacityForLoad(host string, port int, token string) (workerCapacity, bool) {
	var cap workerCapacity
	if host == "" || port <= 0 {
		return cap, false
	}
	url := fmt.Sprintf("http://%s:%d/api/models", host, port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return cap, false
	}
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	client := &http.Client{Timeout: loadFitProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return cap, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cap, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cap, false
	}
	if err := json.Unmarshal(raw, &cap); err != nil {
		return cap, false
	}
	if cap.AvailableRAMMB <= 0 && cap.AvailableVRAMMB <= 0 && cap.VRAMLayoutFreeMB <= 0 {
		return cap, false
	}
	cap.AvailableRAMKnown = cap.AvailableRAMMB > 0
	cap.AvailableVRAMKnown = cap.AvailableVRAMMB > 0 || cap.VRAMLayoutFreeMB > 0
	return cap, true
}

// freeCapacityMB — сколько всего свободно (VRAM локальная + узлы раскладки + RAM).
func (c workerCapacity) freeCapacityMB() int64 {
	vram := c.AvailableVRAMMB
	if c.VRAMLayoutFreeMB > vram {
		vram = c.VRAMLayoutFreeMB
	}
	return c.AvailableRAMMB + vram
}

// loadFitRefusal — нужно ли отказать в загрузке по нехватке памяти.
//
// Возвращает (отказать, текст для клиента). Пустой/неизвестный размер модели
// отказа не даёт: без факта мы не имеем права блокировать загрузку.
func (p *Proxy) loadFitRefusal(backendID, modelName, host string, port int, token string) (bool, string) {
	if p == nil || modelName == "" {
		return false, ""
	}
	sizeBytes := p.getModelSizeBytes(modelName)
	if sizeBytes <= 0 {
		return false, ""
	}
	cap, ok := p.capacityForLoad(host, port, token)
	if !ok {
		return false, ""
	}
	sizeMB := int64(sizeBytes / (1024 * 1024))
	free := cap.freeCapacityMB()
	// Запас 5%: свободная память меняется между опросом и загрузкой, и ложный
	// отказ на границе хуже, чем попытка (её и так делает воркер).
	limit := free - free/20
	if sizeMB <= limit {
		if logger.Get() != nil {
			logger.Get().Debugw("load fit check: модель влезает",
				"backend", backendID, "model", modelName,
				"size_mb", sizeMB, "free_mb", free,
				"ram_mb", cap.AvailableRAMMB, "vram_mb", cap.AvailableVRAMMB,
				"layout_free_mb", cap.VRAMLayoutFreeMB)
		}
		return false, ""
	}
	reason := fmt.Sprintf(
		"модель %s (%d МБ) не влезает в свободную память бэкенда %s: свободно %d МБ "+
			"(RAM %d МБ, VRAM %d МБ, раскладка %d МБ при %d узлах). Загрузка НЕ запущена: "+
			"выгрузите другую модель на этом бэкенде, уменьшите контекст/квант или добавьте узел раскладки [%s]",
		modelName, sizeMB, backendID, free,
		cap.AvailableRAMMB, cap.AvailableVRAMMB, cap.VRAMLayoutFreeMB, cap.VRAMRemoteNodes,
		loadFitRefusalCode)
	return true, reason
}

// loadFitRefusalCode — машинный код отказа по нехватке памяти.
const loadFitRefusalCode = "model_does_not_fit"

// LoadFitRefusalCode — машинный код отказа по нехватке памяти (для API-слоя).
const LoadFitRefusalCode = loadFitRefusalCode

// IsLoadFitRefusalText — текст ошибки означает терминальный отказ «не влезает».
func IsLoadFitRefusalText(s string) bool {
	return strings.Contains(s, loadFitRefusalCode)
}

// rpcLayoutActive — у бэкенда есть АКТИВНЫЕ узлы раскладки (мастер+донор).
//
// ЗАЧЕМ (R191, 2026-10-20). Живой случай: загрузка gemma с раскладкой на донора
// (RPC0 = 19 слоёв) заняла больше окна ожидания, балансер отдал клиенту
// `500 "model not ready after 2m26.408s"` — хотя модель догрузилась и работала
// (layout_degraded=false). Оценка загрузки от воркера не учитывает сетевой узел:
// к времени загрузки добавляется передача весов и инициализация RPC.
//
// Возвращает true, если раскладка активна (тогда окно ожидания расширяется).
func (p *Proxy) rpcLayoutActive(host string, port int, token string) bool {
	cap, ok := p.capacityForLoad(host, port, token)
	if !ok {
		return false
	}
	return cap.RPCEnabled && len(cap.RPCServersActive) > 0
}
