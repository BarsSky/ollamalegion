package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// Round 12 (2026-07-10): tests for agent-attach-to-cppworker dedup.
// Цель: убедиться, что FindBackendByHostPort находит существующий cppworker
// бэкенд, а AttachAgentToBackend корректно прикрепляет к нему агента.

func TestFindBackendByHostPort(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})

	// Add a cppworker backend
	p.AddBackend(types.Backend{
		ID:              "cppworker-gpu-bundled",
		Host:            "cppworker-gpu",
		CppWorkerPort:   18092,
		Type:            types.BackendTypeLlamaCpp,
		Status:          types.StatusHealthy,
		MaxConcurrentReqs: 4,
	})

	// Add a different host backend (should NOT match)
	p.AddBackend(types.Backend{
		ID:              "other-host",
		Host:            "other-host",
		CppWorkerPort:   18092,
		Type:            types.BackendTypeLlamaCpp,
		Status:          types.StatusHealthy,
		MaxConcurrentReqs: 4,
	})

	// Add an Ollama backend with cppWorkerPort=0 (should NOT match)
	p.AddBackend(types.Backend{
		ID:              "ollama-1",
		Host:            "ollama-1",
		OllamaPort:      11434,
		CppWorkerPort:   0,
		Type:            types.BackendTypeOllama,
		Status:          types.StatusHealthy,
		MaxConcurrentReqs: 4,
	})

	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{
			name: "exact match",
			host: "cppworker-gpu",
			port: 18092,
			want: "cppworker-gpu-bundled",
		},
		{
			name: "different host",
			host: "missing-host",
			port: 18092,
			want: "",
		},
		{
			name: "different port",
			host: "cppworker-gpu",
			port: 18093,
			want: "",
		},
		{
			name: "ollama backend (port=0) — функция ищет только cppworker",
			host: "ollama-1",
			port: 0,
			want: "", // cppWorkerPort > 0 filter excludes Ollama backends
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.FindBackendByHostPort(tt.host, tt.port)
			if got != tt.want {
				t.Errorf("FindBackendByHostPort(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestAttachAgentToBackend(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})
	p.AddBackend(types.Backend{
		ID:              "cppworker-gpu-bundled",
		Host:            "cppworker-gpu",
		CppWorkerPort:   18092,
		Type:            types.BackendTypeLlamaCpp,
		Status:          types.StatusHealthy,
		HasAgent:        false,
		MaxConcurrentReqs: 4,
	})

	// Attach agent
	p.AttachAgentToBackend("cppworker-gpu-bundled", "agent-xyz", 18032)

	got := p.GetBackend("cppworker-gpu-bundled")
	if got == nil {
		t.Fatal("backend not found after attach")
	}

	if !got.HasAgent {
		t.Errorf("HasAgent = false, want true")
	}
	if got.AgentID != "agent-xyz" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "agent-xyz")
	}
	if got.AgentPort != 18032 {
		t.Errorf("AgentPort = %d, want 18032", got.AgentPort)
	}
	if got.LastAgentContact.IsZero() {
		t.Error("LastAgentContact should be set after attach")
	}
}

func TestAttachAgentToBackend_NonExistent(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})

	// Attach to non-existent backend — should not panic
	p.AttachAgentToBackend("nonexistent", "agent-1", 18032)

	// No backend should exist
	if p.BackendExists("nonexistent") {
		t.Error("nonexistent backend should not be created by AttachAgentToBackend")
	}
}

// TestAgentAttach_EndToEnd simulates the full bundled-mode flow:
//   1. cppworker registers first (canonical backend)
//   2. agent registers second → should attach, not create
func TestAgentAttach_EndToEnd(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})

	// Step 1: cppworker registers
	p.AddBackend(types.Backend{
		ID:              "cppworker-gpu-bundled",
		Name:            "cppworker-gpu-bundled",
		Host:            "cppworker-gpu",
		CppWorkerPort:   18092,
		Type:            types.BackendTypeLlamaCpp,
		GPUMode:         types.ModeGPU,
		Weight:          100,
		Status:          types.StatusHealthy,
		MaxConcurrentReqs: 4,
		MaxModels:       4,
	})

	// Step 2: agent registers
	// Simulate: find existing by host:port
	existing := p.FindBackendByHostPort("cppworker-gpu", 18092)
	if existing == "" {
		t.Fatal("expected to find cppworker backend, got nothing")
	}
	if existing != "cppworker-gpu-bundled" {
		t.Fatalf("expected to find 'cppworker-gpu-bundled', got %q", existing)
	}

	// Attach
	p.AttachAgentToBackend(existing, "cppworker-gpu-bundled-agent", 18032)

	// Verify: only 1 backend, not 2
	if got := len(p.backends); got != 1 {
		t.Errorf("expected 1 backend (dedup), got %d", got)
	}

	// Verify: backend has agent attached
	backend := p.GetBackend("cppworker-gpu-bundled")
	if !backend.HasAgent {
		t.Error("backend.HasAgent should be true after attach")
	}
	if backend.AgentID != "cppworker-gpu-bundled-agent" {
		t.Errorf("AgentID = %q, want cppworker-gpu-bundled-agent", backend.AgentID)
	}
	if backend.AgentPort != 18032 {
		t.Errorf("AgentPort = %d, want 18032", backend.AgentPort)
	}
}

// TestAgentAttach_StandaloneMode validates fallback: if no cppworker backend
// exists, agent can still register its own (standalone mode — agent without
// bundled cppworker, e.g. legacy Ollama setup).
func TestAgentAttach_StandaloneMode(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})

	// No cppworker backend exists. Agent tries to register with CppWorkerPort=0.
	existing := p.FindBackendByHostPort("agent-standalone", 0)
	if existing != "" {
		t.Fatalf("expected no existing backend, got %q", existing)
	}

	// Agent should create its own backend (legacy flow).
	p.AddBackend(types.Backend{
		ID:              "agent-standalone",
		Host:            "agent-standalone",
		OllamaPort:      11434,
		AgentPort:       18032,
		Type:            types.BackendTypeOllama,
		Status:          types.StatusHealthy,
		MaxConcurrentReqs: 4,
	})

	if got := len(p.backends); got != 1 {
		t.Errorf("expected 1 backend (standalone), got %d", got)
	}
}