package balancer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpc_link_measure_r118.go — R118.15 (2026-10-14): замер пропускной способности
// канала до узлов раскладки СРЕДСТВАМИ ПРОДУКТА.
//
// ЗАЧЕМ. До этой правки число в панели («канал 889 Мбит/с») приходило только из
// переменных оператора LB_RPC_LINK_MBPS / _MEASURED_AT / _SOURCE: он запускал
// scripts/measure-network.ps1 руками и вписывал результат в .env. Число молча
// устаревало (сеть/канал менялись), а по нему оператор судит, потянет ли раскладка
// слои по сети.
//
// ЧТО ТЕПЕРЬ. Балансер сам периодически качает известный объём (по умолчанию 8 МиБ)
// с HTTP-порта cppworker'а КАЖДОГО узла раскладки (узел отдаёт его обработчиком
// /api/rpc-link-probe, cmd/cppworker/handlers_link_probe.go) и считает Мбит/с.
// Замер идёт по той же сети и до того же хоста, где живёт rpc-server раскладки.
//
// Операторское значение из .env остаётся fallback'ом: если узел недоступен или
// замер ещё не сделан, в панели показывается прежнее число с прежним источником —
// «измерено автоматически» никогда не выдаётся за факт, которого нет.
//
// Отключается LB_RPC_LINK_MEASURE_SEC=0; период по умолчанию 900 с (раз в 15 минут:
// 8 МиБ на узел — это доли секунды на 889 Мбит/с и не создаёт нагрузки).

// rpcLinkMeasurement — результат одного замера до узла.
type rpcLinkMeasurement struct {
	At    time.Time
	Node  string
	Mbps  int
	Error string
}

const (
	// rpcLinkProbeBytes — сколько качаем за один замер (обработчик разрешает
	// 64 КиБ…64 МиБ; 8 МиБ — компромисс между точностью и нагрузкой).
	rpcLinkProbeBytes = 8 << 20
	// rpcLinkMeasureDefaultSec — период замеров по умолчанию.
	rpcLinkMeasureDefaultSec = 900
	// rpcLinkMeasureTimeout — таймаут одного замера: медленный/недоступный узел
	// не должен держать горутину минутами.
	rpcLinkMeasureTimeout = 60 * time.Second
	// rpcLinkMeasureFreshFactor — во сколько периодов замера считаем его свежим.
	rpcLinkMeasureFreshFactor = 3
)

// RPCLinkMeasureInterval — период замеров из окружения (0 = выключено).
func RPCLinkMeasureInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_RPC_LINK_MEASURE_SEC"))
	if v == "" {
		return rpcLinkMeasureDefaultSec * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("LB_RPC_LINK_MEASURE_SEC is not a number, using default",
			"value", v, "default_sec", rpcLinkMeasureDefaultSec)
		return rpcLinkMeasureDefaultSec * time.Second
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// measureRPCLinkOnce — один замер до HTTP-порта бэкенда.
//
// Возвращает (Мбит/с, ошибка). Считаем по времени от старта запроса до последнего
// прочитанного байта: так в замер попадает и установка соединения, и передача.
func (p *Proxy) measureRPCLinkOnce(b *types.Backend) (int, error) {
	if p == nil || b == nil {
		return 0, fmt.Errorf("backend is nil")
	}
	host := strings.TrimSpace(b.Host)
	port := p.getBackendPort(b)
	if host == "" || port <= 0 {
		return 0, fmt.Errorf("backend %s: неизвестен адрес cppworker", b.ID)
	}

	url := fmt.Sprintf("http://%s:%d/api/rpc-link-probe?bytes=%d", host, port, rpcLinkProbeBytes)
	ctx, cancel := context.WithTimeout(context.Background(), rpcLinkMeasureTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if b.CppWorkerApiToken != "" {
		req.Header.Set("X-API-Token", b.CppWorkerApiToken)
	}
	client := &http.Client{Timeout: rpcLinkMeasureTimeout}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("узел %s:%d ответил HTTP %d (обработчик замера есть в сборке cppworker r118-probe1+)",
			host, port, resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(start)
	if elapsed <= 0 || n <= 0 {
		return 0, fmt.Errorf("пустой замер: %d байт за %s", n, elapsed)
	}
	mbps := int(float64(n) * 8 / elapsed.Seconds() / 1e6)
	if mbps <= 0 {
		return 0, fmt.Errorf("замер дал 0 Мбит/с (%d байт за %s)", n, elapsed)
	}
	return mbps, nil
}

// MeasureRPCLinkForBackend — замер до узла и запись результата (для API/панели).
//
// Вызывается фоновой петлёй и явным запросом оператора («Проверить канал»).
func (p *Proxy) MeasureRPCLinkForBackend(backendID string) (int, error) {
	if p == nil || backendID == "" {
		return 0, fmt.Errorf("backend id is empty")
	}
	b := p.GetBackend(backendID)
	if b == nil {
		return 0, fmt.Errorf("backend %s not found", backendID)
	}
	if strings.TrimSpace(b.Role) != types.BackendRoleDonor && strings.TrimSpace(b.Role) != types.BackendRoleMain {
		// Мерить имеет смысл только у узлов-участников раскладки.
		return 0, fmt.Errorf("backend %s не участвует в раскладке (role=%q)", backendID, b.Role)
	}

	mbps, err := p.measureRPCLinkOnce(b)

	p.mu.Lock()
	if p.rpcLinkMeasured == nil {
		p.rpcLinkMeasured = map[string]rpcLinkMeasurement{}
	}
	node := fmt.Sprintf("%s:%d", b.Host, p.getBackendPort(b))
	rec := rpcLinkMeasurement{At: time.Now().UTC(), Node: node, Mbps: mbps}
	if err != nil {
		rec.Error = err.Error()
		// Прежний успешный замер НЕ затираем: пусть в панели остаётся последняя
		// достоверная цифра, а причина видна в логе.
		if prev, ok := p.rpcLinkMeasured[backendID]; ok && prev.Mbps > 0 {
			rec.Mbps = prev.Mbps
		}
	}
	p.rpcLinkMeasured[backendID] = rec
	p.mu.Unlock()

	if err != nil {
		logger.Get().Warnw("RPCLinkMeasure: замер канала не удался",
			"backend", backendID, "node", node, "error", err)
		return 0, err
	}
	logger.Get().Infow("RPCLinkMeasure: канал измерен",
		"backend", backendID, "node", node, "mbps", mbps, "bytes", rpcLinkProbeBytes)
	return mbps, nil
}

// RPCLinkMeasured — последний замер для бэкенда: (Мбит/с, когда, свежий ли).
// Свежесть считается от периода замеров; при выключенных замерах — не свежий.
func (p *Proxy) RPCLinkMeasured(backendID string) (int, time.Time, bool) {
	if p == nil || backendID == "" {
		return 0, time.Time{}, false
	}
	p.mu.RLock()
	rec, ok := p.rpcLinkMeasured[backendID]
	p.mu.RUnlock()
	if !ok || rec.Mbps <= 0 {
		return 0, time.Time{}, false
	}
	period := RPCLinkMeasureInterval()
	if period <= 0 {
		return 0, time.Time{}, false
	}
	if time.Since(rec.At) > period*rpcLinkMeasureFreshFactor {
		return 0, time.Time{}, false
	}
	return rec.Mbps, rec.At, true
}

// StartRPCLinkMeasurer — фоновая петля замеров канала до узлов раскладки.
// Идемпотентна по stop-каналу: завершается при его закрытии.
func (p *Proxy) StartRPCLinkMeasurer(stop <-chan struct{}) {
	period := RPCLinkMeasureInterval()
	if p == nil || period <= 0 || stop == nil {
		return
	}
	go func() {
		// Первый замер — не сразу: узлы могли ещё не зарегистрироваться.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				for _, b := range p.GetAllBackends() {
					role := strings.TrimSpace(b.Role)
					if role != types.BackendRoleDonor && role != types.BackendRoleMain {
						continue
					}
					if _, err := p.MeasureRPCLinkForBackend(b.ID); err != nil {
						continue // причина уже в логе
					}
				}
				timer.Reset(period)
			}
		}
	}()
}
