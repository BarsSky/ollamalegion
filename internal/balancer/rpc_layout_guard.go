package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpc_layout_guard.go — R118.2 (2026-10-14): раскладка слоёв не должна ронять
// загрузку модели, если узел-донор недоступен.
//
// ЖИВОЙ ДЕФЕКТ. Раскладка задаётся конфигом cppworker'а главного (`rpcEnabled` +
// список адресов rpc-server'ов) и живёт там, пока её не сменят. Если донор
// выключили/перезапустили/у него упал rpc-server, cppworker главного всё равно
// идёт по сети за слоями:
//
//	ggml-rpc.cpp:345: Remote RPC server crashed or returned malformed response
//	SIGABRT
//
// То есть загрузка модели (и, что важнее, сам процесс cppworker) падает из-за
// недоступного узла — вместо того чтобы просто загрузиться локально. Оператор
// видел «модель не загрузилась» без внятной причины.
//
// ЧТО ДЕЛАЕТ ГАРД. Перед применением раскладки (и перед каждой загрузкой) он
// проверяет TCP-доступность узлов и приводит конфиг главного к ФАКТИЧЕСКИ живому
// набору:
//
//	все узлы живы        → rpcEnabled=true, полный список (как просил оператор)
//	часть узлов умерла   → rpcEnabled=true, только живые + WARN с причиной
//	все узлы умерли      → rpcEnabled=false, грузим ЛОКАЛЬНО + WARN с причиной
//
// Когда узел возвращается, следующий проход (раз в 15 c, см.
// reapplyBackendRolesOnce) сам возвращает полный список — оператору не нужно
// ничего перенастраивать. Лишних записей в конфиг cppworker'а нет: конфиг
// отправляется только когда фактическое состояние отличается от последнего
// применённого (cppworker сохраняет его на диск, см. handlers_config.go).

// rpcNodeDialTimeout — сколько ждём ответа TCP от узла раскладки при проверке.
// Значение маленькое осознанно: это горячий путь загрузки модели, а «занятый»
// rpc-server отвечает на connect мгновенно (он слушает порт).
const rpcNodeDialTimeout = 700 * time.Millisecond

// RPCNodeReachable — доступен ли rpc-server узла (TCP connect).
//
// Проверяем именно TCP, а не HTTP: rpc-server говорит по собственному
// бинарному протоколу ggml-rpc, HTTP-пробы к нему неприменимы.
func RPCNodeReachable(addr string, timeout time.Duration) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	if timeout <= 0 {
		timeout = rpcNodeDialTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// RPCLayoutStatus — состояние раскладки глазами оператора (для API/WebUI).
type RPCLayoutStatus struct {
	// Degraded — раскладка работает не в том составе, который задал оператор.
	Degraded bool `json:"degraded"`
	// Reason — человекочитаемая причина (пусто, если всё в порядке).
	Reason string `json:"reason,omitempty"`
	// Enabled — раскладка включена у главного ПРЯМО СЕЙЧАС.
	Enabled bool `json:"enabled"`
	// Servers — фактически применённый список узлов.
	Servers []string `json:"servers,omitempty"`
}

// LayoutGuardStatus — последнее применённое состояние раскладки (без похода в сеть).
func (p *Proxy) LayoutGuardStatus(backendID string) RPCLayoutStatus {
	if p == nil || backendID == "" {
		return RPCLayoutStatus{}
	}
	p.mu.RLock()
	reason := ""
	if p.rpcLayoutReason != nil {
		reason = p.rpcLayoutReason[backendID]
	}
	p.mu.RUnlock()
	st := RPCLayoutStatus{Degraded: reason != "", Reason: reason}
	if enabled, servers, known := p.cppworkerRPCLayout(backendID); known {
		st.Enabled = enabled
		st.Servers = servers
	}
	return st
}

// rpcNodePort — порт rpc-server'а узла раскладки (LB_RPC_NODE_PORT, по умолчанию
// 50052 — тот же, что использует api-слой при сборке списка доноров).
func (p *Proxy) rpcNodePort() int {
	if v := strings.TrimSpace(os.Getenv("LB_RPC_NODE_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 50052
}

// layoutDonorServers — адреса узлов раскладки из ролей бэкендов (host:port).
func (p *Proxy) layoutDonorServers() []string {
	var out []string
	for _, b := range p.GetAllBackends() {
		if strings.TrimSpace(b.Role) != types.BackendRoleDonor {
			continue
		}
		host := strings.TrimSpace(b.Host)
		if host == "" {
			continue
		}
		// R171 (2026-10-17): у ВНЕШНЕГО узла порт не rpcNodePort, а тот, что
		// сообщил воркер (CPPWORKER_RPC_EXTERNAL_ADDR). Раньше мастер получал
		// host:50052 и стучался в пустоту (или в Docker-прокси без процесса).
		if addr := p.reportedRPCNodeAddr(b.ID); addr != "" {
			out = append(out, addr)
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", host, p.rpcNodePort()))
	}
	sort.Strings(out)
	return out
}

// reportedRPCNodeAddr — адрес узла, который СООБЩИЛ сам воркер (если сообщил).
//
// R171: у внешнего узла адрес задаёт оператор (CPPWORKER_RPC_EXTERNAL_ADDR), и он
// может не совпадать с host:rpcNodePort. Правда о том, куда стучаться, — в отчёте
// воркера (rpc_server.addr), и она главнее догадки балансера.
func (p *Proxy) reportedRPCNodeAddr(backendID string) string {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return ""
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil && lm.RPCNode != nil {
		if addr := strings.TrimSpace(lm.RPCNode.Addr); addr != "" && !strings.HasPrefix(addr, "0.0.0.0") {
			return addr
		}
	}
	return ""
}

// reportedRPCNodePort — порт узла из отчёта воркера (0 = не сообщён).
func (p *Proxy) reportedRPCNodePort(backendID string) int {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil && lm.RPCNode != nil {
		return lm.RPCNode.Port
	}
	return 0
}

// cppworkerRPCLayout — что cppworker главного сообщает о раскладке (метрики
// поллера: /api/models → rpc_enabled + rpc_servers_active).
// known=false, если поллер ещё не собрал эти поля.
func (p *Proxy) cppworkerRPCLayout(backendID string) (enabled bool, servers []string, known bool) {
	if p == nil || p.metricsMgr == nil {
		return false, nil, false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil {
		return false, nil, false
	}
	servers = append([]string(nil), lm.RPCServersActive...)
	sort.Strings(servers)
	return lm.RPCEnabled, servers, true
}

// pushRPCLayoutConfig — отправить cppworker'у состав раскладки.
//
// Токен берём из записи бэкенда (cppworker запомнил его при саморегистрации):
// без него защищённый /config/update ответит 401.
func pushRPCLayoutConfig(b *types.Backend, enabled bool, servers []string) error {
	if b == nil {
		return fmt.Errorf("backend is nil")
	}
	if b.CppWorkerPort <= 0 {
		return fmt.Errorf("backend %s: неизвестен порт cppworker", b.ID)
	}
	if servers == nil {
		servers = []string{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"rpcEnabled": enabled,
		"rpcServers": servers,
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
	client := &http.Client{Timeout: 15 * time.Second}
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

// EnsureRPCLayout — привести раскладку бэкенда к фактически живому составу узлов.
//
// Возвращает degraded=true и причину, если раскладка работает не в полном
// составе (или выключена из-за недоступности узлов). Вызывается:
//   - при применении ролей (applyBackendRoles) и раз в 15 c (reapplyBackendRolesOnce);
//   - ПЕРЕД загрузкой модели на главном (executeLlamaCppLoad / auto-load) — чтобы
//     узел, умерший между тиками, не уронил загрузку.
func (p *Proxy) EnsureRPCLayout(backendID string) (degraded bool, reason string) {
	if p == nil || backendID == "" {
		return false, ""
	}
	b := p.GetBackend(backendID)
	if b == nil {
		return false, ""
	}

	role := strings.TrimSpace(b.Role)
	// R229 (2026-10-22): решение оператора (WebUI-тумблер «RPC» + «Применить»)
	// важнее роли. Живой дефект: выключенная оператором раскладка возвращалась
	// ролью главного через ~15 с (и перед каждой загрузкой), то есть тумблер
	// молча перезаписывался балансером.
	//
	// Семантика:
	//   auto (nil)  — как раньше, состав берётся из ролей;
	//   on (true)   — включить на донорах, которых видит балансер (доступность
	//                 всё равно проверяется ниже: мёртвый адрес в загрузку отдавать
	//                 нельзя — llama.cpp падает SIGABRT);
	//   off (false) — раскладка выключена, узлы не нужны; это НЕ деградация.
	var wanted []string
	if b.RPCEnabledOverride != nil {
		if *b.RPCEnabledOverride {
			wanted = p.layoutDonorServers()
		} else {
			wanted = nil
		}
	} else {
		switch role {
		case types.BackendRoleMain:
			wanted = p.layoutDonorServers()
		case types.BackendRoleDonor:
			// Донор — источник слоёв, а не потребитель: свою раскладку он не держит.
			// Если она у него осталась с прежней роли (живой случай: узел был главным,
			// потом стал донором) — гасим.
			wanted = nil
		default:
			// Самодостаточный режим (роль не задана или standalone): раскладки нет.
			wanted = nil
		}
	}

	alive, dead := probeRPCNodes(p.rpcProbeFunc(), wanted)
	if len(wanted) > 0 && len(alive) == 0 {
		reason = fmt.Sprintf("узлы раскладки недоступны (%s): раскладка выключена, загрузка идёт локально",
			strings.Join(dead, ", "))
	} else if len(dead) > 0 {
		reason = fmt.Sprintf("часть узлов раскладки недоступна (%s): работаем на живых (%s)",
			strings.Join(dead, ", "), strings.Join(alive, ", "))
	}

	enabled := len(alive) > 0
	sort.Strings(alive)
	key := layoutKey(enabled, alive)

	p.mu.Lock()
	if p.rpcLayoutApplied == nil {
		p.rpcLayoutApplied = map[string]string{}
	}
	if p.rpcLayoutReason == nil {
		p.rpcLayoutReason = map[string]string{}
	}
	lastKey := p.rpcLayoutApplied[backendID]
	p.mu.Unlock()

	// Уже применено и cppworker это подтверждает — не трогаем (конфиг cppworker
	// сохраняется на диск, лишние записи не нужны).
	if lastKey == key {
		if repEnabled, repServers, known := p.cppworkerRPCLayout(backendID); !known || layoutKey(repEnabled, repServers) == key {
			p.setLayoutReason(backendID, reason)
			return reason != "", reason
		}
	}

	if err := pushRPCLayoutConfig(b, enabled, alive); err != nil {
		logger.Get().Warnw("EnsureRPCLayout: не удалось применить раскладку",
			"backend", backendID, "role", role, "enabled", enabled,
			"servers", alive, "error", err)
		return true, fmt.Sprintf("раскладка не применена: %v", err)
	}

	p.mu.Lock()
	p.rpcLayoutApplied[backendID] = key
	p.mu.Unlock()
	p.setLayoutReason(backendID, reason)

	if reason != "" {
		logger.Get().Warnw("EnsureRPCLayout: раскладка работает не в полном составе",
			"backend", backendID, "role", role,
			"enabled", enabled, "servers", alive, "reason", reason)
	} else if enabled {
		logger.Get().Infow("EnsureRPCLayout: раскладка применена",
			"backend", backendID, "servers", alive)
	} else {
		logger.Get().Infow("EnsureRPCLayout: раскладка выключена (самодостаточный режим)",
			"backend", backendID)
	}
	return reason != "", reason
}

// setLayoutReason — запомнить/сбросить причину деградации для API и WebUI.
func (p *Proxy) setLayoutReason(backendID, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpcLayoutReason == nil {
		p.rpcLayoutReason = map[string]string{}
	}
	if reason == "" {
		delete(p.rpcLayoutReason, backendID)
		return
	}
	p.rpcLayoutReason[backendID] = reason
}

// rpcProbeFunc — функция проверки узла (в тестах подменяется).
//
// R118.21 (2026-10-15): проба обёрнута в проверку «а что об узле знает сам
// балансер». TCP-проба даёт ЛОЖНОПОЛОЖИТЕЛЬНЫЙ результат на хосте с
// docker-proxy: прокси принимает соединение к опубликованному порту даже когда
// контейнер уже мёртв. Живой случай: узел 192.0.2.10 выключен, TCP «отвечала»,
// раскладка ушла на мёртвый узел, клиент получил 503, и лишь через ~9 c guard
// деградировал. При этом у балансера уже была правда: запись узла unhealthy и без
// агента (agent timeout). Теперь она учитывается ДО попытки загрузки.
func (p *Proxy) rpcProbeFunc() func(string, time.Duration) bool {
	p.mu.RLock()
	base := p.rpcProbe
	p.mu.RUnlock()
	if base == nil {
		base = RPCNodeReachable
	}
	return func(server string, timeout time.Duration) bool {
		if p.nodeMarkedUnavailable(server) {
			return false
		}
		return base(server, timeout)
	}
}

// nodeHostOf — host из "host:port" (порт не важен: узел опознаётся по машине).
func nodeHostOf(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if i := strings.LastIndex(server, ":"); i > 0 {
		return server[:i]
	}
	return server
}

// nodeMarkedUnavailable — знает ли балансер, что узел раскладки недоступен.
//
// Признак — запись бэкенда с тем же host, помеченная как нездоровая или как
// «агент молчит» (AgentSilentSince). Если записи нет, возвращаем false: решает
// TCP-проба, как раньше (узел может быть зарегистрирован вручную, без агента).
func (p *Proxy) nodeMarkedUnavailable(server string) bool {
	host := nodeHostOf(server)
	if host == "" {
		return false
	}
	for _, b := range p.GetAllBackends() {
		if !strings.EqualFold(strings.TrimSpace(b.Host), host) {
			continue
		}
		if b.Status != types.StatusHealthy && b.Status != "" {
			return true
		}
		if b.AgentSilentSince != nil && !b.AgentSilentSince.IsZero() {
			return true
		}
	}
	return false
}

// probeRPCNodes — разделить узлы на живые и мёртвые (параллельно).
func probeRPCNodes(probe func(string, time.Duration) bool, servers []string) (alive, dead []string) {
	if len(servers) == 0 {
		return nil, nil
	}
	if probe == nil {
		probe = RPCNodeReachable
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ok  = make([]string, 0, len(servers))
		bad = make([]string, 0, len(servers))
	)
	for _, s := range servers {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if probe(s, rpcNodeDialTimeout) {
				mu.Lock()
				ok = append(ok, s)
				mu.Unlock()
				return
			}
			mu.Lock()
			bad = append(bad, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(ok)
	sort.Strings(bad)
	return ok, bad
}

// layoutKey — компактный ключ состояния раскладки ("off" | "on:addr1,addr2").
func layoutKey(enabled bool, servers []string) string {
	if !enabled || len(servers) == 0 {
		return "off"
	}
	sorted := append([]string(nil), servers...)
	sort.Strings(sorted)
	return "on:" + strings.Join(sorted, ",")
}

// EnsureRPCLayoutForAll — привести раскладку всех бэкендов с ролью (вызывается
// периодически из reapplyBackendRolesOnce: узел мог вернуться и раскладку нужно
// включить обратно без действий оператора).
//
// R133 (2026-10-16): здесь же приводится к роли СОБСТВЕННЫЙ узел бэкенда —
// донору отправляется приказ поднять rpc-server (см. rpc_layout_node_r133.go).
// Периодичность важна не меньше, чем для главного: донор мог быть пересоздан и
// забыть своё состояние (файл переопределений лежит на его томе), тогда «поднят»
// вернётся в пределах тика без участия оператора.
func (p *Proxy) EnsureRPCLayoutForAll() int {
	if p == nil {
		return 0
	}
	changed := 0
	for _, b := range p.GetAllBackends() {
		if strings.TrimSpace(b.Role) == "" {
			continue
		}
		if b.Type != "" && b.Type != types.BackendTypeLlamaCpp {
			continue
		}
		before := p.LayoutGuardStatus(b.ID)
		p.EnsureRPCLayout(b.ID)
		after := p.LayoutGuardStatus(b.ID)
		if before.Enabled != after.Enabled || before.Degraded != after.Degraded {
			changed++
		}
		if did, _ := p.EnsureRPCNodeServing(b.ID); did {
			changed++
		}
	}
	return changed
}
