//go:build llama_stub

// layout_verify_r176_test.go — R176 (2026-10-19).
//
// Закрывает дефект, локализованный в R174: при «Применить профиль» серверы
// раскладки передавались в мост, бюджет их учитывал, но llama.cpp на загрузке
// печатал только `using device CUDA0` — узел не становился устройством модели, и
// все слои уезжали на локальную карту. Загрузчик молчал: он не сверял
// «передали» с «зарегистрировалось».
package cppbackend

import (
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestR176_LayoutRegistration — сверка переданных серверов с фактом раскладки.
func TestR176_LayoutRegistration(t *testing.T) {
	servers := []string{"192.0.2.10:50052"}

	cases := []struct {
		name       string
		servers    []string
		layers     []bridge.DeviceLayerRange
		wantDegrad bool
		wantInMsg  string
	}{
		{
			name:    "раскладка есть: RPC0 получил слои",
			servers: servers,
			layers: []bridge.DeviceLayerRange{
				{Device: "CPU", Count: 5},
				{Device: "CUDA0", Count: 21},
				{Device: "RPC0", Count: 17, Node: "192.0.2.10:50052"},
			},
			wantDegrad: false,
		},
		{
			name:    "авария R174: передан один сервер, RPC-устройства нет",
			servers: servers,
			layers: []bridge.DeviceLayerRange{
				{Device: "CPU", Count: 1},
				{Device: "CUDA0", Count: 42},
			},
			wantDegrad: true,
			wantInMsg:  "192.0.2.10:50052",
		},
		{
			name:    "RPC-устройство есть, но слоёв на нём 0 (тоже деградация)",
			servers: servers,
			layers: []bridge.DeviceLayerRange{
				{Device: "CUDA0", Count: 42},
				{Device: "RPC0", Count: 0, Node: "192.0.2.10:50052"},
			},
			wantDegrad: true,
		},
		{
			name:    "раскладку не настраивали — сверять нечего",
			servers: nil,
			layers:  []bridge.DeviceLayerRange{{Device: "CUDA0", Count: 42}},
		},
		{
			name:    "регистр имени устройства не важен",
			servers: servers,
			layers:  []bridge.DeviceLayerRange{{Device: "rpc0", Count: 3}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			degraded, reason := layoutRegistration(c.servers, c.layers)
			if degraded != c.wantDegrad {
				t.Fatalf("degraded = %v, want %v (reason: %s)", degraded, c.wantDegrad, reason)
			}
			if !degraded {
				if reason != "" {
					t.Errorf("при нормальной раскладке причина должна быть пустой, got %q", reason)
				}
				return
			}
			if reason == "" {
				t.Fatal("деградация без причины: оператор не узнает, почему слои не на узле")
			}
			if c.wantInMsg != "" && !strings.Contains(reason, c.wantInMsg) {
				t.Errorf("в причине нет адреса узла %q: %s", c.wantInMsg, reason)
			}
		})
	}
}

// TestR176_SplitFitsDevices — сплит применяется только к тому числу устройств,
// для которого посчитан (иначе пропорции молча игнорируются).
func TestR176_SplitFitsDevices(t *testing.T) {
	cases := []struct {
		name  string
		split []float32
		devs  int
		want  bool
	}{
		{"сплита нет — пересчитывать нечего", nil, 1, true},
		{"два устройства, два коэффициента", []float32{0.5, 0.5}, 2, true},
		{"авария R174: сплит на 2 устройства, устройство одно", []float32{0.538, 0.462}, 1, false},
		{"сплит на 1 устройство, устройств два", []float32{1}, 2, false},
		{"устройств не определили", []float32{0.5, 0.5}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitFitsDevices(c.split, c.devs); got != c.want {
				t.Errorf("splitFitsDevices(%v, %d) = %v, want %v", c.split, c.devs, got, c.want)
			}
		})
	}
}
