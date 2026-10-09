package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// foreignRegistrantGrace — R-MultiHost (2026-10-07): сколько времени запись
// бэкенда считается «занятой» её текущим узлом.
//
// Если агент-владелец не выходил на связь дольше этого окна, запись считается
// осиротевшей и регистрация с ДРУГОГО адреса принимается (takeover). Это нужно
// для штатного случая: контейнер воркера пересоздали (`up -d --build`), он
// получил новый bridge-IP и обязан перерегистрироваться, а старая запись ещё
// держит прежний адрес. Без окна такой рестарт был бы заклинен в 409.
//
// 60 секунд — с большим запасом больше штатного heartbeat (5 с) и периода
// опроса агента балансером, поэтому:
//   - живая вторая машина (heartbeat каждые 5 с) отвергается ВСЕГДА;
//   - мёртвый контейнер освобождает ID не позднее чем через минуту.
const foreignRegistrantGrace = 60 * time.Second

// peerIP — адрес узла, с которого пришёл запрос.
//
// Для multi-host это единственный признак, различающий машины: имена
// контейнеров, backendID и порты на обеих машинах совпадают буквально, потому
// что compose-файл один и тот же (см. types.Backend.NodeAddr).
func peerIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		// RemoteAddr без порта (некоторые in-process вызовы в тестах).
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		// ::ffff:192.0.2.11 → 192.0.2.11: один и тот же узел не должен
		// выглядеть как два разных из-за формы записи адреса.
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
		return ip.String()
	}
	return host
}

// sameNode — один ли это узел. Пустые значения не считаются совпадением:
// «неизвестно» разбирается вызывающей стороной отдельно.
func sameNode(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b
}

// registrationOwnerIsLive — агент-владелец записи выходил на связь недавно?
//
// ВАЖНО про окно после старта балансера. `LastAgentContact` восстанавливается из
// state.json, то есть после перезапуска оно «старое» по определению. Если
// считать по нему, сразу после рестарта любая запись выглядит осиротевшей — и
// чужой узел с тем же ID успевает её захватить раньше законного владельца.
// Ровно это и наблюдалось на живой паре: imageworker перешёл к удалённой машине
// (ownerNode=172.23.0.1) и больше не принимал метрики своей, хотя её агент был
// жив и слал heartbeat.
//
// Поэтому пока балансер сам не прожил foreignRegistrantGrace, записи считаются
// занятыми: у законного владельца есть минута, чтобы отметиться (heartbeat идёт
// каждые 5 с), и только потом возможен takeover.
func (s *Server) registrationOwnerIsLive(existing *types.Backend) bool {
	if existing == nil {
		return false
	}
	if !s.startedAt.IsZero() && time.Since(s.startedAt) < foreignRegistrantGrace {
		return true
	}
	if existing.LastAgentContact.IsZero() {
		return false
	}
	return time.Since(existing.LastAgentContact) < foreignRegistrantGrace
}

// rejectForeignRegistration — R-MultiHost: не дать одной машине захватить
// запись бэкенда, принадлежащую другой.
//
// Возвращает true, если запрос УЖЕ отвергнут (ответ 409 отправлен) и обработчик
// обязан немедленно завершиться.
//
// ЗАЧЕМ. На живой стойке из двух машин compose на обеих одинаков, поэтому:
//   - host = "cppworker-gpu" (имя контейнера) — совпадает;
//   - backendID = "cppworker-gpu-bundled-agent" — совпадает;
//   - cppWorkerPort = 18092 — совпадает.
//
// В результате вторая машина проходила и ветку dedup (FindBackendByHostPort),
// и ветку «backend exists», и молча переписывала AgentID/AgentPort/метрики
// чужой записи. Симптом: страница модели показывала CPU/GPU ВТОРОЙ машины под
// URL первой, а сама вторая машина не обслуживала ни одного запроса — то есть
// «не зарегистрировалась».
//
// Теперь такая регистрация отвергается явно и с инструкцией, что задать.
// Поведение для одной машины (источник совпадает с NodeAddr) не меняется;
// записи без NodeAddr (созданные до этой ревизии или вручную из WebUI) не
// защищаются — прежнее поведение сохраняется.
func (s *Server) rejectForeignRegistration(w http.ResponseWriter, r *http.Request, backendID string) bool {
	if backendID == "" {
		return false
	}
	existing := s.proxy.GetBackend(backendID)
	if existing == nil {
		return false
	}
	// Узел-владелец неизвестен → защищать нечего, ведём себя как раньше.
	if existing.NodeAddr == "" {
		return false
	}
	src := peerIP(r)
	if src == "" || sameNode(src, existing.NodeAddr) {
		return false
	}
	// Владелец молчит дольше окна → запись осиротела, takeover разрешён
	// (штатный рестарт контейнера).
	if !s.registrationOwnerIsLive(existing) {
		logger.Get().Infow("registration guard: takeover of idle backend record",
			"backend", backendID,
			"ownerNode", existing.NodeAddr,
			"newNode", src,
			"lastAgentContact", existing.LastAgentContact)
		return false
	}

	logger.Get().Warnw("registration guard: rejected registration for a backend owned by another node",
		"backend", backendID,
		"ownerNode", existing.NodeAddr,
		"newNode", src,
		"agentId", existing.AgentID,
		"lastAgentContact", existing.LastAgentContact,
		"hint", "set a unique AGENT_ID / SDWORKER_BACKEND_ID and BACKEND_HOST=<this host address> on the new node")

	s.writeJSON(w, http.StatusConflict, map[string]interface{}{
		"success": false,
		"error": fmt.Sprintf(
			"Backend %q is already served by node %s (its agent is alive). "+
				"Two hosts are using identical backend IDs and advertised host names because the compose file is the same. "+
				"On this node (%s) set a unique AGENT_ID (or SDWORKER_BACKEND_ID for the image worker) and BACKEND_HOST=%s, then restart the worker.",
			backendID, existing.NodeAddr, src, src),
		"backendId":      backendID,
		"ownerNode":      existing.NodeAddr,
		"registrantNode": src,
	})
	return true
}

// rejectForeignNode — R-MultiHost: метрики и heartbeat принимаются только от
// узла, за которым закреплена запись.
//
// ПОЧЕМУ ОТДЕЛЬНО ОТ guard'а РЕГИСТРАЦИИ. Агент регистрируется РОВНО ОДИН раз
// при старте (collector.go: Start → register, без повторов), а метрики и
// heartbeat шлёт в цикле. Если запись уже была «угнана» до этой ревизии (или на
// стенде, где вторая машина успела зарегистрироваться раньше), запрет на
// регистрацию ничего не изменит: чужой агент продолжит слать метрики по тому же
// agentID, и балансер будет по-прежнему показывать его CPU/GPU под записью
// первой машины. Наблюдалось на живой паре: CPU/GPU одной записи переключались
// между двумя машинами от замера к замеру.
//
// Здесь окна «осиротевшей записи» нет намеренно: метрики обязан присылать сам
// владелец. Смена владельца происходит только через регистрацию (см.
// rejectForeignRegistration), которая и обновляет NodeAddr.
func (s *Server) rejectForeignNode(w http.ResponseWriter, r *http.Request, backendID string) bool {
	if backendID == "" {
		return false
	}
	existing := s.proxy.GetBackend(backendID)
	if existing == nil || existing.NodeAddr == "" {
		return false
	}
	src := peerIP(r)
	if src == "" || sameNode(src, existing.NodeAddr) {
		return false
	}

	logger.Get().Warnw("registration guard: rejected telemetry from a node that does not own the backend",
		"backend", backendID,
		"ownerNode", existing.NodeAddr,
		"requestNode", src,
		"path", r.URL.Path,
		"hint", "run only one collector per backend; the external agent belongs to Ollama backends only")

	s.writeJSON(w, http.StatusForbidden, map[string]interface{}{
		"success": false,
		"error": fmt.Sprintf(
			"Backend %q belongs to node %s; telemetry from node %s was rejected. "+
				"Run only one metrics collector per backend (on a worker host that is the embedded agent, not the external `agent` container).",
			backendID, existing.NodeAddr, src),
		"backendId":      backendID,
		"ownerNode":      existing.NodeAddr,
		"registrantNode": src,
	})
	return true
}

// stampNodeAddr — запомнить узел, за которым закреплена запись бэкенда.
//
// Вызывается после успешной регистрации/прикрепления. Пустой addr игнорируется,
// чтобы не затирать уже известный узел, когда источник определить не удалось.
func (s *Server) stampNodeAddr(backendID, addr string) {
	if backendID == "" || addr == "" {
		return
	}
	existing := s.proxy.GetBackend(backendID)
	if existing == nil || existing.NodeAddr == addr {
		return
	}
	updated := *existing
	updated.NodeAddr = addr
	if err := s.proxy.UpdateBackend(backendID, updated); err != nil {
		logger.Get().Warnw("registration guard: failed to stamp node address",
			"backend", backendID, "nodeAddr", addr, "error", err)
	}
	s.warnAmbiguousHost(backendID, updated.Host, addr)
}

// warnAmbiguousHostsAtStartup — тот же контроль, но по уже загруженному
// состоянию: две машины могли зарегистрироваться ДО того, как балансер узнал про
// `NodeAddr`, и после перезапуска регистрации не будет — записи просто читаются из
// state.json. Без стартовой проверки такое расхождение остаётся незамеченным,
// пока кто-нибудь не перезапустит воркер.
func (s *Server) warnAmbiguousHostsAtStartup() {
	byHost := make(map[string][]types.Backend)
	for _, b := range s.proxy.GetAllBackends() {
		if b.Host == "" || b.NodeAddr == "" {
			continue
		}
		byHost[b.Host] = append(byHost[b.Host], b)
	}
	for host, list := range byHost {
		if len(list) < 2 {
			continue
		}
		nodes := make(map[string]bool, len(list))
		for _, b := range list {
			nodes[b.NodeAddr] = true
		}
		if len(nodes) < 2 {
			continue
		}
		ids := make([]string, 0, len(list))
		for _, b := range list {
			ids = append(ids, fmt.Sprintf("%s@%s", b.ID, b.NodeAddr))
		}
		logger.Get().Warnw("registration guard: two different nodes advertise the SAME host — HTTP requests for one of them will go to the wrong machine",
			"host", host,
			"backends", ids,
			"hint", "set BACKEND_HOST=<this machine's LAN address> (and/or SDWORKER_ADVERTISE_HOST) on the remote worker and recreate it: the compose service name is NOT a valid identity across machines")
	}
}

// warnAmbiguousHost — R-MultiHost (2026-10-07): два РАЗНЫХ узла объявили один и
// тот же `host`, и по нему балансер строит URL.
//
// ЗАЧЕМ. Имя хоста внутри docker-сети разрешается через DNS compose — то есть
// `imageworker` на машине-балансере указывает на ЕЁ СОБСТВЕННЫЙ контейнер. Если
// удалённый воркер зарегистрировался с host="imageworker" (SDWORKER_ADVERTISE_HOST
// не доехал, BACKEND_HOST не задан), его запись выглядит живой и «своей», но все
// HTTP-запросы к ней уходят на ПЕРВУЮ машину. Метрики при этом честно приезжают
// от второй (агент шлёт их сам), поэтому в WebUI бэкенд второй машины выглядит
// исправным и показывает ЕЁ GPU. Проверено на живой паре: `/api/v1/image/backends/
// IMAGEWORKER-34/models` возвращал ровно тот же список из 2 моделей, что и
// локальный `imageworker`, тогда как у настоящего воркера машины 2 моделей 0.
//
// Такое не должно проходить молча: предупреждение называет обе записи и говорит,
// что задать.
func (s *Server) warnAmbiguousHost(backendID, host, nodeAddr string) {
	if host == "" || nodeAddr == "" {
		return
	}
	for _, other := range s.proxy.GetAllBackends() {
		if other.ID == backendID || other.Host != host {
			continue
		}
		if other.NodeAddr == "" || other.NodeAddr == nodeAddr {
			continue
		}
		logger.Get().Warnw("registration guard: two different nodes advertise the SAME host — HTTP requests for one of them will go to the wrong machine",
			"host", host,
			"backend", backendID, "node", nodeAddr,
			"otherBackend", other.ID, "otherNode", other.NodeAddr,
			"hint", "set BACKEND_HOST=<this machine's LAN address> (and/or SDWORKER_ADVERTISE_HOST) on the remote worker and recreate it: the compose service name is NOT a valid identity across machines")
		return
	}
}
