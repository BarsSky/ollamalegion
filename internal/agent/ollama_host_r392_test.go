package agent

// ollama_host_r392_test.go — R392 (2026-11-02): для ollama-бэкенда балансеру
// отдаётся адрес OLLAMA_URL, а не адрес самого агента.
//
// Живой дефект: в compose агент регистрировал Ollama как `<agent>:11434`
// (publicHost = имя контейнера агента), и балансер не мог до неё достучаться.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR392_OllamaHostFromURL(t *testing.T) {
	base := func(ollamaURL, publicHost string) *Agent {
		return &Agent{config: &types.AgentConfig{
			OllamaURL:  ollamaURL,
			PublicHost: publicHost,
		}}
	}

	cases := []struct {
		name       string
		ollamaURL  string
		publicHost string
		want       string
	}{
		{"полный URL", "http://192.168.13.5:11434", "ollama-agent", "192.168.13.5"},
		{"без схемы", "192.168.13.5:11434", "ollama-agent", "192.168.13.5"},
		{"https", "https://ollama.lan:11434", "ollama-agent", "ollama.lan"},
		{"docker-хост", "http://host.docker.internal:11434", "ollama-agent", "host.docker.internal"},
		{"порт по умолчанию", "http://10.0.0.7", "ollama-agent", "10.0.0.7"},
		// localhost внутри контейнера агента — это сам контейнер; такой адрес
		// балансеру бесполезен, отдаём публичный адрес агента.
		{"localhost → publicHost", "http://localhost:11434", "ollama-host.lan", "ollama-host.lan"},
		{"127.0.0.1 → publicHost", "http://127.0.0.1:11434", "ollama-host.lan", "ollama-host.lan"},
		{"пустой URL → publicHost", "", "ollama-host.lan", "ollama-host.lan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base(tc.ollamaURL, tc.publicHost)
			if got := a.extractOllamaHost(); got != tc.want {
				t.Fatalf("extractOllamaHost(%q, public=%q) = %q, ожидалось %q",
					tc.ollamaURL, tc.publicHost, got, tc.want)
			}
		})
	}
}

// TestR392_OllamaPortStillFromURL — порт продолжает браться из OLLAMA_URL
// (регрессия не задевает уже работающее поведение).
func TestR392_OllamaPortStillFromURL(t *testing.T) {
	a := &Agent{config: &types.AgentConfig{OllamaURL: "http://192.168.13.5:11500"}}
	if got := a.extractOllamaPort(); got != 11500 {
		t.Fatalf("extractOllamaPort = %d, ожидалось 11500", got)
	}
	a = &Agent{config: &types.AgentConfig{OllamaURL: "http://192.168.13.5"}}
	if got := a.extractOllamaPort(); got != 11434 {
		t.Fatalf("extractOllamaPort без порта = %d, ожидалось 11434", got)
	}
}
