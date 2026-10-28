package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// requestUserIdentity — R67b (2026-09-23): идентификатор пользователя из
// заголовков. OpenWebUI (и любой прокси перед ним) может передавать
// X-User-Id / X-OpenWebUI-User-Id / X-User-Email; балансер обязан их учитывать,
// иначе все пользователи OpenWebUI (один IP, один Authorization-токен)
// сливаются в одну сессию. См. r67b_session_identity_test.go.
func requestUserIdentity(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, h := range []string{"X-User-Id", "X-OpenWebUI-User-Id", "X-User-Email"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

// firstStringField — первое непустое строковое поле из набора ключей.
func firstStringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// requestBodyIdentity — R67b: user/chat идентификаторы из тела запроса.
//
// Источники (по убыванию приоритета):
//   - top-level: user / user_id, chat_id / conversation_id / session_id;
//   - nested metadata: metadata.user_id, metadata.chat_id, metadata.session_id —
//     именно так OpenWebUI (OpenAI-совместимый режим) помечает запрос.
//
// Тело читается один раз (до 1 MiB) и ВОССТАНАВЛИВАЕТСЯ через io.NopCloser,
// так что вызывающий код может читать его снова.
func requestBodyIdentity(r *http.Request) (user, chat string) {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return "", ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if len(body) == 0 || len(body) > 1<<20 {
		return "", ""
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	user = firstStringField(req, "user", "user_id", "userId", "user_email")
	chat = firstStringField(req, "chat_id", "chatId", "conversation_id", "conversationId", "session_id")
	if md, ok := req["metadata"].(map[string]interface{}); ok {
		if user == "" {
			user = firstStringField(md, "user_id", "userId", "user", "user_email")
		}
		if chat == "" {
			chat = firstStringField(md, "chat_id", "chatId", "conversation_id", "conversationId", "session_id")
		}
	}
	return user, chat
}

// extractChatIDFromBody — пытается извлечь идентификатор чата из запроса.
// Используется в первую очередь для OpenWebUI, который может передавать
// chat_id в заголовке (X-Chat-Id / X-Conversation-Id) или в теле запроса
// (chat_id / conversation_id / session_id, в т.ч. внутри metadata).
// Это позволяет балансеру различать разные чаты одного пользователя, которые
// иначе имели бы одинаковый sessionID.
//
// Функция читает тело через io.ReadAll и ВОССТАНАВЛИВАЕТ r.Body
// через io.NopCloser — вызывающий код может перечитать тело без проблем.
// Возвращает "", если ничего не нашли.
func extractChatIDFromBody(r *http.Request) string {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return ""
	}
	// Сначала пробуем заголовки (быстрее, без чтения тела).
	for _, h := range []string{"X-Chat-Id", "X-Chat-ID", "X-Conversation-Id", "X-Conversation-ID"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	_, chat := requestBodyIdentity(r)
	return chat
}

// defaultTrustedProxies — стандартные доверенные сети: loopback, Docker bridge, RFC1918
var defaultTrustedProxies = []string{
	"127.0.0.1/8",
	"::1/128",
	"172.17.0.0/16", // Docker default bridge
	"172.18.0.0/16", // Docker compose bridge
	"172.19.0.0/16",
	"172.20.0.0/16",
	"172.21.0.0/16",
	"172.22.0.0/16",
	"172.23.0.0/16",
	"172.24.0.0/16",
	"172.25.0.0/16",
	"172.26.0.0/16",
	"172.27.0.0/16",
	"172.28.0.0/16",
	"172.29.0.0/16",
	"172.30.0.0/16",
	"172.31.0.0/16",
	"10.0.0.0/8",
	"192.168.0.0/16",
	"fc00::/7", // IPv6 ULA
}

// parseTrustedProxyCIDRs — парсит CIDR из конфига + дефолты.
// Вызывается один раз при создании Proxy, результат кешируется в p.trustedNets.
func parseTrustedProxyCIDRs(configCIDRs []string) []*net.IPNet {
	raw := configCIDRs
	if len(raw) == 0 {
		raw = defaultTrustedProxies
	}
	var nets []*net.IPNet
	for _, s := range raw {
		_, ipnet, err := net.ParseCIDR(s)
		if err == nil {
			nets = append(nets, ipnet)
			continue
		}
		ip := net.ParseIP(s)
		if ip != nil {
			if ip.To4() != nil {
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
			} else {
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
			}
		}
	}
	return nets
}

// isTrustedProxy — проверяет, что IP принадлежит доверенной сети.
// Использует кешированные trustedNets, распарсенные при создании Proxy.
func (p *Proxy) isTrustedProxy(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	for _, n := range p.trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// getClientRealIP - извлечение реального IP клиента с учётом reverse proxy заголовков.
// Поддерживает AlwaysTrustProxyHeaders для окружений где балансер всегда за reverse proxy.
func (p *Proxy) getClientRealIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	// AlwaysTrustProxyHeaders — флаг для окружений где балансер всегда за nginx/traefik
	// и RemoteAddr всегда от доверенного прокси. Принудительно доверяем XFF даже без проверки trustedProxies.
	alwaysTrust := p.config.LoadBalancer.AlwaysTrustProxyHeaders

	// Если прямой TCP-соединитель не из доверенных сетей и alwaysTrust выключен —
	// игнорируем XFF/X-Real-IP (защита от подделки заголовков при прямом доступе)
	trustHeaders := alwaysTrust || p.isTrustedProxy(remoteIP)

	headers := p.config.LoadBalancer.ClientIPHeaders
	if len(headers) == 0 {
		headers = []string{"X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP"}
	}

	if trustHeaders {
		for _, h := range headers {
			val := r.Header.Get(h)
			if val == "" {
				continue
			}
			// X-Forwarded-For: client, proxy1, proxy2... — берём первый (реальный клиент)
			if strings.EqualFold(h, "X-Forwarded-For") {
				parts := strings.Split(val, ",")
				if len(parts) > 0 {
					ip := strings.TrimSpace(parts[0])
					if ip != "" {
						logger.Get().Debugw("getClientRealIP: using X-Forwarded-For",
							"remote_addr", remoteIP, "header", h, "value", val, "extracted_ip", ip,
							"trusted", trustHeaders, "always_trust", alwaysTrust)
						return ip
					}
				}
			} else {
				ip := strings.TrimSpace(val)
				if ip != "" {
					logger.Get().Debugw("getClientRealIP: using proxy header",
						"remote_addr", remoteIP, "header", h, "extracted_ip", ip,
						"trusted", trustHeaders, "always_trust", alwaysTrust)
					return ip
				}
			}
		}
	}

	// Fallback: RemoteAddr (IP прямого TCP-соединения)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logger.Get().Debugw("getClientRealIP: using remote_addr fallback",
			"remote_addr", r.RemoteAddr, "trusted", trustHeaders,
			"always_trust", alwaysTrust, "headers_checked", headers)
		return r.RemoteAddr
	}
	logger.Get().Debugw("getClientRealIP: using remote_addr",
		"remote_addr", ip, "trusted", trustHeaders,
		"always_trust", alwaysTrust, "headers_checked", headers)
	return ip
}

// getClientFingerprint - генерация fingerprint клиента для разделения сессий
// за одним IP (NAT/прокси). Использует X-Client-ID, Authorization header,
// X-Session-ID, или генерирует fingerprint из комбинации IP + User-Agent.
func (p *Proxy) getClientFingerprint(r *http.Request) string {
	// Приоритет 0 (R67b, 2026-09-23): явный пользователь из заголовков.
	// OpenWebUI проксирует всех пользователей с одного IP, с одним User-Agent и
	// одним Authorization-токеном, поэтому без этого все они получали один и
	// тот же fingerprint (в живом стенде 3 запроса от 3 пользователей дали одну
	// сессию с requestCount=3).
	if user := requestUserIdentity(r); user != "" {
		h := sha256.Sum256([]byte("user:" + user))
		return "usr:" + hex.EncodeToString(h[:8])
	}
	// Приоритет 1: X-Client-ID — явный идентификатор клиента (Cline передаёт)
	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		h := sha256.Sum256([]byte(clientID))
		return "cid:" + hex.EncodeToString(h[:8])
	}
	// Приоритет 2: Bearer token из Authorization (OpenWebUI передаёт токен на пользователя)
	if auth := r.Header.Get("Authorization"); auth != "" {
		h := sha256.Sum256([]byte(auth))
		return "tkn:" + hex.EncodeToString(h[:8])
	}
	// Приоритет 3: X-Session-ID — уже назначенный балансером session ID
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		h := sha256.Sum256([]byte(sessionID))
		return "sid:" + hex.EncodeToString(h[:8])
	}
	// Fallback: хеш от IP + User-Agent (без порта — стабильный для одного агента)
	// NOTE: клиенты за одним NAT/IP с одинаковым UA будут иметь одинаковый fingerprint.
	// Для различения им следует передавать X-Client-ID или уникальный Authorization token.
	ip := p.getClientRealIP(r)
	ua := r.UserAgent()
	h := sha256.Sum256([]byte(ip + "::" + ua))
	return "fp:" + hex.EncodeToString(h[:8])
}

// getSessionID — УДАЛЁН в R252 (2026-10-28).
//
// Он был мёртв в production (ни одного вызова вне этого файла) и при этом
// ДУБЛИРОВАЛ начало getSessionIDWithModel другим составом ключа: без модели, без
// типа эндпоинта и без разговора. Два похожих резолвера в одном файле — это
// приглашение выбрать «не тот» (именно так уже терялась привязка к бэкенду).
// Оставлен один: getSessionIDWithModel ниже.

// getSessionIDWithModel - версия с учётом модели (используется при создании сессии)
// ВАЖНО: X-Client-ID комбинируется с IP и моделью, т.к. разные Cline-клиенты
// отправляют одинаковый X-Client-ID, что приводило к слипанию сессий.
// Также учитываются X-Tab-ID, X-Request-ID, chat_id и тип Ollama-эндпоинта
// для различения параллельных запросов от одного клиента (разные вкладки/чаты).
func (p *Proxy) getSessionIDWithModel(r *http.Request, clientName, model string) string {
	realIP := p.getClientRealIP(r)

	// Извлекаем тип endpoint для разделения сессий chat / generate / embed
	endpointType := p.getOllamaEndpointType(r.URL.Path)

	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		// Приоритет 1: X-Client-ID + IP + модель + endpoint type + tab/request ID
		tabID := r.Header.Get("X-Tab-ID")
		requestID := r.Header.Get("X-Request-ID")
		composite := clientID + "::" + realIP + "::" + model + "::" + endpointType
		if tabID != "" {
			composite += "::tab:" + tabID
		}
		if requestID != "" {
			composite += "::req:" + requestID
		}
		h := sha256.Sum256([]byte(composite))
		return "cid:" + hex.EncodeToString(h[:16]) + "::" + model
	}
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID + "::" + model + "::" + endpointType
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value + "::" + model + "::" + endpointType
	}

	// R67b (2026-09-23): явный пользователь (заголовок или тело/metadata) —
	// РАНЬШЕ chat_id и fingerprint. Иначе несколько пользователей OpenWebUI
	// (один IP, один Authorization-токен, разные чаты) либо сливались в одну
	// сессию, либо «разделялись» только по чату, и жалоба «балансер не
	// различает разных пользователей OpenWebUI» оставалась в силе.
	// Порядок: заголовок X-User-Id → user в теле → metadata.user_id.
	bodyUser, _ := requestBodyIdentity(r)
	userID := requestUserIdentity(r)
	if userID == "" {
		userID = bodyUser
	}
	if userID != "" {
		composite := "user:" + userID + "::" + model + "::" + endpointType
		if chatID := extractChatIDFromBody(r); chatID != "" {
			composite += "::chat:" + chatID
		}
		h := sha256.Sum256([]byte(composite))
		return "usr:" + hex.EncodeToString(h[:16]) + "::" + model
	}

	// Приоритет 3: chat_id из заголовка или тела запроса (OpenWebUI / LiteLLM).
	// Без этого все чаты одного пользователя OpenWebUI мапились бы в одну
	// сессию (fp + clientName совпадают), что приводило к попаданию в чужую
	// привязку backendID и «пустым» ответам при смене чата.
	if chatID := extractChatIDFromBody(r); chatID != "" {
		h := sha256.Sum256([]byte("chat::" + chatID + "::" + realIP + "::" + endpointType))
		return "chat:" + hex.EncodeToString(h[:16]) + "::" + model
	}

	fp := p.getClientFingerprint(r)

	// R118.9 (2026-10-14): ДВА ЭКЗЕМПЛЯРА КЛИЕНТА С ОДНОЙ МАШИНЫ.
	//
	// Живая жалоба: «запрос от одной машины, но от двух экземпляров клиента Cline —
	// балансер не разделяет». Отпечаток `fp` (IP+UA) и имя клиента у них совпадают,
	// поэтому до этой правки оба экземпляра попадали в ОДНУ сессию: общая привязка к
	// бэкенду, общий кошелёк справедливости в admission-очереди, одна строка в
	// панели. Различаем их по НАЧАЛУ РАЗГОВОРА (см. session_instance_r118.go):
	// Cline пересылает историю целиком, поэтому системная инструкция + первая задача
	// стабильны внутри задачи и различаются между задачами/экземплярами.
	if conv := conversationFingerprintFromRequest(r); conv != "" {
		composite := fp + "::" + clientName + "::" + model + "::" + endpointType + "::conv:" + conv
		h := sha256.Sum256([]byte(composite))
		candidate := "conv:" + hex.EncodeToString(h[:16]) + "::" + model
		// R252 (2026-10-28): СЖАТИЕ ИСТОРИИ НЕ ДОЛЖНО НАЧИНАТЬ НОВУЮ СЕССИЮ.
		//
		// ЖИВОЙ ДЕФЕКТ (Cline, длинная задача). Cline при переполнении окна
		// переписывает начало разговора (урезает/суммирует историю). Отпечаток
		// начала (`conv`) меняется → получался НОВЫЙ sessionID: старая строка
		// висела в панели до idle TTL, работа продолжалась в новой, а «кошелёк»
		// admission-очереди (ключ = sessionID) обнулялся посреди задачи. Снаружи
		// это выглядело как «балансер потерял клиента / начал другой».
		//
		// Правило переиспользования (R252.1): тот же клиент (IP+имя), та же модель и
		// тип эндпоинта, и при этом РАЗГОВОР ЯВЛЯЕТСЯ ПРОДОЛЖЕНИЕМ — совпал первый
		// system-промпт (он переживает сжатие истории) ИЛИ новое первое
		// user-сообщение содержит/начинается с сохранённого хвоста прежнего
		// (Cline переносит содержимое задачи в сводку дословно).
		//
		// ЧЕГО ЗДЕСЬ БОЛЬШЕ НЕТ: признака «активный стрим» как условия. Cline
		// сжимает историю МЕЖДУ ходами, когда стрима уже нет, — на этом условии
		// реальный сценарий не покрывался вовсе.
		//
		// Явные идентификаторы клиента (X-Client-ID / X-Session-ID / cookie /
		// user / chat_id) сюда не попадают: у них ветки выше, и там своя семантика.
		parts := conversationPartsFromRequest(r)
		if sid := p.resolveConversationSessionID(candidate, realIP, clientName, model, endpointType, conv, parts); sid != "" {
			candidate = sid
		}
		p.recordSessionIdentity(candidate, realIP, clientName, model, endpointType, conv, parts)
		return candidate
	}

	return fp + "::" + clientName + "::" + model + "::" + endpointType
}

// resolveConversationSessionID — R252: переиспользовать id существующей сессии,
// если это тот же клиент/модель и продолжение того же разговора.
//
// Возвращает "" (нет переиспользования) или id существующей сессии. Правило
// сравнения живёт в ОДНОМ месте: иначе «устойчивость к сжатию» будет реализована
// по-разному на разных путях — ровно та болезнь, что лечится в R252.
//
// МАТЧЕР НЕ ЗОВЁТ МЕТОДЫ МЕНЕДЖЕРА: он выполняется под его RLock (R252.1 — без
// копирования карты сессий в срез). Идентичность приходит вторым аргументом.
func (p *Proxy) resolveConversationSessionID(candidate, realIP, clientName, model, endpointType, conv string, parts conversationParts) string {
	if p == nil || p.sessionMgr == nil {
		return ""
	}
	return p.sessionMgr.ResolveID(candidate, func(s *types.Session, ident SessionIdentity) bool {
		if ident.IP != realIP || ident.Client != clientName ||
			ident.Model != model || ident.EndpointType != endpointType {
			return false
		}
		// «Сильное» продолжение: начало разговора не изменилось вообще.
		strong := conv != "" && conv == ident.ConvKey
		// Хвост прежнего первого user-сообщения встречается в новом — сводка
		// содержит прежний текст дословно.
		tailContinuation := ident.UserTail != "" && parts.UserHead != "" &&
			strings.Contains(parts.UserHead, ident.UserTail)
		if strong || tailContinuation {
			return true
		}
		// Слабое продолжение — совпал system-промпт (workspace/режим/инструкции).
		systemSame := ident.SystemHead != "" && ident.SystemHead == parts.SystemHead
		if !systemSame {
			return false
		}
		// R252.1 / БЛОКЕР 2: если у сессии ИДЁТ стрим и разговор не подтверждён
		// сильным признаком, это ДРУГАЯ параллельная задача того же клиента (второе
		// окно Cline, переключение Plan/Act): не прилипаем. Иначе вернулся бы
		// регресс R118.9 («два экземпляра слились в одну сессию»).
		if s.HasActiveStream {
			return false
		}
		// R252.2: одного system-промпта НЕДОСТАТОЧНО. В одном workspace две РАЗНЫЕ
		// последовательные задачи (закончил одну — начал другую) имеют одинаковый
		// system, и weak-правило склеивало их: новая задача наследовала чужой
		// кошелёк admission и чужую строку в панели.
		//
		// Продолжением считаем только при ДОПОЛНИТЕЛЬНОМ признаке:
		//   - явный маркер сжатия истории в новом тексте (сводка);
		//   - существенное пересечение сохранённых частей разговора;
		//   - тот же task-id из environment_details.
		if parts.UserHasSummaryMarker {
			return true
		}
		if ident.TaskID != "" && parts.TaskID != "" && ident.TaskID == parts.TaskID {
			return true
		}
		return conversationOverlap(parts, identConversationParts(ident), conversationOverlapMinRunes)
	})
}

// identConversationParts — сохранённые признаки разговора из SessionIdentity.
func identConversationParts(ident SessionIdentity) conversationParts {
	return conversationParts{
		SystemHead: ident.SystemHead,
		UserHead:   ident.UserHead,
		UserTail:   ident.UserTail,
	}
}

// recordSessionIdentity — R252: запомнить «кто/какой разговор» по id сессии.
func (p *Proxy) recordSessionIdentity(sessionID, realIP, clientName, model, endpointType, conv string, parts conversationParts) {
	if p == nil || p.sessionMgr == nil || sessionID == "" {
		return
	}
	p.sessionMgr.RecordIdentity(sessionID, SessionIdentity{
		IP:                   realIP,
		Client:               clientName,
		Model:                model,
		EndpointType:         endpointType,
		SystemHead:           parts.SystemHead,
		UserHead:             parts.UserHead,
		UserTail:             parts.UserTail,
		UserHasSummaryMarker: parts.UserHasSummaryMarker,
		TaskID:               parts.TaskID,
		ConvKey:              conv,
	})
}

// getOllamaEndpointType — определяет тип Ollama эндпоинта для разделения сессий.
// Разные типы запросов (/api/chat, /api/generate, /api/embed) не должны
// блокировать друг друга в рамках одного клиента.
func (p *Proxy) getOllamaEndpointType(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/chat"):
		return "chat"
	case strings.HasPrefix(path, "/api/generate"):
		return "gen"
	case strings.HasPrefix(path, "/api/embed"):
		return "emb"
	default:
		return "api"
	}
}

// getClientName - извлечение имени клиента из запроса (Cline, OpenWebUI, etc.)
func (p *Proxy) getClientName(r *http.Request) string {
	if name := r.Header.Get("X-Client-Name"); name != "" {
		return name
	}
	ua := r.UserAgent()
	if strings.Contains(ua, "cline") || strings.Contains(ua, "Cline") {
		return "Cline"
	}
	if strings.Contains(ua, "open-webui") || strings.Contains(ua, "OpenWebUI") {
		return "OpenWebUI"
	}
	return ua
}
