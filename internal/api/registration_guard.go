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
		// ::ffff:192.168.13.34 → 192.168.13.34: один и тот же узел не должен
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
func registrationOwnerIsLive(existing *types.Backend) bool {
	if existing == nil {
		return false
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
	if !registrationOwnerIsLive(existing) {
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
}
