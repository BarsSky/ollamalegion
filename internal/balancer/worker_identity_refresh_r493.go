// worker_identity_refresh_r493.go — R493-12i, вторая половина (2026-11-12):
// ИДЕНТИЧНОСТЬ ВОРКЕРА ДОЛЖНА БЫТЬ СВЕЖЕЙ, а не «та, что была при регистрации».
//
// ЖИВОЙ ДЕФЕКТ (локальный стенд, 2026-11-12). После пересборки воркера
// `/api/v1/backends` показывал `workerVersion=261110-1925`, `workerCommit=9d42a329`,
// хотя образ был `gpu-r261112-2146` / `32153c14`. Причина: идентичность пишется в
// `/app/data/config.json` в момент регистрации, а регистрация у воркера —
// ОДНОРАЗОВАЯ (при старте). Любой перезапуск балансера поднимал запись из
// config.json, и в ней оставалась идентичность ПРЕДЫДУЩЕГО образа: узел пересобрали,
// а поле говорит «старое» — тот же ложный вывод «раскатали не то», только с другой
// стороны.
//
// РЕШЕНИЕ. Балансер и так опрашивает воркер каждые ~30 с (`/api/models`). Раз в
// TTL (2 мин) он дополнительно спрашивает `/api/info` — там лежат build-поля,
// запечённые в образ — и обновляет запись бэкенда. Один лишний GET на узел раз в
// две минуты: дешевле, чем оператор, который сверяет `docker inspect` глазами.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// workerIdentityTTLR493 — как часто переспрашивать идентичность. Две минуты:
// пересборка воркера случается раз в часы, а лишние запросы к GPU-узлу ни к чему.
const workerIdentityTTLR493 = 2 * time.Minute

// workerIdentityTimeoutR493 — сколько ждать ответ /api/info. Короткий таймаут:
// запрос идёт в фоне, и «узел молчит» не должно задерживать ни метрики, ни тесты.
const workerIdentityTimeoutR493 = 2 * time.Second

// workerIdentityR493 — то, что узел сообщает о своей сборке.
type workerIdentityR493 struct {
	Version   string
	Commit    string
	BuildDate string
	RPCProto  string
}

// identityRefreshR493 — TTL-состояние опроса идентичности (по бэкенду).
//
// Отдельная структура, а не поле поллера: у поллера уже есть mu для
// running/stopCh, и складывать туда постороннее состояние — путь к неочевидным
// блокировкам в горячем цикле.
type identityRefreshR493 struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (r *identityRefreshR493) due(backendID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]time.Time, 4)
	}
	if t, ok := r.last[backendID]; ok && now.Sub(t) < workerIdentityTTLR493 {
		return false
	}
	r.last[backendID] = now
	return true
}

// refreshWorkerIdentityR493 — спросить /api/info у воркера и обновить запись.
//
// Вызывается из цикла поллера (после /api/models): ошибки только логируются —
// идентичность не должна ломать опрос метрик.
func (p *llamaCppMetricsPoller) refreshWorkerIdentityR493(b backendInfo) {
	if p == nil || p.proxy == nil || !p.identityRefresh.due(b.id, time.Now()) {
		return
	}
	url := "http://" + b.host + ":" + itoaPortR493(b.port) + "/api/info"
	ctx, cancel := context.WithTimeout(context.Background(), workerIdentityTimeoutR493)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		logger.Get().Debugw("R493: идентичность воркера не получена",
			"backend", b.id, "url", url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Get().Debugw("R493: /api/info воркера вернул не-2xx",
			"backend", b.id, "url", url, "status", resp.StatusCode)
		return
	}
	var body struct {
		BuildVersion string `json:"build_version"`
		BuildCommit  string `json:"build_commit"`
		BuildDate    string `json:"build_date"`
		RPCProto     string `json:"rpc_proto"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		logger.Get().Debugw("R493: /api/info воркера не разобран",
			"backend", b.id, "error", err)
		return
	}
	ident := workerIdentityR493{
		Version:   strings.TrimSpace(body.BuildVersion),
		Commit:    strings.TrimSpace(body.BuildCommit),
		BuildDate: strings.TrimSpace(body.BuildDate),
		RPCProto:  strings.TrimSpace(body.RPCProto),
	}
	if ident.Version == "" && ident.Commit == "" && ident.BuildDate == "" && ident.RPCProto == "" {
		// Старый образ без build-полей: не трогаем запись, чтобы не стирать то,
		// что пришло регистрацией.
		return
	}
	p.proxy.applyWorkerIdentityR493(b.id, ident)
}

// applyWorkerIdentityR493 — записать свежую идентичность в состояние бэкенда.
// true = что-то изменилось (для лога: не шуметь каждые две минуты).
func (p *Proxy) applyWorkerIdentityR493(backendID string, ident workerIdentityR493) bool {
	if p == nil || backendID == "" {
		return false
	}
	p.mu.RLock()
	state := p.backends[backendID]
	p.mu.RUnlock()
	if state == nil || state.Backend == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	before := *state.Backend
	setNonEmptyR493(&state.Backend.WorkerVersion, ident.Version)
	setNonEmptyR493(&state.Backend.WorkerCommit, ident.Commit)
	setNonEmptyR493(&state.Backend.WorkerBuildDate, ident.BuildDate)
	setNonEmptyR493(&state.Backend.WorkerRPCProto, ident.RPCProto)
	// Источник: идентичность только что пришла от самого воркера — это факт,
	// а не «догадка по агенту». Поле объясняет, почему значениям можно верить.
	setNonEmptyR493(&state.Backend.WorkerIdentitySource, "worker-http")
	changed := before.WorkerVersion != state.Backend.WorkerVersion ||
		before.WorkerCommit != state.Backend.WorkerCommit ||
		before.WorkerBuildDate != state.Backend.WorkerBuildDate ||
		before.WorkerRPCProto != state.Backend.WorkerRPCProto ||
		before.WorkerIdentitySource != state.Backend.WorkerIdentitySource
	if changed {
		logger.Get().Infow("R493: идентичность воркера обновлена по /api/info",
			"backend", backendID,
			"was_version", before.WorkerVersion, "was_commit", before.WorkerCommit,
			"now_version", state.Backend.WorkerVersion, "now_commit", state.Backend.WorkerCommit)
	}
	return changed
}

// setNonEmptyR493 — записать значение, только если оно непустое.
func setNonEmptyR493(dst *string, value string) {
	if value != "" {
		*dst = value
	}
}

// itoaPortR493 — порт в строку (strconv в этом файле не тянем: пять цифр максимум).
func itoaPortR493(port int) string {
	if port <= 0 {
		return "0"
	}
	var buf [6]byte
	i := len(buf)
	for port > 0 {
		i--
		buf[i] = byte('0' + port%10)
		port /= 10
	}
	return string(buf[i:])
}
