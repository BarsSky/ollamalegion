// cold_load_probe.go — R663 (2026-11-20): живая проверка кандидата ПЕРЕД
// загрузкой «вместо ожидания».
//
// ЗАЧЕМ. R659 решает, где поднять модель, по КЭШУ метрик (поллер раз в 30 с) и по
// гейту «окно влезает». Живая проверка на двухузловом стенде показала два класса
// лишних `POST /load`:
//
//  1. узел уже поднял модель (R659 сам её и загрузил), но кэш ещё не обновился —
//     балансер просит загрузку повторно (воркер отвечает 202 и делает no-op, то
//     есть работа впустую и шум в логе воркера);
//  2. узел объявлен ДОНОРОМ раскладки (`CPPWORKER_RPC_SERVER_ENABLED=true`): такой
//     узел по определению не держит собственных копий и отвечает
//     `409 donor_cannot_host_models`. Балансеру это не видно ни из регистрации, ни
//     из кэша метрик — видно только в живом `/api/models` (поля role / donor /
//     can_load_own_models). На стенде это был реальный блокер: R659 «выбирал» узел,
//     а загрузка отклонялась.
//
// ЧТО ДЕЛАЕТ ПРОБА. Один GET `/api/models` кандидата (таймаут 1.5 с, результат
// кэшируется на 10 с): есть ли модель (loaded/loading) и разрешает ли узел себе
// собственные копии. Если ответа нет — решение остаётся за кэшем метрик, то есть
// поведение прежнее (живая проверка не должна становиться новым источником отказов).
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

const (
	// coldLoadProbeTimeout — LAN-запрос к воркеру, не полезная работа: ждём меньше,
	// чем собственная загрузка модели, чтобы не задерживать заявку.
	coldLoadProbeTimeout = 1500 * time.Millisecond
	// coldLoadProbeCacheTTL — сколько верим живому ответу. 10 с — компромисс: кэш
	// метрик обновляется раз в 30 с, а загрузку R659 запускает сам, поэтому
	// «протухнуть между проверками» ответ успевает только на секунды.
	coldLoadProbeCacheTTL = 10 * time.Second
	// coldLoadRetryAfterFailedR659 — сколько ждём после попытки, которая по живой
	// проверке НЕ подхватилась (модели нет и она не грузится), прежде чем просить
	// загрузку снова. Меньше прежних 120 с: отказ узла (например, донор) больше не
	// блокирует повторы на две минуты, а настоящая загрузка успевает перейти в
	// состояние loading и попасть под дедупликацию.
	coldLoadRetryAfterFailedR659 = 20 * time.Second
)

// coldLoadProbeR659 — что узел говорит о СЕБЕ прямо сейчас.
type coldLoadProbeR659 struct {
	// Reason — почему грузить не надо ("" = можно грузить).
	Reason string
	// ModelPresent — модель есть на узле (loaded или loading).
	ModelPresent bool
	// CanLoadOwn — узел разрешает себе собственные копии (false = донор раскладки).
	CanLoadOwn bool
	// Known — ответ получен и разобран; false = живую проверку выполнить нельзя.
	Known bool
}

// probeColdLoadCandidateR659 — живой ответ узла, с кэшем на coldLoadProbeCacheTTL.
func (p *Proxy) probeColdLoadCandidateR659(backendID, model string) coldLoadProbeR659 {
	if p == nil || backendID == "" || model == "" {
		return coldLoadProbeR659{}
	}
	now := time.Now()
	if probe, ok := p.coldLoadStateR659.cachedProbeR659(backendID, model, now); ok {
		return probe
	}
	probe := p.fetchColdLoadProbeR659(backendID, model)
	p.coldLoadStateR659.putProbeR659(backendID, model, now, probe)
	return probe
}

// fetchColdLoadProbeR659 — один GET /api/models кандидата без ретраев.
func (p *Proxy) fetchColdLoadProbeR659(backendID, model string) (probe coldLoadProbeR659) {
	backend := p.GetBackend(backendID)
	if backend == nil {
		return probe
	}
	port := p.getBackendPort(backend)
	if port <= 0 || backend.Host == "" {
		return probe
	}
	url := fmt.Sprintf("http://%s:%d/api/models", backend.Host, port)
	client := &http.Client{Timeout: coldLoadProbeTimeout}
	req, reqErr := http.NewRequest("GET", url, nil)
	if reqErr != nil {
		return probe
	}
	// Метка источника: воркер видит в логе, что это проба размещения, а не опрос
	// метрик (два разных потребителя одного /api/models).
	req.Header.Set("X-R659-Probe", "cold-load")
	resp, err := client.Do(req)
	if err != nil {
		if logger.Get() != nil {
			logger.Get().Debugw("R663: живая проверка кандидата недоступна — решает кэш метрик",
				"backend", backendID, "model", model, "error", err)
		}
		return probe
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probe
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return probe
	}
	var data struct {
		Models []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"models"`
		CanLoadOwnModels *bool  `json:"can_load_own_models"`
		Role             string `json:"role"`
		Donor            bool   `json:"donor"`
		DonorReason      string `json:"donor_reason"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return probe
	}
	probe.Known = true
	probe.CanLoadOwn = true
	if data.CanLoadOwnModels != nil {
		probe.CanLoadOwn = *data.CanLoadOwnModels
	} else if data.Donor {
		probe.CanLoadOwn = false
	}
	for _, m := range data.Models {
		if !modelNameMatches(m.Name, model) {
			continue
		}
		probe.ModelPresent = true
		probe.Reason = "модель уже загружена на узле"
		if strings.EqualFold(strings.TrimSpace(m.State), "loading") {
			probe.Reason = "модель уже загружается на узле"
		}
		break
	}
	if probe.ModelPresent && probe.CanLoadOwn {
		// Модель есть и грузить нечего: причина уже названа выше.
		return probe
	}
	if !probe.CanLoadOwn {
		reason := data.DonorReason
		if reason == "" {
			reason = "role=" + strings.TrimSpace(data.Role)
		}
		probe.Reason = "узел не держит собственных копий модели (донор раскладки: " + reason + ")"
	}
	return probe
}
