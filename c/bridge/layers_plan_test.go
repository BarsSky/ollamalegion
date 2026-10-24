//go:build llama_stub

// layers_plan_test.go — R118.16 (2026-10-14).
//
// Разбор строк llama.cpp о раскладке слоёв: это ВХОД для точной (а не расчётной)
// раскладки в WebUI. Тесты фиксируют форматы, которые llama.cpp реально печатает
// (взяты из живого лога контейнера), устойчивость к мусорным строкам и сборку
// непрерывных диапазонов по устройствам.
package bridge

import (
	"testing"
)

// Живые строки из лога cppworker (загрузка gemma-4-E4B с раскладкой на узел).
const (
	liveLineCUDA = "load_tensors: layer  33 assigned to device CUDA0, is_swa = 1"
	liveLineRPC  = "load_tensors: layer  39 assigned to device RPC0, is_swa = 0"
	liveLineCPU  = "load_tensors: layer  42 assigned to device CPU, is_swa = 0"
	liveLinePad  = "[bridge] load_tensors: layer  7 assigned to device CUDA0"
)

func TestR118_16_ParseLayerAssignment(t *testing.T) {
	cases := []struct {
		line string
		want int
		dev  string
		ok   bool
	}{
		{liveLineCUDA, 33, "CUDA0", true},
		{liveLineRPC, 39, "RPC0", true},
		{liveLineCPU, 42, "CPU", true},
		{liveLinePad, 7, "CUDA0", true},
		{"load_tensors: loading model tensors", 0, "", false},
		{"", 0, "", false},
		{"assigned to device CUDA0", 0, "", false},
		{"load_tensors: layer abc assigned to device CUDA0", 0, "", false},
	}
	for _, c := range cases {
		layer, dev, ok := parseLayerAssignment(c.line)
		if ok != c.ok || layer != c.want || dev != c.dev {
			t.Errorf("parse(%q) = (%d, %q, %v), want (%d, %q, %v)",
				c.line, layer, dev, ok, c.want, c.dev, c.ok)
		}
	}
}

func TestR118_16_SnapshotAggregatesRanges(t *testing.T) {
	resetDeviceLayers()
	// 1..3 CUDA0, 4..5 RPC0, 6 CPU — как после настоящей загрузки.
	for _, line := range []string{
		"load_tensors: layer   1 assigned to device CUDA0, is_swa = 0",
		"load_tensors: layer   2 assigned to device CUDA0, is_swa = 1",
		"load_tensors: layer   3 assigned to device CUDA0, is_swa = 1",
		"load_tensors: layer   4 assigned to device RPC0, is_swa = 0",
		"load_tensors: layer   5 assigned to device RPC0, is_swa = 1",
		"load_tensors: layer   6 assigned to device CPU, is_swa = 0",
		// Адрес узла llama.cpp печатает отдельной строкой (живой формат).
		"load_tensors: RPC0[192.0.2.10:50052] model buffer size =  1251.07 MiB",
	} {
		recordLayerAssignment(line)
	}
	plan := deviceLayersSnapshot()
	if len(plan) != 3 {
		t.Fatalf("диапазонов = %d, want 3 (%+v)", len(plan), plan)
	}
	if plan[0].Device != "CUDA0" || plan[0].From != 1 || plan[0].To != 3 || plan[0].Count != 3 {
		t.Errorf("CUDA0: %+v", plan[0])
	}
	if plan[1].Device != "RPC0" || plan[1].From != 4 || plan[1].To != 5 || plan[1].Count != 2 {
		t.Errorf("RPC0: %+v", plan[1])
	}
	// Узел раскладки — ФАКТ из лога, а не «первый в списке узлов».
	if plan[1].Node != "192.0.2.10:50052" {
		t.Errorf("RPC0.Node = %q, want 192.0.2.10:50052", plan[1].Node)
	}
	if plan[0].Node != "" {
		t.Errorf("у локального CUDA0 не должно быть узла: %+v", plan[0])
	}
	if plan[2].Device != "CPU" || plan[2].From != 6 || plan[2].To != 6 || plan[2].Count != 1 {
		t.Errorf("CPU: %+v", plan[2])
	}

	// Слой, назначенный ПОВТОРНО (перезагрузка), не должен дублироваться.
	recordLayerAssignment("load_tensors: layer   2 assigned to device RPC0, is_swa = 1")
	plan = deviceLayersSnapshot()
	total := 0
	for _, r := range plan {
		total += r.Count
	}
	if total != 6 {
		t.Errorf("слоёв в плане = %d, want 6 (повтор не должен добавлять слой)", total)
	}
}

func TestR118_16_ParseRPCEndpoint(t *testing.T) {
	cases := []struct {
		line string
		dev  string
		ep   string
		ok   bool
	}{
		{"load_tensors: RPC0[192.0.2.10:50052] model buffer size =  1251.07 MiB", "RPC0", "192.0.2.10:50052", true},
		{"load_tensors: RPC1[10.0.0.5:50052] model buffer size =  10.00 MiB", "RPC1", "10.0.0.5:50052", true},
		// Локальные GPU и CPU печатаются без адреса — их не разбираем.
		{"load_tensors:        CUDA0 model buffer size =  1177.51 MiB", "", "", false},
		{"load_tensors:   CPU_Mapped model buffer size =  1936.74 MiB", "", "", false},
		// Назначение слоя — не адрес.
		{"load_tensors: layer  33 assigned to device CUDA0, is_swa = 1", "", "", false},
		// Строка bridge начинается с '[' — это не устройство llama.cpp.
		{"[bridge] RPC: узел 192.0.2.10:50052 подключён", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		dev, ep, ok := parseRPCEndpoint(c.line)
		if ok != c.ok || dev != c.dev || ep != c.ep {
			t.Errorf("parseRPCEndpoint(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.line, dev, ep, ok, c.dev, c.ep, c.ok)
		}
	}
	// Запись адреса не должна создавать слой в плане.
	resetDeviceLayers()
	recordLayerAssignment("load_tensors: RPC0[192.0.2.10:50052] model buffer size = 1.00 MiB")
	if got := deviceLayersSnapshot(); len(got) != 0 {
		t.Errorf("строка с адресом узла не должна давать слои: %+v", got)
	}
}

func TestR118_16_ResetClearsPlan(t *testing.T) {
	resetDeviceLayers()
	recordLayerAssignment(liveLineCUDA)
	if len(deviceLayersSnapshot()) == 0 {
		t.Fatal("план пуст после записи строки")
	}
	resetDeviceLayers()
	if got := deviceLayersSnapshot(); len(got) != 0 {
		t.Errorf("после сброса план не пуст: %+v", got)
	}
}
