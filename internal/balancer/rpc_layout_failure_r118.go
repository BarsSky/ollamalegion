package balancer

import (
	"fmt"
	"net"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// rpc_layout_failure_r118.go — R118.3 (2026-10-14): неудачная загрузка с
// раскладкой не должна оставлять оператора без модели и без объяснения.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СЛОЙ ПОСЛЕ ГАРДА. rpc_layout_guard.go проверяет узлы ДО
// загрузки. Но узел может умереть ВО ВРЕМЯ неё (перезапуск rpc-server, обрыв
// сети, падение узла). Тогда загрузка падает с ошибкой уровня RPC:
//
//	ggml-rpc.cpp:345: Remote RPC server crashed or returned malformed response
//	request failed: Post "http://.../api/models/load": EOF / connection refused
//	SIGABRT (cppworker уходит в рестарт)
//
// Раньше это выглядело для оператора как «модель не загрузилась» без причины, а
// cppworker мог ещё и уйти в рестарт. Теперь: раскладка гасится ЯВНО (с причиной,
// видимой в WebUI), после возврата бэкенда загрузка повторяется ЛОКАЛЬНО, а
// результат честно сообщает, что раскладка отключена и почему. Когда узел снова
// доступен, гард включает раскладку обратно (reapplyBackendRolesOnce).

// layoutFailureRetryMaxWait — сколько ждать возврата cppworker'а после падения,
// прежде чем повторить загрузку локально. Контейнер поднимается политикой
// `unless-stopped` за секунды; 90 c — с запасом на тяжёлый старт.
const layoutFailureRetryMaxWait = 90 * time.Second

// rpcFailureMarkers — признаки того, что загрузка упала из-за раскладки.
//
// Список намеренно узкий: локальный повтор стоит минуты, поэтому «повторяем
// локально» включается только на явные RPC/транспортные симптомы. Формулировки
// взяты из живых логов: ggml-rpc.cpp (клиент), remote/соединение, а также
// транспортные ошибки, которые видит балансер, когда cppworker ушёл в рестарт.
var rpcFailureMarkers = []string{
	"rpc",
	"ggml-rpc",
	"malformed response",
	"remote rpc server",
	"connection refused",
	"connection reset",
	"broken pipe",
	// Голый EOF: cppworker ушёл в рестарт прямо во время загрузки. При активной
	// раскладке это почти всегда следствие мёртвого узла (без него те же модели
	// грузятся локально), а локальный повтор стоит одной попытки.
	"eof",
	// ЖИВОЙ СИМПТОМ (стенд, 22:44): загрузка ушла в 202 Accepted (async), cppworker
	// упал на раскладке, и балансер увидел только «модель не появилась в списке».
	// Формулировка сбивает с толку («проверь наличие GGUF-файла»), хотя файл на
	// месте — поэтому такой исход тоже считается отказом раскладки (при активной
	// раскладке).
	"did not appear in cppworker",
	"load did not start",
	"no route to host",
	"i/o timeout",
}

// shouldRetryLoadWithoutLayout — стоит ли повторить загрузку без раскладки.
// Возвращает (true, симптом) только если раскладка была включена и текст ошибки
// указывает на RPC/транспорт.
func shouldRetryLoadWithoutLayout(errText string, layoutActive bool) (bool, string) {
	if !layoutActive {
		return false, ""
	}
	low := strings.ToLower(strings.TrimSpace(errText))
	if low == "" {
		return false, ""
	}
	for _, m := range rpcFailureMarkers {
		if strings.Contains(low, m) {
			return true, m
		}
	}
	return false, ""
}

// firstErrorLine — короткая форма ошибки для сообщения оператору.
func firstErrorLine(errText string) string {
	s := strings.TrimSpace(errText)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// DisableRPCLayoutFor — аварийно выключить раскладку у бэкенда с причиной.
//
// Нужно после неудачной загрузки: даже если TCP-проба узла ещё «зелёная» (узел
// принимает соединение, но ломается на слоях), работать с ним нельзя — иначе
// следующая загрузка снова упадёт. Гард увидит причину и не станет включать
// раскладку до следующего успешного прохода (узел вернулся → пробуем снова).
func (p *Proxy) DisableRPCLayoutFor(backendID, reason string) {
	if p == nil || backendID == "" {
		return
	}
	b := p.GetBackend(backendID)
	if b == nil || b.CppWorkerPort <= 0 {
		return
	}
	if err := pushRPCLayoutConfig(b, false, nil); err != nil {
		logger.Get().Warnw(i18n.T("balancer.rpc_layout_failure.disable_failed_warn"),
			"backend", backendID, "error", err)
		return
	}
	p.mu.Lock()
	if p.rpcLayoutApplied == nil {
		p.rpcLayoutApplied = map[string]string{}
	}
	p.rpcLayoutApplied[backendID] = layoutKey(false, nil)
	p.mu.Unlock()
	p.setLayoutReason(backendID, reason)
	logger.Get().Warnw(i18n.T("balancer.rpc_layout_failure.disabled_after_failed_load_warn"),
		"backend", backendID, "reason", reason)
}

// WaitBackendReachable — ждать, пока cppworker снова начнёт принимать соединения.
//
// После SIGABRT/рестарта контейнера порт какое-то время закрыт: повторять
// загрузку в это время бессмысленно (получим тот же connection refused).
func (p *Proxy) WaitBackendReachable(backendID string, timeout time.Duration) bool {
	if p == nil || backendID == "" {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if p.backendTCPReachable(backendID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// backendTCPReachable — отвечает ли порт cppworker'а бэкенда.
func (p *Proxy) backendTCPReachable(backendID string) bool {
	p.mu.RLock()
	st, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok || st == nil || st.Backend == nil {
		return false
	}
	host := strings.TrimSpace(st.Backend.Host)
	port := p.getBackendPort(st.Backend)
	if host == "" || port <= 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 1500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// executeLlamaCppLoad — загрузка модели с защитой раскладки.
//
// Слой состоит из трёх шагов:
//  1. гард: до загрузки привести раскладку к живым узлам (rpc_layout_guard.go);
//  2. обычная загрузка (executeLlamaCppLoadOnce);
//  3. если загрузка упала с RPC/транспортным симптомом и раскладка была
//     включена — выключить раскладку с причиной, дождаться живого бэкенда и
//     повторить загрузку ЛОКАЛЬНО, честно сообщив об этом в результате.
//
// ПРО «все слои на GPU» (`gpu_layers=-1`) ПРИ РАСКЛАДКЕ. R118.6 (2026-10-14)
// проверено измерением на стенде: 6 загрузок подряд с `-1` и включённой
// раскладкой — 6/6 успешно, 0 падений cppworker (RestartCount без изменений,
// ~90 с на загрузку); затем 15 последовательных инференсов и 3 пары параллельных
// — 21/21 HTTP 200, 0 падений. Падения, которые наблюдались РАНЬШЕ, совпадали с
// внешним убийством узла во время загрузки (остановка rpc-server в тестах) и уже
// закрыты шагом 3: раскладка гасится, модель поднимается локально, оператор
// получает причину. Поэтому `-1` НЕ переводится насильно в AUTO — молчаливая
// подмена явного намерения оператора без подтверждённой причины недопустима.
func (mm *ModelManager) executeLlamaCppLoad(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	layoutActive := false
	if mm.proxy != nil {
		layoutActive = mm.proxy.RPCLayoutActive(backendID)
		if degraded, reason := mm.proxy.EnsureRPCLayout(backendID); degraded {
			logger.Get().Warnw(i18n.T("balancer.rpc_layout_failure.degraded_warn"),
				"backend", backendID, "model", req.ModelName, "reason", reason)
			layoutActive = mm.proxy.RPCLayoutActive(backendID)
		}
	}

	res := mm.executeLlamaCppLoadOnce(host, port, backendID, req)
	if res == nil || res.Success || mm.proxy == nil {
		return res
	}
	retry, marker := shouldRetryLoadWithoutLayout(res.Error, layoutActive)
	if !retry && layoutActive && !mm.proxy.backendTCPReachable(backendID) {
		// Текст ошибки может быть любым («модель не появилась», таймаут poll'а), но
		// факт налицо: cppworker перестал отвечать ровно на загрузке с раскладкой.
		// Без узла те же модели грузятся локально — повторяем.
		retry = true
		marker = i18n.T("balancer.rpc_layout_failure.unresponsive_marker")
	}
	if !retry {
		return res
	}

	reason := i18n.T("balancer.rpc_layout_failure.disabled_reason",
		marker, firstErrorLine(res.Error))
	logger.Get().Warnw(i18n.T("balancer.rpc_layout_failure.retry_locally_warn"),
		"backend", backendID, "model", req.ModelName, "marker", marker, "error", firstErrorLine(res.Error))
	mm.proxy.DisableRPCLayoutFor(backendID, reason)

	if !mm.proxy.WaitBackendReachable(backendID, layoutFailureRetryMaxWait) {
		res.Error = i18n.T("balancer.rpc_layout_failure.retry_not_done_error",
			firstErrorLine(res.Error), reason, backendID, layoutFailureRetryMaxWait)
		return res
	}

	retryRes := mm.executeLlamaCppLoadOnce(host, port, backendID, req)
	if retryRes == nil {
		return res
	}
	if retryRes.Success {
		note := i18n.T("balancer.rpc_layout_failure.disabled_note", reason)
		if strings.TrimSpace(retryRes.Message) == "" {
			retryRes.Message = note
		} else {
			retryRes.Message = retryRes.Message + " | " + note
		}
		logger.Get().Infow(i18n.T("balancer.rpc_layout_failure.retry_ok_info"),
			"backend", backendID, "model", req.ModelName)
		return retryRes
	}
	retryRes.Error = i18n.T("balancer.rpc_layout_failure.retry_failed_error",
		firstErrorLine(res.Error), firstErrorLine(retryRes.Error))
	return retryRes
}

// RPCLayoutActive — включена ли раскладка у бэкенда прямо сейчас (по последнему
// решению гарда, без похода в сеть).
func (p *Proxy) RPCLayoutActive(backendID string) bool {
	if p == nil || backendID == "" {
		return false
	}
	p.mu.RLock()
	key := ""
	if p.rpcLayoutApplied != nil {
		key = p.rpcLayoutApplied[backendID]
	}
	p.mu.RUnlock()
	if key != "" {
		return strings.HasPrefix(key, "on:")
	}
	// Гард ещё не принимал решения (например, сразу после рестарта балансера) —
	// доверяем тому, что сообщает cppworker.
	enabled, servers, known := p.cppworkerRPCLayout(backendID)
	return known && enabled && len(servers) > 0
}
