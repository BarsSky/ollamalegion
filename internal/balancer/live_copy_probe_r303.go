package balancer

// live_copy_probe_r303.go — R303: «где модель УЖЕ загружена» — спрашиваем живой
// кластер, а не только кэш метрик.
//
// ЖИВОЙ ДЕФЕКТ (двухузловой отчёт, D4). Модель загружена на доноре и работает.
// Клиент приходит через балансер:
//
//	19:07:42  routeRequest: /api/chat
//	          … пять минут тишины, мастер не получил ни одного POST /api/models/load
//	19:12:42  клиенту: 503 model "gemma-…" is being reloaded to n_ctx=32768
//	                    (loaded=0); retry in 30s
//
// Причина: кэш метрик опрашивает бэкенды раз в ~30 c, поэтому свежезагруженную
// копию на доноре балансер ещё не знает. `collectModelCopyCandidates` требует
// записи в кэше (backendHasModel), копий нет → выбор падает на ПУСТОЙ узел →
// preflight на пустом узле видит «модели нет» (loaded=0) → планирует загрузку
// с нуля → ждёт 240 c → отдаёт 503 с числами, которых на самом деле нет
// («loaded=0» при живой модели на другом узле).
//
// ПРАВИЛО R303. Прежде чем принимать решение «грузить/растить окно ЗДЕСЬ»,
// спросить живой кластер: если модель уже загружена на здоровом узле с
// подходящим окном — обслуживать там. Один опрос /api/models на узел, результат
// кэшируется на несколько секунд (иначе каждый запрос бил бы по всем узлам).
//
// ЧЕГО ЭТО НЕ ДЕЛАЕТ: не грузит модель, не меняет чужие состояния, не трогает
// узлы с идущими стримами (это забота R271/R293). Только отвечает на вопрос
// «где модель уже живая и с каким окном НА СЛОТ».
//
// Флаг оператора — LB_LIVE_COPY_PROBE (по умолчанию ON), горячий: 0 немедленно
// возвращает прежнее поведение без рестарта (полезно, когда опрос узлов дорогой,
// например по медленному каналу).

import (
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// liveCopyProbeEnabledR303 — флаг «спрашивать живой кластер, где модель уже есть».
func liveCopyProbeEnabledR303() bool {
	if v, ok := settings.HotInt("LB_LIVE_COPY_PROBE"); ok {
		return v != 0
	}
	return true
}

// liveCopyProbeTTLR303 — сколько живёт результат опроса. Держим коротким: цель —
// перекрыть окно «модель только что загружена, поллер ещё не увидел», а не
// заменить поллер. 3 c достаточно, чтобы один клиентский запрос не превращался
// в шторм опросов, и мало, чтобы «протухшее» состояние жило заметно долго.
const liveCopyProbeTTLR303 = 3 * time.Second

// liveCopyProbeTimeoutR303 — предел на весь опрос (по всем узлам). Опрос идёт
// только на пути, где иначе была бы ложная перезагрузка на минуты, поэтому
// несколько секунд здесь — выигрыш, а не потеря.
const liveCopyProbeTimeoutR303 = 4 * time.Second

type liveCopyProbeEntryR303 struct {
	expiresAt time.Time
	backendID string
	perSeq    int
}

var (
	liveCopyProbeMu    sync.Mutex
	liveCopyProbeCache = map[string]liveCopyProbeEntryR303{}
)

// resetLiveCopyProbeCacheR303 — сброс кэша опроса (тесты и смена состава узлов).
func resetLiveCopyProbeCacheR303() {
	liveCopyProbeMu.Lock()
	liveCopyProbeCache = map[string]liveCopyProbeEntryR303{}
	liveCopyProbeMu.Unlock()
}

func liveCopyProbeCacheKeyR303(model string, needPerSeq int) string {
	return model + "|" + itoaCacheKeyR303(needPerSeq)
}

// findLiveLoadedCopyR303 — узел, на котором модель УЖЕ загружена (живой опрос).
//
// Возвращает (backendID, perSeq, true), если нашли. Предпочтение:
//  1. копия, чьё окно НА СЛОТ покрывает needPerSeq (клиент обслужат сразу);
//  2. иначе копия с самым большим окном (растить окно там дешевле, чем грузить
//     модель с нуля на пустом узле);
//  3. детерминированный порядок при равенстве — по ID узла.
//
// exclude — узлы, которые нельзя предлагать (например текущий).
func (p *Proxy) findLiveLoadedCopyR303(model string, needPerSeq int, exclude map[string]bool) (string, int, bool) {
	if p == nil || model == "" || !liveCopyProbeEnabledR303() {
		return "", 0, false
	}
	key := liveCopyProbeCacheKeyR303(model, needPerSeq)
	liveCopyProbeMu.Lock()
	if e, ok := liveCopyProbeCache[key]; ok && time.Now().Before(e.expiresAt) {
		liveCopyProbeMu.Unlock()
		if e.backendID == "" {
			return "", 0, false
		}
		// Закэшированный ответ мог относится к узлу, который в ЭТОМ вызове
		// исключён (например это текущий узел залипшей сессии) — тогда кэш не
		// используем, но и не опрашиваем заново ради исключённого узла.
		if exclude != nil && exclude[e.backendID] {
			return "", 0, false
		}
		return e.backendID, e.perSeq, true
	}
	liveCopyProbeMu.Unlock()

	best := ""
	bestPerSeq := 0
	covers := false
	deadline := time.Now().Add(liveCopyProbeTimeoutR303)

	for _, id := range p.liveCopyCandidateIDsR303(exclude) {
		if time.Now().After(deadline) {
			logger.Get().Warnw("R303: опрос копий модели прерван по времени",
				"model", model, "probed_until", best)
			break
		}
		n, state := p.resolveLoadedWindowNCtx(id, model, true)
		if state != loadedWindowLoaded || n <= 0 {
			continue
		}
		perSeq := n
		if v := p.loadedPerSeqNCtx(id, model); v > 0 {
			perSeq = v
		}
		covered := needPerSeq > 0 && perSeq >= needPerSeq
		switch {
		case best == "":
			best, bestPerSeq, covers = id, perSeq, covered
		case covered && !covers:
			best, bestPerSeq, covers = id, perSeq, true
		case covered == covers && perSeq > bestPerSeq:
			best, bestPerSeq = id, perSeq
		}
	}

	liveCopyProbeMu.Lock()
	if len(liveCopyProbeCache) > 512 {
		liveCopyProbeCache = map[string]liveCopyProbeEntryR303{}
	}
	liveCopyProbeCache[key] = liveCopyProbeEntryR303{
		expiresAt: time.Now().Add(liveCopyProbeTTLR303),
		backendID: best,
		perSeq:    bestPerSeq,
	}
	liveCopyProbeMu.Unlock()

	if best == "" {
		return "", 0, false
	}
	logger.Get().Infow("R303: найден узел с уже загруженной моделью (живой опрос, кэш метрик молчал)",
		"model", model, "backend", best,
		"loaded_per_seq", bestPerSeq, "requested_per_seq", needPerSeq,
		"covers_request", covers)
	return best, bestPerSeq, true
}

// liveCopyCandidateIDsR303 — узлы, которые имеет смысл опрашивать: здоровые
// llama.cpp, не воркеры раскладки, не выключенные оператором, не исключённые.
// Порядок детерминированный (по ID), чтобы решение не «мигало» между запросами.
func (p *Proxy) liveCopyCandidateIDsR303(exclude map[string]bool) []string {
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if exclude != nil && exclude[id] {
			continue
		}
		if normalizeBackendType(state.Backend.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if state.Backend.Disabled {
			continue
		}
		if p.isGroupWorkerR92(id) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// modelNotLoadedHereButAliveElsewhereR303 — «здесь модели нет, но она жива на
// другом узле». Используется preflight'ом, чтобы НЕ планировать загрузку с нуля
// на пустом узле (и не ждать 240 c, отдавая потом ложное «(loaded=0)»).
//
// Возвращает (altBackend, altPerSeq, true) только когда модель на текущем узле
// действительно отсутствует (loadedWindowAbsent) — «неизвестно» сюда не попадает,
// иначе один неудачный опрос вводил бы запрет на загрузку.
func (p *Proxy) modelNotLoadedHereButAliveElsewhereR303(backendID, model string, needPerSeq int) (string, int, bool) {
	if p == nil || backendID == "" || model == "" {
		return "", 0, false
	}
	if !liveCopyProbeEnabledR303() {
		return "", 0, false
	}
	n, state := p.resolveLoadedWindowNCtx(backendID, model, true)
	if state != loadedWindowAbsent || n > 0 {
		return "", 0, false
	}
	alt, altPerSeq, ok := p.findLiveLoadedCopyR303(model, needPerSeq, map[string]bool{backendID: true})
	if !ok {
		return "", 0, false
	}
	return alt, altPerSeq, true
}

// modelAbsentOnBackendR303 — «на этом узле модели ТОЧНО нет» (для переезда
// залипшей сессии). Отвечает true только при положительном доказательстве:
// живой опрос узла прошёл и модели в ответе нет.
//
// «Состояние неизвестно» (узел не отвечает, опрос не прошёл) → false: переезд по
// недостоверным данным запрещён, иначе один медленный узел гонял бы запросы по
// кластеру.
func (p *Proxy) modelAbsentOnBackendR303(backendID, model string) bool {
	if p == nil || backendID == "" || model == "" || !liveCopyProbeEnabledR303() {
		return false
	}
	if n, state := p.loadedWindowFromCache(backendID, model); state != loadedWindowUnknown {
		// Кэш уже знает модель на этом узле — переезжать некуда.
		return state == loadedWindowAbsent && n == 0
	}
	n, state := p.resolveLoadedWindowNCtx(backendID, model, true)
	return state == loadedWindowAbsent && n == 0
}

// itoaCacheKeyR303 — ключ кэша опроса без strconv (отрицательных значений нет).
func itoaCacheKeyR303(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return strings.TrimLeft(string(buf[i:]), "\x00")
}
