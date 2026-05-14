package agent

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// getPublicHost - определение публичного хоста для регистрации
// Приоритет: AGENT_PUBLIC_HOST > auto-detected IP > hostname
func (a *Agent) getPublicHost() string {
	// Если явно задан PublicHost — используем его
	if a.config.PublicHost != "" {
		return a.config.PublicHost
	}

	// Попытка получить публичный IP
	publicIP := a.getOutboundIP()
	if publicIP != "" && publicIP != "127.0.0.1" {
		return publicIP
	}

	// Fallback на hostname
	hostname, _ := os.Hostname()
	if hostname != "" {
		return hostname
	}

	return "localhost"
}

// getOutboundIP - получение исходящего IP адреса (публичного интерфейса)
func (a *Agent) getOutboundIP() string {
	// Подключаемся к произвольному внешнему адресу для определения исходящего интерфейса
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr == nil {
		return ""
	}
	return localAddr.IP.String()
}

// extractOllamaPort - извлечение порта Ollama из OllamaURL
func (a *Agent) extractOllamaPort() int {
	if a.config.OllamaURL != "" {
		// Парсим URL вида http://host:port или host:port
		urlStr := a.config.OllamaURL
		if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
			urlStr = "http://" + urlStr
		}
		if u, err := url.Parse(urlStr); err == nil && u.Port() != "" {
			if port, err := strconv.Atoi(u.Port()); err == nil {
				return port
			}
		}
	}
	return 11434 // fallback
}

// getOllamaBaseURL - базовый URL Ollama из конфигурации
func (a *Agent) getOllamaBaseURL() string {
	if a.config.OllamaURL != "" {
		return a.config.OllamaURL
	}
	return "http://localhost:11434"
}