package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpc_layout_node_r133.go — R133 (2026-10-16): донор поднимает свой узел
// раскладки ПО ПРИКАЗУ БАЛАНСЕРА.
//
// ЧЕГО НЕ ХВАТАЛО. Роли бэкендов (main/donor/standalone) и сборка списка доноров
// работали с R118: оператор в панели говорит, кто донор, а балансер отправляет
// ГЛАВНОМУ rpcEnabled + rpcServers (rpc_layout_guard.go). Донорской половины не
// было: роль donor только ГАСИЛА у донора его собственную раскладку-потребителя
// (EnsureRPCLayout, ветка donor) и НИКОГДА не просила его ОТДАВАТЬ GPU. Поэтому
// rpc-server на донорской машине приходилось поднимать руками — переменной
// окружения CPPWORKER_RPC_SERVER_ENABLED=true или тумблером в WebUI на самой
// машине. Ровно та асимметрия, из-за которой «в панели роль есть, а узел не
// работает».
//
// ЧТО ДЕЛАЕТ ЭТОТ ФАЙЛ. Для бэкенда с ролью donor балансер отправляет ему сам
// ту же ручку, которой раньше пользовался оператор вручную
// (PUT /api/v1/cppworker/config/update, ключи rpcServerEnabled + rpcServerPort) —
// ту, что уже умеет поднимать/гасить дочерний процесс на ходу (R123) и
// сохранять выбор в файле операторских переопределений (R131/R133). Статус
// возвращается сам: cppworker отдаёт rpc_server в /api/models, поллер его
// читает (pkg/types.RPCNodeStatus), API и панель показывают.
//
// ГРАНИЦА ПОЛНОМОЧИЙ (осознанно узкая). Балансер управляет узлом ТОЛЬКО у тех
// бэкендов, которым выдал роль donor, и гасит узел только там, где поднимал его
// сам (rpcNodeServed). Ручной тумблер оператора на остальных бэкендах не
// затаптывается: «я поднял сам» и «оператор поднял сам» — разные вещи, и вторую
// балансер не отменяет.
//
// ЧЕГО БАЛАНСЕР СДЕЛАТЬ НЕ МОЖЕТ. Опубликовать порт: это свойство запуска
// контейнера (docker run/compose), из процесса его не изменить. Если у донора
// порт не опубликован, узел поднимется и будет слушать ВНУТРИ контейнера —
// поэтому статус различает running (процесс) и listening (порт), а недоступность
// с балансера объясняется словами, а не молчанием.

// rpcNodeServeDialTimeout — сколько ждём TCP при проверке «узел донора доступен
// с балансера». Тот же порядок, что у проверки узлов у главного.
const rpcNodeServeDialTimeout = 700 * time.Millisecond

// rpcNodeOrderHTTPTimeout — сколько ждём ответа на приказ узлу.
//
// ЖИВОЙ ЗАМЕР (2026-10-16): с таймаутом 20 с один офлайн-донор (192.0.2.10)
// задерживал ВЕСЬ периодический проход: приказ мастеру уходил только после того,
// как отвалится недоступный узел. Приказ — это PUT на соседний воркер, он либо
// отвечает быстро, либо узел не в сети.
const rpcNodeOrderHTTPTimeout = 8 * time.Second

// rpcNodeOrderRetryInterval — как часто повторять приказ, если воркер НЕ
// отчитывается о своём узле (старый образ, где нет поля rpc_server в /api/models).
//
// Зачем интервал, а не каждый тик: приказ записывается воркером в файл на диск,
// поэтому повтор раз в 15 с — это лишние записи на донорской машине без новой
// информации. Раз в 2 минуты сохраняет самовосстановление (пересозданный донор
// снова получит приказ) и не молотит диск.
const rpcNodeOrderRetryInterval = 2 * time.Minute

// pushRPCNodeConfig — сказать бэкенду поднять (или погасить) ЕГО rpc-server.
//
// Токен берём из записи бэкенда: cppworker запомнил его при саморегистрации, без
// него защищённый /config/update ответит 401 (та же логика, что у
// pushRPCLayoutConfig — там это уже проверено живьём).
func pushRPCNodeConfig(b *types.Backend, enabled bool, port int) error {
	if b == nil {
		return fmt.Errorf("backend is nil")
	}
	if b.CppWorkerPort <= 0 {
		return fmt.Errorf(i18n.T("balancer.rpc_layout_node.unknown_port"), b.ID)
	}
	if port <= 0 {
		port = 50052
	}
	body, err := json.Marshal(map[string]interface{}{
		"rpcServerEnabled": enabled,
		"rpcServerPort":    port,
		// R304 (2026-10-30): роль едет ВМЕСТЕ с приказом о своём узле.
		//
		// ЗАЧЕМ. До этой правки воркер не знал, что он донор: роль жила только в
		// балансере, а воркер видел лишь «поднять мой rpc-server». Из-за этого он
		// не мог отказать в загрузке своей копии модели — а именно это и есть
		// смысл роли («донор только делит память с мастером»). Теперь он получает
		// роль тем же приказом и запоминает её (переживает пересоздание
		// контейнера), поэтому отказ работает и без связи с балансером.
		"backendRole": strings.TrimSpace(b.Role),
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s:%d/api/v1/cppworker/config/update", b.Host, b.CppWorkerPort)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.CppWorkerApiToken != "" {
		req.Header.Set("X-API-Token", b.CppWorkerApiToken)
	}
	client := &http.Client{Timeout: rpcNodeOrderHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cppworker %s: %w", b.ID, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cppworker %s: status %d", b.ID, resp.StatusCode)
	}
	return nil
}

// rpcNodeOrderKey — компактный ключ приказа узлу ("on:50052" | "off:50052").
func rpcNodeOrderKey(enabled bool, port int) string {
	if enabled {
		return fmt.Sprintf("on:%d", port)
	}
	return fmt.Sprintf("off:%d", port)
}

// rpcNodeStateKey — грубое состояние узла по словам воркера:
// unknown | up | starting | down.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ КЛЮЧ (R159). Повтор приказа надо глушить, только пока воркер
// топчется на месте. Как только состояние СМЕНИЛОСЬ (узел поднялся, упал или
// начал слушать порт), ждать интервал нельзя: упавший узел — это уже не «ждём
// подтверждения» и чинить надо сразу.
func rpcNodeStateKey(known, running, listening bool) string {
	switch {
	case !known:
		return "unknown"
	case running && listening:
		return "up"
	case running:
		return "starting"
	default:
		return "down"
	}
}

// rememberRPCNodeOrder — запомнить, что приказ узлу уже отправлен (и когда),
// вместе с состоянием воркера на тот момент.
func (p *Proxy) rememberRPCNodeOrder(backendID, key, state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpcNodeOrder == nil {
		p.rpcNodeOrder = map[string]string{}
	}
	if p.rpcNodeOrderAt == nil {
		p.rpcNodeOrderAt = map[string]time.Time{}
	}
	if p.rpcNodeOrderState == nil {
		p.rpcNodeOrderState = map[string]string{}
	}
	p.rpcNodeOrder[backendID] = key
	p.rpcNodeOrderAt[backendID] = time.Now()
	p.rpcNodeOrderState[backendID] = state
}

// lastRPCNodeOrder — последний отправленный приказ узлу.
func (p *Proxy) lastRPCNodeOrder(backendID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key, ok := p.rpcNodeOrder[backendID]
	return key, ok
}

// lastRPCNodeOrderAt — когда отправляли последний приказ узлу.
func (p *Proxy) lastRPCNodeOrderAt(backendID string) time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rpcNodeOrderAt[backendID]
}

// lastRPCNodeOrderState — состояние воркера на момент последнего приказа.
func (p *Proxy) lastRPCNodeOrderState(backendID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.rpcNodeOrderState[backendID]
	return st, ok
}

// rpcNodeOrderSuppressed — можно ли НЕ повторять приказ на этом проходе.
//
// ЖИВОЙ ДЕФЕКТ, который это закрывает (2026-10-17). Троттлинг повтора был только
// для случая «воркер вообще не отчитывается» (`!known`). Самый же частый случай
// другой: воркер отчитался, но порт ещё не слушает — узел стартует, или, как
// было на стенде, порт не опубликован. Тогда приказ уходил КАЖДЫЙ проход (тик
// ~15 с), а каждый приказ — это запись файла операторских переопределений на
// донорской машине. Наблюдалось вживую: пересоздали контейнер донора — балансер
// молотил приказом до самого перезапуска.
//
// Повторяем при любом из трёх условий:
//   - приказ изменился (другой порт или направление: включить/выключить);
//   - ИЗМЕНИЛОСЬ состояние воркера (узел упал или начал слушать — реагируем сразу);
//   - прошёл rpcNodeOrderRetryInterval (самовосстановление, если состояние залипло).
func (p *Proxy) rpcNodeOrderSuppressed(backendID string, enabled bool, port int, state string) bool {
	key := rpcNodeOrderKey(enabled, port)
	last, ok := p.lastRPCNodeOrder(backendID)
	if !ok || last != key {
		return false
	}
	if prev, okPrev := p.lastRPCNodeOrderState(backendID); okPrev && prev != state {
		return false
	}
	return time.Since(p.lastRPCNodeOrderAt(backendID)) < rpcNodeOrderRetryInterval
}

// cppworkerRPCNode — что бэкенд сообщает о СВОЁМ узле раскладки.
// known=false, если поллер ещё не собрал это поле (старый образ воркера).
func (p *Proxy) cppworkerRPCNode(backendID string) (running, listening bool, port int, known bool) {
	if p == nil || p.metricsMgr == nil {
		return false, false, 0, false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil || lm.RPCNode == nil {
		return false, false, 0, false
	}
	return lm.RPCNode.Running, lm.RPCNode.Listening, lm.RPCNode.Port, true
}

// rpcNodeAddr — адрес узла раскладки этого бэкенда (host:port).
func (p *Proxy) rpcNodeAddr(b *types.Backend) string {
	if b == nil {
		return ""
	}
	host := strings.TrimSpace(b.Host)
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(p.rpcNodePort()))
}

// VramRemoteSourceFor — откуда вклад узлов в бюджет у СПРАВОЧНЫХ путей этого
// бэкенда: none | cache | probed (R132, сообщает сам воркер в /api/models).
//
// Пусто означает «воркер ещё не сообщил» (старый образ). В панели это должно
// выглядеть как «неизвестно», а не как «узлов нет» — ровно та разница, ради
// которой поле и появилось.
func (p *Proxy) VramRemoteSourceFor(backendID string) string {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return ""
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil {
		return strings.TrimSpace(lm.VramRemoteSource)
	}
	return ""
}

// RPCNodeReport — состояние узла раскладки глазами балансера (для API/WebUI).
//
// Отдаётся БЕЗ похода в сеть: всё, что здесь есть, либо уже собрано поллером, либо
// записано последним проходом EnsureRPCNodeServing.
type RPCNodeReport struct {
	// Role — роль бэкенда в раскладке (standalone|main|donor).
	Role string `json:"role,omitempty"`
	// Wanted — должен ли узел быть поднят по роли (истина только для донора).
	Wanted bool `json:"wanted"`
	// Enabled/Running/Listening — что сообщает сам воркер: желание, процесс, порт.
	Enabled   bool `json:"enabled"`
	Running   bool `json:"running"`
	Listening bool `json:"listening"`
	// Addr — адрес, по которому узел должен быть доступен с балансера.
	Addr string `json:"addr,omitempty"`
	Port int    `json:"port,omitempty"`
	// Reachable — последняя TCP-проверка с балансера (только для wanted).
	Reachable bool `json:"reachable"`
	// Reason — почему узел не в том состоянии, которого требует роль.
	Reason string `json:"reason,omitempty"`
}

// RPCNodeReport собирает отчёт по бэкенду.
func (p *Proxy) RPCNodeReport(backendID string) RPCNodeReport {
	rep := RPCNodeReport{}
	if p == nil || backendID == "" {
		return rep
	}
	b := p.GetBackend(backendID)
	if b == nil {
		return rep
	}
	rep.Role = strings.TrimSpace(b.Role)
	rep.Wanted = rep.Role == types.BackendRoleDonor
	rep.Addr = p.rpcNodeAddr(b)
	rep.Port = p.rpcNodePort()
	if p.metricsMgr != nil {
		p.metricsMgr.mu.RLock()
		if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil && lm.RPCNode != nil {
			rep.Enabled = lm.RPCNode.Enabled
			rep.Running = lm.RPCNode.Running
			rep.Listening = lm.RPCNode.Listening
			if lm.RPCNode.Port > 0 {
				rep.Port = lm.RPCNode.Port
			}
			// R171 (2026-10-17): внешний узел живёт по адресу, который задал
			// оператор (CPPWORKER_RPC_EXTERNAL_ADDR), и он может не совпадать с
			// host:rpcNodePort. Показываем СООБЩЁННЫЙ адрес — иначе балансер
			// проверяет доступность не того порта и пишет reachable=false при
			// работающем узле (живой случай: узел на 50055, проверка на 50052).
			if addr := strings.TrimSpace(lm.RPCNode.Addr); addr != "" && !strings.HasPrefix(addr, "0.0.0.0") {
				rep.Addr = addr
			}
		}
		p.metricsMgr.mu.RUnlock()
	}
	p.mu.RLock()
	rep.Reason = p.rpcNodeReason[backendID]
	rep.Reachable = p.rpcNodeReachable[backendID]
	p.mu.RUnlock()
	return rep
}

// setRPCNodeState — запомнить причину/доступность узла (под p.mu).
func (p *Proxy) setRPCNodeState(backendID string, served bool, reachable bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpcNodeServed == nil {
		p.rpcNodeServed = map[string]bool{}
	}
	if p.rpcNodeReason == nil {
		p.rpcNodeReason = map[string]string{}
	}
	if p.rpcNodeReachable == nil {
		p.rpcNodeReachable = map[string]bool{}
	}
	if served {
		p.rpcNodeServed[backendID] = true
	}
	p.rpcNodeReachable[backendID] = reachable
	if strings.TrimSpace(reason) == "" {
		delete(p.rpcNodeReason, backendID)
		return
	}
	p.rpcNodeReason[backendID] = reason
}

// rpcNodeWasServed — поднимали ли узел МЫ (а не оператор руками).
func (p *Proxy) rpcNodeWasServed(backendID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rpcNodeServed[backendID]
}

// forgetRPCNodeServed — забыть, что узел поднимали мы (после того как погасили).
func (p *Proxy) forgetRPCNodeServed(backendID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.rpcNodeServed, backendID)
}

// EnsureRPCNodeServing — привести СОБСТВЕННЫЙ узел раскладки бэкенда к тому, чего
// требует его роль. Возвращает changed (мы что-то поменяли приказом) и reason
// (человекочитаемая причина, если узел не в нужном состоянии).
//
// Идемпотентно: если воркер уже сообщает running+listening на нужном порту, приказ
// не отправляется (конфиг воркера пишется на диск, лишние записи не нужны).
func (p *Proxy) EnsureRPCNodeServing(backendID string) (bool, string) {
	if p == nil || backendID == "" {
		return false, ""
	}
	b := p.GetBackend(backendID)
	if b == nil {
		return false, ""
	}
	if b.Type != "" && b.Type != types.BackendTypeLlamaCpp {
		return false, ""
	}
	role := strings.TrimSpace(b.Role)
	wantPort := p.rpcNodePort()
	// R171 (2026-10-17): если воркер сообщил свой адрес узла (внешний контейнер
	// с CPPWORKER_RPC_EXTERNAL_ADDR), решение и проверка идут по НЕМУ. Иначе
	// балансер проверял host:50052, где узла нет, и писал reachable=false при
	// работающем узле, а мастер получал неверный список rpcServers.
	//
	// R350 (2026-10-31): сообщённый адрес теперь ПРОВЕРЯЕТСЯ. Живой случай: донор
	// переехал (host записи исправлен на 100.64.0.7), а на нём остался старый
	// CPPWORKER_RPC_EXTERNAL_ADDR=192.168.13.34. По R171 отчёт воркера главнее
	// догадки балансера, поэтому мастер получал мёртвый адрес, guard писал
	// «раскладка работает не в полном составе», и модель грузилась локально при
	// rpc_enabled=true. Теперь мёртвый сообщённый адрес заменяется host'ом записи
	// бэкенда — с предупреждением в лог (молча подменять нельзя).
	addr, _ := p.rpcNodeAddrResolved(b, rpcNodeServeDialTimeout)
	if repPort := p.reportedRPCNodePort(b.ID); repPort > 0 {
		wantPort = repPort
	}

	if role != types.BackendRoleDonor {
		// Роль снята или сменилась. Гасим узел ТОЛЬКО если поднимали его мы:
		// ручной тумблер оператора на не-донорском бэкенде — не наше решение.
		if !p.rpcNodeWasServed(backendID) {
			return false, ""
		}
		if err := pushRPCNodeConfig(b, false, wantPort); err != nil {
			reason := i18n.T("balancer.rpc_layout_node.stop_failed_reason", err)
			p.setRPCNodeState(backendID, true, false, reason)
			logger.Get().Warnw(i18n.T("balancer.rpc_layout_node.stop_failed_warn"),
				"backend", backendID, "role", role, "error", err)
			return false, reason
		}
		p.forgetRPCNodeServed(backendID)
		p.setRPCNodeState(backendID, false, false, "")
		p.rememberRPCNodeOrder(backendID, rpcNodeOrderKey(false, wantPort), "off")
		logger.Get().Infow(i18n.T("balancer.rpc_layout_node.stopped_info"),
			"backend", backendID, "role", role)
		return true, ""
	}

	// Роль donor: узел обязан быть поднят.
	running, listening, reportedPort, known := p.cppworkerRPCNode(backendID)

	// R164 (2026-10-17): ПРИНУДИТЕЛЬНОЕ ОБНОВЛЕНИЕ ПЕРЕД РЕШЕНИЕМ.
	//
	// ЗАЧЕМ. Живой случай: воркер слушал порт и отдавал rpc_server.listening=true,
	// а балансер видел listening=false, повторял приказ каждые 15 с и «ожил» только
	// после перезапуска процесса. Точную причину залипания доказать не удалось
	// (редкая гонка при пересоздании контейнера), но лечится она детерминированно:
	// если узел НЕ подтверждён, нельзя опираться на снимок, которому уже нельзя
	// верить, — опрашиваем бэкенд прямо здесь и решаем по свежим данным.
	//
	// Цена: один HTTP-запрос на проход, и только пока узел не подтверждён
	// (в норме — ноль). Это дешевле, чем молотящий приказ и ложное «недоступен».
	if !known || !running || !listening {
		if p.llamaCppMetricsPoller != nil {
			if b, ok := p.backendInfoForPoller(backendID); ok {
				p.llamaCppMetricsPoller.pollBackend(b)
				prevKnown, prevRunning, prevListening := known, running, listening
				running, listening, reportedPort, known = p.cppworkerRPCNode(backendID)
				if prevRunning != running || prevListening != listening || prevKnown != known {
					logger.Get().Infow(i18n.T("balancer.rpc_layout_node.snapshot_refreshed_info"),
						"backend", backendID,
						"was_known", prevKnown, "was_running", prevRunning, "was_listening", prevListening,
						"known", known, "running", running, "listening", listening)
				}
			}
		}
	}
	if known && running && listening && (reportedPort == 0 || reportedPort == wantPort) {
		// Узел по факту поднят. Проверяем, доступен ли он С БАЛАНСЕРА: «процесс
		// слушает порт» и «мастер может к нему подключиться» — разные вещи, если
		// порт не опубликован при запуске контейнера.
		reachable := true
		reason := ""
		if addr != "" {
			reachable = p.rpcProbeFunc()(addr, rpcNodeServeDialTimeout)
			if !reachable {
				reason = i18n.T("balancer.rpc_layout_node.not_reachable_reason", addr)
			}
		}
		p.setRPCNodeState(backendID, true, reachable, reason)
		p.rememberRPCNodeOrder(backendID, rpcNodeOrderKey(true, wantPort), "up")
		if !reachable {
			logger.Get().Warnw(i18n.T("balancer.rpc_layout_node.unreachable_warn"),
				"backend", backendID, "addr", addr)
		}
		return false, reason
	}

	// Бэкенд, о котором балансер уже знает, что он не в сети: приказ всё равно не
	// дойдёт, а ожидание ответа задерживает весь проход (живой замер: офлайн-донор
	// держал тик 20 с, и приказ мастеру уходил только после этого). Роль при этом
	// НЕ снимаем: оператор может настраивать узел (доктрина R118.10), приказ уйдёт
	// сам, как только запись станет здоровой.
	if addr != "" && p.nodeMarkedUnavailable(addr) {
		reason := i18n.T("balancer.rpc_layout_node.offline_reason", addr)
		p.setRPCNodeState(backendID, false, false, reason)
		return false, reason
	}

	// ПОВТОР ПРИКАЗА — НЕ КАЖДЫЙ ТИК (R159). До этой правки троттлинг работал
	// только для «воркер не отчитывается», а ветка «отчитался, но порт не
	// слушает» молотила приказом каждый проход (см. rpcNodeOrderSuppressed).
	state := rpcNodeStateKey(known, running, listening)
	if p.rpcNodeOrderSuppressed(backendID, true, wantPort, state) {
		// Диагностика на уровне Debug осознанно: при залипшем поллере именно эти
		// четыре поля показывают, что балансер видит «не так», как воркер.
		logger.Get().Debugw(i18n.T("balancer.rpc_layout_node.order_pending_debug"),
			"backend", backendID, "state", state, "known", known,
			"running", running, "listening", listening, "port", wantPort)
		return false, p.RPCNodeReport(backendID).Reason
	}
	logger.Get().Debugw(i18n.T("balancer.rpc_layout_node.state_mismatch_debug"),
		"backend", backendID, "state", state, "known", known,
		"running", running, "listening", listening, "reported_port", reportedPort, "want_port", wantPort)

	if err := pushRPCNodeConfig(b, true, wantPort); err != nil {
		reason := i18n.T("balancer.rpc_layout_node.start_failed_reason", err)
		p.setRPCNodeState(backendID, true, false, reason)
		logger.Get().Warnw(i18n.T("balancer.rpc_layout_node.order_not_delivered_warn"),
			"backend", backendID, "addr", addr, "error", err)
		return false, reason
	}
	// Статус появится со следующего опроса поллера; до тех пор честно говорим,
	// что узел ещё не подтверждён, а не выдаём приказ за готовность.
	p.rememberRPCNodeOrder(backendID, rpcNodeOrderKey(true, wantPort), state)
	p.setRPCNodeState(backendID, true, false,
		i18n.T("balancer.rpc_layout_node.order_sent_reason", b.ID, wantPort))
	logger.Get().Infow(i18n.T("balancer.rpc_layout_node.order_sent_info"),
		"backend", backendID, "addr", addr, "port", wantPort, "state", state)
	return true, ""
}
