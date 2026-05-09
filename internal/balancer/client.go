package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

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

// initTrustedProxies — парсит CIDR из конфига + дефолты
func (p *Proxy) initTrustedProxies() []*net.IPNet {
	raw := p.config.LoadBalancer.TrustedProxies
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

// isTrustedProxy — проверяет, что IP принадлежит доверенной сети
func (p *Proxy) isTrustedProxy(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	nets := p.initTrustedProxies()
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// getClientRealIP - извлечение реального IP клиента с учётом reverse proxy заголовков
func (p *Proxy) getClientRealIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	// Если прямой TCP-соединитель не из доверенных сетей — игнорируем XFF/X-Real-IP
	// (защита от подделки заголовков при прямом доступе)
	trustHeaders := p.isTrustedProxy(remoteIP)

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
							"remote_addr", remoteIP, "header", h, "value", val, "extracted_ip", ip, "trusted", trustHeaders)
						return ip
					}
				}
			} else {
				ip := strings.TrimSpace(val)
				if ip != "" {
					logger.Get().Debugw("getClientRealIP: using proxy header",
						"remote_addr", remoteIP, "header", h, "extracted_ip", ip, "trusted", trustHeaders)
					return ip
				}
			}
		}
	}

	// Fallback: RemoteAddr (IP прямого TCP-соединения)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logger.Get().Debugw("getClientRealIP: using remote_addr fallback",
			"remote_addr", r.RemoteAddr, "trusted", trustHeaders, "headers_checked", headers)
		return r.RemoteAddr
	}
	logger.Get().Debugw("getClientRealIP: using remote_addr",
		"remote_addr", ip, "trusted", trustHeaders, "headers_checked", headers)
	return ip
}

// getClientFingerprint - генерация fingerprint клиента для разделения сессий
// за одним IP (NAT/прокси). Использует Authorization header, Bearer token,
// или генерирует случайный fingerprint из комбинации IP + User-Agent хеша.
func (p *Proxy) getClientFingerprint(r *http.Request) string {
	// Приоритет: Bearer token из Authorization (OpenWebUI передаёт токен)
	if auth := r.Header.Get("Authorization"); auth != "" {
		h := sha256.Sum256([]byte(auth))
		return "tkn:" + hex.EncodeToString(h[:8])
	}
	// Fallback: хеш от IP + User-Agent (лучше чем просто IP)
	ip := p.getClientRealIP(r)
	ua := r.UserAgent()
	h := sha256.Sum256([]byte(ip + "::" + ua))
	return "fp:" + hex.EncodeToString(h[:8])
}

// getSessionID - получение ID сессии из запроса (стабильный, без эфемерного порта)
func (p *Proxy) getSessionID(r *http.Request, clientName string) string {
	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		return clientID
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
func (p *Proxy) getSessionIDWithModel(r *http.Request, clientName, model string) string {
	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		return clientID + "::" + model
	}
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID + "::" + model
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value + "::" + model
	}

	fp := p.getClientFingerprint(r)
	return fp + "::" + clientName + "::" + model
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