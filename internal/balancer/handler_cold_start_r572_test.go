package balancer

import (
	"testing"
	"time"
)

// R572 (F8 handler-путь, docs/plans/2026-11-19-webui-and-balancer-fixes.md §2.8 п.(d)).
//
// ЖИВОЙ ПРОБЕЛ. Этап «модель не загружена нигде → выбрать узел, где ФАЙЛ на диске»
// существовал и на handler-пути (`selectLlamaCppBackendForModel`,
// `llamacpp_backend_helpers.go`), но звал ПРОСТУЮ версию помощника — без проверки
// свободного слота и свободной VRAM (R554 добавил их для селектора SPA-пути).
//
// Следствие: на OpenAI-поверхности (/v1/*) выбирался узел, где модель уже некуда
// поставить или где она не влезает в память — то есть тот же дефект, что R554 закрыл
// для /api/chat, оставался на второй поверхности.

// TestR572_HandlerPathColdStartChecksSlot — занятый узел с файлом не выбирается,
// пока есть свободный с тем же файлом.
func TestR572_HandlerPathColdStartChecksSlot(t *testing.T) {
	p := availProxyR493(t, map[string]int{"busy": 1, "free": 1})
	setMetricsR493(p, "busy", nil, 20000)
	setMetricsR493(p, "free", nil, 20000)
	catalogStoreR552(p, "busy", []string{"m12b.gguf"}, "")
	catalogStoreR552(p, "free", []string{"m12b.gguf"}, "")
	p.backends["busy"].ActiveReqs = 1 // слота нет

	lr := &LlamaCppRouter{proxy: p}
	got := lr.selectLlamaCppBackendForModel("m12b")
	if got != "free" {
		t.Fatalf("handler-путь выбрал %q: узел без свободного слота не должен выбираться, "+
			"пока есть свободный с тем же файлом (R554 закрыл это для /api/chat)", got)
	}
}

// TestR572_HandlerPathColdStartChecksVram — модель 12 ГиБ не уходит на узел с 6 ГБ.
func TestR572_HandlerPathColdStartChecksVram(t *testing.T) {
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == "m12b" {
			return int64(12) << 30
		}
		return 0
	})
	defer restore()

	p := availProxyR493(t, map[string]int{"aaa": 1, "zzz": 1})
	setMetricsR493(p, "aaa", nil, 6000)
	setMetricsR493(p, "zzz", nil, 20000)
	catalogStoreR552(p, "aaa", []string{"m12b.gguf"}, "")
	catalogStoreR552(p, "zzz", []string{"m12b.gguf"}, "")
	setGpuR493(p, "aaa", 0, 23000, 17000, 0) // свободно 6 ГБ
	setGpuR493(p, "zzz", 0, 23000, 3000, 0)  // свободно 20 ГБ

	lr := &LlamaCppRouter{proxy: p}
	got := lr.selectLlamaCppBackendForModel("m12b")
	if got != "zzz" {
		t.Fatalf("handler-путь выбрал %q, ожидался узел с 20 ГБ свободной VRAM: "+
			"модель 12 ГиБ не влезает в 6 ГБ — загрузка упала бы по памяти", got)
	}
}

// TestR572_WarmCopyStillPreferred — прежнее поведение сохранено: если модель уже
// загружена, выбирается она (загрузку второй копии молча не запускаем).
func TestR572_WarmCopyStillPreferred(t *testing.T) {
	p := availProxyR493(t, map[string]int{"warm": 1, "cold": 1})
	setMetricsR493(p, "warm", []string{"m12b"}, 20000)
	setMetricsR493(p, "cold", nil, 20000)
	catalogStoreR552(p, "cold", []string{"m12b.gguf"}, "")
	_ = time.Now()

	lr := &LlamaCppRouter{proxy: p}
	if got := lr.selectLlamaCppBackendForModel("m12b"); got != "warm" {
		t.Fatalf("прогретая копия обязана быть выбрана первой, got %q", got)
	}
}
