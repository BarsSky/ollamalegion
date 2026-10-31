// rpc_probe_cache_r349.go — R349 (2026-10-31): один dial на узел раскладки вместо
// трёх за проход.
//
// ЖИВОЙ ФАКТ (двухузловой стенд, docs/plans/2026-10-31-two-node-donor-check.md §7).
// Пока раскладка включена, rpc-server донора видит периодические пары
// «Accepted client connection / Client connection closed» — замерено 10-11
// подключений в минуту при ПОЛНОСТЬЮ простаивающем стенде (модели не загружены).
// Один проход guard'а (раз в 15 с) дёргает узел несколько раз: список узлов для
// мастера, проверка «узел доступен» перед решением и разрешение адреса (R350).
// Каждый вызов — новый TCP-connect, то есть шум в логе донора, лишние wake-up'ы и
// невозможность отличить «проверка живости» от «реальный отказ» в отчётах.
//
// ПРАВИЛО. Проба узла на путях РАСКЛАДКИ (guard, разрешение адреса, решение о
// приказе) кэшируется на короткий TTL: один dial обслуживает все вызовы прохода.
//
// ЧЕГО КЭШ НЕ КАСАЕТСЯ (важно, R114). Гейт перед ГЕНЕРАЦИЕЙ
// (checkRPCNodesForInference) обязан видеть правду СЕЙЧАС: мёртвый узел на этом
// пути — это abort() в llama.cpp и падение мастера, а не «плохой ответ». Он
// сознательно не использует кэш, и это закреплено тестом-сторожем.
package balancer

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rpcProbeCacheDefaultTTL — сколько живёт результат пробы по умолчанию.
//
// 10 с при проходе guard'а раз в 15 с: каждый проход получает свежую пробу, но
// внутренние вызовы одного прохода переиспользуют её. Меньше 2 с смысла не имеет
// (несколько вызовов идут подряд), больше 15 с — проход начнёт работать по
// устаревшему «жив».
const rpcProbeCacheDefaultTTL = 10 * time.Second

// rpcProbeCacheEntry — результат одной пробы.
type rpcProbeCacheEntry struct {
	alive bool
	at    time.Time
}

// rpcProbeCacheTTL — TTL из окружения (LB_RPC_PROBE_CACHE_SEC), 0 = кэш выключен.
//
// Читается на каждом вызове, как и остальные горячие настройки LB_* в этом пакете:
// оператор может выключить кэш без перезапуска (0 — «всегда честный dial»).
func rpcProbeCacheTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_RPC_PROBE_CACHE_SEC"))
	if v == "" {
		return rpcProbeCacheDefaultTTL
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return rpcProbeCacheDefaultTTL
	}
	if n == 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// rpcProbeCached — проба узла с коротким кэшем.
//
// Порядок решения важен:
//  1. отметка «узел недоступен» из записи бэкенда проверяется ВСЕГДА и сразу
//     (она дешевле dial и меняется мгновенно) — кэш не может «оживить» узел,
//     который балансер уже считает мёртвым;
//  2. свежий результат из кэша — отдаём без dial;
//  3. иначе dial и запоминание.
func (p *Proxy) rpcProbeCached(addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	if p.nodeMarkedUnavailable(addr) {
		return false
	}

	ttl := rpcProbeCacheTTL()
	if ttl <= 0 {
		return p.rpcProbeFunc()(addr, timeout)
	}

	p.rpcProbeMu.Lock()
	ent, ok := p.rpcProbeCachedBy[addr]
	p.rpcProbeMu.Unlock()
	if ok && time.Since(ent.at) < ttl {
		noteRPCCacheHit()
		return ent.alive
	}

	alive := p.rpcProbeFunc()(addr, timeout)
	noteRPCDial()

	p.rpcProbeMu.Lock()
	if p.rpcProbeCachedBy == nil {
		p.rpcProbeCachedBy = map[string]rpcProbeCacheEntry{}
	}
	p.rpcProbeCachedBy[addr] = rpcProbeCacheEntry{alive: alive, at: time.Now()}
	p.rpcProbeMu.Unlock()
	return alive
}

// rpcProbeCachedFn — тот же кэш в форме функции (для probeRPCNodes).
func (p *Proxy) rpcProbeCachedFn() func(string, time.Duration) bool {
	return func(addr string, timeout time.Duration) bool {
		return p.rpcProbeCached(addr, timeout)
	}
}

// forgetRPCProbeCache — сброс кэша проб (смена состава раскладки, установка роли).
//
// ЗАЧЕМ. Узел, который только что назначили донором, не должен ждать истечения TTL,
// чтобы попасть в список; и наоборот, снятый с роли узел не должен «жить» в кэше.
func (p *Proxy) forgetRPCProbeCache() {
	if p == nil {
		return
	}
	p.rpcProbeMu.Lock()
	p.rpcProbeCachedBy = nil
	p.rpcProbeMu.Unlock()
}

// rpcProbeCacheStats — счётчики для отчёта/тестов: сколько dial'ов реально
// случилось и сколько вызовов обслужено из кэша.
type rpcProbeCacheStats struct {
	Dials int
	Hits  int
}

var rpcProbeCacheCounters struct {
	mu sync.Mutex
	d  int
	h  int
}

func noteRPCDial() {
	rpcProbeCacheCounters.mu.Lock()
	rpcProbeCacheCounters.d++
	rpcProbeCacheCounters.mu.Unlock()
}

func noteRPCCacheHit() {
	rpcProbeCacheCounters.mu.Lock()
	rpcProbeCacheCounters.h++
	rpcProbeCacheCounters.mu.Unlock()
}

// RPCProbeCacheStats — снимок счётчиков (для /api/info и тестов).
func RPCProbeCacheStats() rpcProbeCacheStats {
	rpcProbeCacheCounters.mu.Lock()
	defer rpcProbeCacheCounters.mu.Unlock()
	return rpcProbeCacheStats{Dials: rpcProbeCacheCounters.d, Hits: rpcProbeCacheCounters.h}
}

// ResetRPCProbeCacheStatsForTest — обнуление счётчиков (только тесты).
func ResetRPCProbeCacheStatsForTest() {
	rpcProbeCacheCounters.mu.Lock()
	rpcProbeCacheCounters.d = 0
	rpcProbeCacheCounters.h = 0
	rpcProbeCacheCounters.mu.Unlock()
}
