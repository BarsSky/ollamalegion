// stage_r477.go — R477 (2026-11-09): на какой СТАДИИ сломался ход.
//
// ЗАЧЕМ. Диагностика уже умеет связать ход по request_id (R476), но по логу
// нельзя было ответить на главный вопрос: «какой компонент дал сбой». Строка
// request trace показывала код ответа и длительность, но не стадию, на которой
// запрос умер: выбор бэкенда, очередь, preflight окна, апстрим, стрим, запись
// клиенту. Из-за этого живые жалобы («токены генерировались, ответ не дошёл»)
// разбирались догадками.
//
// ЧТО ЗДЕСЬ. Трекер стадии в контексте запроса + кольцо последних сбоев.
// Стадия ставится в точках, которые уже существуют в пути запроса; при выходе
// строка request trace печатает stage, а для ответов 5xx и паник — failed_stage
// и reason. Последние сбои доступны в /api/v1/diag/summary (recent_failures) и
// в архиве (/api/v1/diag/bundle, файл failures.json).
package balancer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// Стадии пути запроса. Имена стабильные: они попадают в лог и в архив, по ним
// ищут («на какой стадии умер ход»), поэтому не переименовывать без правки доков.
const (
	stageSelectR477    = "select_backend" // выбор бэкенда/сессии
	stageAdmissionR477 = "admission"      // очередь на слот
	stagePreflightR477 = "preflight"      // проверки окна/загрузки модели
	stageUpstreamR477  = "upstream"       // запрос ушёл воркеру
	stageStreamR477    = "stream"         // идёт чтение стрима
	stageClientR477    = "client_write"   // запись клиенту
	stageDoneR477      = "done"           // успешно завершено
)

// stageTrackerR477 — текущая стадия одного запроса.
type stageTrackerR477 struct {
	mu    sync.Mutex
	stage string
	at    time.Time
}

// stageCtxKeyR477 — ключ контекста (свой тип, чтобы не пересечься).
type stageCtxKeyR477 struct{}

// newStageTrackerR477 — трекер для одного запроса.
func newStageTrackerR477() *stageTrackerR477 {
	return &stageTrackerR477{at: time.Now()}
}

// withStageTrackerR477 — положить трекер в контекст запроса.
func withStageTrackerR477(r *http.Request, t *stageTrackerR477) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), stageCtxKeyR477{}, t))
}

// stageTrackerFromCtxR477 — достать трекер (nil, если контекст не наш).
func stageTrackerFromCtxR477(ctx context.Context) *stageTrackerR477 {
	if ctx == nil {
		return nil
	}
	if t, ok := ctx.Value(stageCtxKeyR477{}).(*stageTrackerR477); ok {
		return t
	}
	return nil
}

// SetStageR477 — отметить стадию. Безопасно вызывать без трекера и из горутин.
func SetStageR477(ctx context.Context, stage string) {
	t := stageTrackerFromCtxR477(ctx)
	if t == nil || stage == "" {
		return
	}
	t.mu.Lock()
	t.stage = stage
	t.at = time.Now()
	t.mu.Unlock()
}

// CurrentStageR477 — стадия и время её установки.
func CurrentStageR477(ctx context.Context) (stage string, at time.Time) {
	t := stageTrackerFromCtxR477(ctx)
	if t == nil {
		return "", time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stage, t.at
}

// diagFailureR477 — один зафиксированный сбой для архива и панели.
type diagFailureR477 struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Stage     string    `json:"stage"`
	Status    int       `json:"status"`
	Backend   string    `json:"backend,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// diagFailuresR477 — кольцо последних сбоев (не больше 64 записей).
const diagFailuresMaxR477 = 64

var (
	diagFailuresMu  sync.Mutex
	diagFailuresLog []diagFailureR477
)

// recordFailureR477 — запомнить сбой (вызывается только для 5xx и паник).
func recordFailureR477(f diagFailureR477) {
	if f.Time.IsZero() {
		f.Time = time.Now()
	}
	diagFailuresMu.Lock()
	defer diagFailuresMu.Unlock()
	diagFailuresLog = append(diagFailuresLog, f)
	if len(diagFailuresLog) > diagFailuresMaxR477 {
		diagFailuresLog = diagFailuresLog[len(diagFailuresLog)-diagFailuresMaxR477:]
	}
	logger.Get().Warnw("request failed at stage",
		"request_id", f.RequestID, "stage", f.Stage, "status", f.Status,
		"method", f.Method, "path", f.Path, "backend", f.Backend, "reason", f.Reason)
}

// RecentFailuresR477 — снимок кольца (новые в конце).
func RecentFailuresR477() []diagFailureR477 {
	diagFailuresMu.Lock()
	defer diagFailuresMu.Unlock()
	out := make([]diagFailureR477, len(diagFailuresLog))
	copy(out, diagFailuresLog)
	return out
}

// resetFailuresR477 — очистить кольцо (тесты).
func resetFailuresR477() {
	diagFailuresMu.Lock()
	diagFailuresLog = nil
	diagFailuresMu.Unlock()
}

// panicTextR477 — текст паники без обращения к reflect (экономия на hot path не
// нужна: паника — редкое событие, но и лишних зависимостей не тянем).
func panicTextR477(rec interface{}) string {
	switch v := rec.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return "unknown panic value"
	}
}
