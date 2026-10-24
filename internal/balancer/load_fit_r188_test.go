// load_fit_r188_test.go — R188 (2026-10-20).
//
// ЖИВЫЕ ЗАМЕТКИ ОПЕРАТОРА: балансер ставил тяжёлую модель на бэкенд, где уже
// жила другая, и запускал загрузку, которая заведомо не могла поместиться —
// слот/очередь занимались на десятки минут, клиент терял ход.
//
// Контракт: отказ ДО загрузки по фактическим числам воркера; при неизвестном
// размере модели или старом воркере — не блокируем (предпроверка не должна
// становиться новым источником отказов).
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func fitServer(t *testing.T, cap map[string]int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(cap)
	}))
}

func hostPortOf(t *testing.T, srvURL string) (string, int) {
	t.Helper()
	hp := strings.TrimPrefix(srvURL, "http://")
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		t.Fatalf("не разобрать адрес: %s", srvURL)
	}
	port, err := strconv.Atoi(hp[i+1:])
	if err != nil {
		t.Fatalf("не разобрать порт: %s", srvURL)
	}
	return hp[:i], port
}

func TestR188_FitRefusalWhenModelTooBig(t *testing.T) {
	srv := fitServer(t, map[string]int64{
		"available_ram_mb": 2000, "available_vram_mb": 500, "total_ram_mb": 24000, "total_vram_mb": 8192,
	})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	p := &Proxy{}
	modelSizeBytesForTest = func(name string) int64 { return 16 * 1024 * 1024 * 1024 } // 16 ГБ
	defer func() { modelSizeBytesForTest = nil }()

	refuse, reason := p.loadFitRefusal("be1", "Qwen3.8-27B-UD-Q4_K_M", host, port, "")
	if !refuse {
		t.Fatal("ожидался отказ: модель 16 ГБ при свободных 2.5 ГБ")
	}
	for _, want := range []string{"Qwen3.8", "be1", "не влезает", loadFitRefusalCode} {
		if !strings.Contains(reason, want) {
			t.Errorf("в причине нет %q: %s", want, reason)
		}
	}
}

func TestR188_NoRefusalWhenModelFits(t *testing.T) {
	srv := fitServer(t, map[string]int64{"available_ram_mb": 20000, "available_vram_mb": 4000})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	p := &Proxy{}
	modelSizeBytesForTest = func(name string) int64 { return 4 * 1024 * 1024 * 1024 } // 4 ГБ
	defer func() { modelSizeBytesForTest = nil }()

	if refuse, reason := p.loadFitRefusal("be1", "gemma", host, port, ""); refuse {
		t.Fatalf("ложный отказ при достаточной памяти: %s", reason)
	}
}

func TestR188_NoRefusalWhenCapacityUnknown(t *testing.T) {
	// Старый воркер: полей о памяти нет.
	srv := fitServer(t, map[string]int64{})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	p := &Proxy{}
	modelSizeBytesForTest = func(name string) int64 { return 16 * 1024 * 1024 * 1024 }
	defer func() { modelSizeBytesForTest = nil }()

	if refuse, reason := p.loadFitRefusal("be1", "m", host, port, ""); refuse {
		t.Fatalf("отказ без данных о памяти недопустим: %s", reason)
	}
}

func TestR188_NoRefusalWhenSizeUnknown(t *testing.T) {
	srv := fitServer(t, map[string]int64{"available_ram_mb": 100, "available_vram_mb": 100})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	p := &Proxy{}
	modelSizeBytesForTest = func(name string) int64 { return 0 } // размер неизвестен
	defer func() { modelSizeBytesForTest = nil }()

	if refuse, reason := p.loadFitRefusal("be1", "m", host, port, ""); refuse {
		t.Fatalf("отказ при неизвестном размере модели недопустим: %s", reason)
	}
}

// TestR191_RPCLayoutActive — окно ожидания расширяется только когда раскладка реально активна.
func TestR191_RPCLayoutActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"available_ram_mb": 4000, "available_vram_mb": 500,
			"rpc_enabled": true, "rpc_servers_active": []string{"192.0.2.10:50052"},
		})
	}))
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)
	p := &Proxy{}
	if !p.rpcLayoutActive(host, port, "") {
		t.Fatal("активная раскладка не распознана: окно ожидания не расширится")
	}

	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"available_ram_mb": 4000, "available_vram_mb": 500,
			"rpc_enabled": true, "rpc_servers_active": []string{},
		})
	}))
	defer idle.Close()
	h2, p2 := hostPortOf(t, idle.URL)
	if p.rpcLayoutActive(h2, p2, "") {
		t.Fatal("без активных узлов окно расширять нельзя")
	}
}
