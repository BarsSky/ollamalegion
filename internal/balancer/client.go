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
)

// extractChatIDFromBody — пытается извлечь идентификатор чата из запроса.
// Используется в первую очередь для OpenWebUI, который может передавать
// chat_id в заголовке (X-Chat-Id / X-Conversation-Id) или в теле запроса
// (chat_id / conversation_id). Это позволяет балансеру различать разные
// чаты одного пользователя, которые иначе имели бы одинаковый sessionID.
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
	// Затем — тело (если оно не слишком большое).
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if len(body) == 0 || len(body) > 1<<20 { // 1 MiB — выше не имеет смысла
		return ""
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for _, key := range []string{"chat_id", "conversation_id", "chatId", "conversationId"} {
		if v, ok := req[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
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

// getSessionID - получение ID сессии из запроса (стабильный, без эфемерного порта)
// ВАЖНО: X-Client-ID комбинируется с IP и clientName, т.к. разные Cline-клиенты
// отправляют одинаковый X-Client-ID, что приводило к слипанию сессий.
func (p *Proxy) getSessionID(r *http.Request, clientName string) string {
	realIP := p.getClientRealIP(r)

	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		// Комбинируем X-Client-ID с реальным IP — гарантирует уникальность сессий
		// для разных клиентов за разными IP, даже с одинаковым X-Client-ID
		h := sha256.Sum256([]byte(clientID + "::" + realIP))
		return "cid:" + hex.EncodeToString(h[:16])
	}
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value
	}

	fp := p.getClientFingerprint(r)
	return fp + "::" + clientName
}

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

	// Приоритет 3: chat_id из заголовка или тела запроса (OpenWebUI / LiteLLM).
	// Без этого все чаты одного пользователя OpenWebUI мапились бы в одну
	// сессию (fp + clientName совпадают), что приводило к попаданию в чужую
	// привязку backendID и «пустым» ответам при смене чата.
	if chatID := extractChatIDFromBody(r); chatID != "" {
		h := sha256.Sum256([]byte("chat::" + chatID + "::" + realIP + "::" + endpointType))
		return "chat:" + hex.EncodeToString(h[:16]) + "::" + model
	}

	fp := p.getClientFingerprint(r)
	return fp + "::" + clientName + "::" + model + "::" + endpointType
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