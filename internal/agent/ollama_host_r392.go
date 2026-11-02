// ollama_host_r392.go — R392 (2026-11-02): адрес Ollama-бэкенда для балансера
// берётся из OLLAMA_URL, а не из адреса агента.
//
// ЖИВОЙ ДЕФЕКТ. Для BACKEND_TYPE=ollama агент регистрировал бэкенд на
// `publicHost` — то есть на АДРЕС САМОГО АГЕНТА (в compose это имя контейнера
// агента, например `cppworker-gpu-agent`), с портом из OLLAMA_URL. Балансер
// получал запись «ollama на cppworker-gpu-agent:11434» и не мог достучаться до
// Ollama: пробы/инференс уходили в контейнер агента, где Ollama нет.
//
// Теперь хост Ollama — из OLLAMA_URL (это и есть адрес, по которому агент сам
// ходит в Ollama). Особый случай: localhost/127.0.0.1 внутри контейнера агента
// указывает на сам контейнер — такой адрес балансеру бесполезен, поэтому для
// него остаётся publicHost (оператор задаёт его как адрес, видимый с балансера).
package agent

import (
	"net/url"
	"strings"
)

// extractOllamaHost — хост Ollama для регистрации бэкенда.
func (a *Agent) extractOllamaHost() string {
	if a == nil {
		return ""
	}
	raw := strings.TrimSpace(a.config.OllamaURL)
	if raw == "" {
		return a.getPublicHost()
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(u.Hostname()) == "" {
		return a.getPublicHost()
	}
	host := strings.TrimSpace(u.Hostname())
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		// Ollama «на этом же контейнере»: балансеру нужен внешний адрес, а он
		// задаётся оператором в AGENT_PUBLIC_HOST.
		return a.getPublicHost()
	}
	return host
}
