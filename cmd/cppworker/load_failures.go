// load_failures.go — R66d (2026-09-22): память о последней неудачной загрузке.
//
// ЗАЧЕМ. Асинхронная загрузка (POST /api/models/load → 202 Accepted + фоновая
// горутина runAsyncLoad) при провале только писала строку в лог:
//
//	runAsyncLoad: background load failed ... failed to open GGUF file ... No such file
//
// а запись о модели удалялась из backend.models (см. LoadModelWithOpts: на ошибке
// inst удаляется). В итоге и GET /api/models, и GET /api/models/load/progress
// показывали «ничего нет» — то есть для клиента провалившаяся загрузка была
// НЕОТЛИЧИМА от «ещё грузится». Балансер в этой ситуации поллил до 3-15 минут
// (maxWait), держал операцию load в статусе running, а клиенту всё это время
// отдавал 503 «load started, wait ~30-180s, retry_after=90».
//
// ЖИВОЙ КЕЙС: Cline настроен на gemma-4-E4B-it-Q4_K_M, файла на диске нет —
// cppworker падал за 3 мс, а клиент получал «подожди 90 секунд» бесконечно.
//
// ТЕПЕРЬ: последняя ошибка загрузки/перезагрузки запоминается на TTL и
// отдаётся в /api/models/load/progress как {"state":"failed","error":"..."},
// поэтому и балансер, и WebUI (GgufLoadProgress) видят настоящую причину.

package main

import (
	"sync"
	"time"
)

// loadFailureTTL — сколько держим запись о провале. Достаточно, чтобы клиент
// (или балансер) успел её прочитать после неудачной попытки, но не настолько
// много, чтобы «failed» висел вечно после того, как файл положили на место.
const loadFailureTTL = 10 * time.Minute

// loadFailureEntry — одна запись о провале загрузки.
//
// Порядок полей — по требованию govet fieldalignment (в .golangci.yml включён
// govet.enable-all): первым идёт поле с указателем (time.Time хранит
// *Location), за ним строка.
type loadFailureEntry struct {
	At  time.Time
	Err string
}

// loadFailureRegistry — потокобезопасный реестр последних провалов загрузки.
type loadFailureRegistry struct {
	m   map[string]loadFailureEntry
	now func() time.Time // подмена времени в тестах
	ttl time.Duration
	mu  sync.Mutex
}

func newLoadFailureRegistry(ttl time.Duration) *loadFailureRegistry {
	return &loadFailureRegistry{
		m:   make(map[string]loadFailureEntry),
		ttl: ttl,
		now: time.Now,
	}
}

// record — запомнить ошибку загрузки модели.
func (r *loadFailureRegistry) record(name string, err error) {
	if r == nil || name == "" || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = loadFailureEntry{Err: err.Error(), At: r.now()}
}

// clear — забыть ошибку (успешная загрузка).
func (r *loadFailureRegistry) clear(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, name)
}

// get — последняя ошибка модели, если она ещё не истекла по TTL.
func (r *loadFailureRegistry) get(name string) (loadFailureEntry, bool) {
	if r == nil || name == "" {
		return loadFailureEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[name]
	if !ok {
		return loadFailureEntry{}, false
	}
	if r.ttl > 0 && r.now().Sub(e.At) > r.ttl {
		delete(r.m, name)
		return loadFailureEntry{}, false
	}
	return e, true
}

// list — все не истёкшие записи (для GET /api/models/load/progress без model).
func (r *loadFailureRegistry) list() map[string]loadFailureEntry {
	out := make(map[string]loadFailureEntry)
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for name, e := range r.m {
		if r.ttl > 0 && now.Sub(e.At) > r.ttl {
			delete(r.m, name)
			continue
		}
		out[name] = e
	}
	return out
}

// loadFailures — процесс-глобальный реестр (cppworker обслуживает один backend).
var loadFailures = newLoadFailureRegistry(loadFailureTTL)
