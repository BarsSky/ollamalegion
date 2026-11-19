package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R554 (F8-хвост, docs/plans/2026-11-19-webui-and-balancer-fixes.md §2.8).
//
// ЖИВОЙ ДЕФЕКТ. Этап 3c (холодный старт: «модели в VRAM нет ни у кого, есть файл
// на диске») выбирал узел по loadRatio и score, но НЕ проверял двух вещей,
// которые проверяет 3b:
//   - есть ли СВОБОДНЫЙ СЛОТ (мог выбрать узел, где модель уже некуда поставить,
//     и загрузка вставала в очередь сама к себе);
//   - влезает ли модель в СВОБОДНУЮ VRAM (узлы с 6 ГБ и «модель 12 ГиБ» —
//     равноправные кандидаты, выбор решался tie-break по id).
//
// После правки у 3c своя точка входа
// `findLeastLoadedBackendWithModelOnDiskForColdStart`: слот + VRAM-fit. У переезда
// и failover осталась прежняя функция: там свободный слот не обязателен (слот
// освобождается вместе с упавшим узлом), и это поведение здесь не меняется —
// на него есть отдельный тест.

// coldStartProxyR554 — два свободных узла с файлом модели на диске.
func coldStartProxyR554(t *testing.T) *Proxy {
	t.Helper()
	p := availProxyR493(t, map[string]int{"aaa": 1, "zzz": 1})
	setMetricsR493(p, "aaa", nil, 6000)
	setMetricsR493(p, "zzz", nil, 20000)
	// Оба свободны, оба знают файл: старый код решал tie-break по id → «aaa».
	catalogStoreR552(p, "aaa", []string{"m12b.gguf"}, "")
	catalogStoreR552(p, "zzz", []string{"m12b.gguf"}, "")
	// Свободная VRAM: aaa — 6 ГБ, zzz — 20 ГБ (12 ГиБ модель влезает только в zzz).
	setGpuR493(p, "aaa", 0, 23000, 17000, 0)
	setGpuR493(p, "zzz", 0, 23000, 3000, 0)
	return p
}

// TestR554_ColdStartRespectsVram — модель 12 ГиБ не грузится на узел с 6 ГБ.
func TestR554_ColdStartRespectsVram(t *testing.T) {
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == "m12b" {
			return int64(12) << 30
		}
		return 0
	})
	defer restore()

	p := coldStartProxyR554(t)
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	got := p.findLeastLoadedBackendWithModelOnDiskForColdStart("m12b", types1)
	if got != "zzz" {
		t.Fatalf("холодный старт выбрал %q, ожидался узел с 20 ГБ свободной VRAM: "+
			"модель 12 ГиБ не влезает в 6 ГБ, и загрузка там упала бы по памяти", got)
	}

	// Контроль: без проверки VRAM (переезд/failover) выбор прежний — tie-break по id.
	if got := p.findLeastLoadedBackendWithModelOnDisk("m12b", types1); got != "aaa" {
		t.Fatalf("контроль: прежняя функция обязана вести себя как раньше (aaa), got %q", got)
	}
}

// TestR554_ColdStartSkipsSaturatedNode — узел с файлом, но без свободного слота
// не выбирается: иначе загрузка встаёт в очередь сама к себе.
func TestR554_ColdStartSkipsSaturatedNode(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 1, "free": 1})
	setMetricsR493(p, "only", nil, 20000)
	setMetricsR493(p, "free", nil, 20000)
	catalogStoreR552(p, "only", []string{"m12b.gguf"}, "")
	catalogStoreR552(p, "free", []string{"m12b.gguf"}, "")
	p.backends["only"].ActiveReqs = 1 // слотов нет
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if got := p.findLeastLoadedBackendWithModelOnDiskForColdStart("m12b", types1); got != "free" {
		t.Fatalf("холодный старт выбрал %q: узел без свободного слота не должен выбираться, "+
			"пока есть свободный с таким же файлом", got)
	}
}

// TestR554_OnlySaturatedNodeGivesQueue — если файл есть ТОЛЬКО на занятом узле,
// холодный старт честно возвращает пусто (решает очередь), а не выбирает узел,
// который не примет загрузку.
func TestR554_OnlySaturatedNodeGivesQueue(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 1, "other": 1})
	setMetricsR493(p, "only", nil, 20000)
	setMetricsR493(p, "other", nil, 20000)
	catalogStoreR552(p, "only", []string{"m12b.gguf"}, "")
	catalogStoreR552(p, "other", []string{"other.gguf"}, "")
	p.backends["only"].ActiveReqs = 1
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if got := p.findLeastLoadedBackendWithModelOnDiskForColdStart("m12b", types1); got != "" {
		t.Fatalf("единственный узел с файлом занят — ожидалась очередь (\"\"), got %q", got)
	}
}

// TestR554_FailoverKeepsOldBehaviour — переезд/failover НЕ получил новых фильтров:
// там узел может быть занят (слот освободится вместе с упавшим), и менять это
// поведение в этой правке нельзя.
func TestR554_FailoverKeepsOldBehaviour(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 1})
	setMetricsR493(p, "only", nil, 20000)
	catalogStoreR552(p, "only", []string{"m12b.gguf"}, "")
	p.backends["only"].ActiveReqs = 1
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	// Холодный старт: занятый узел не годится.
	if got := p.findLeastLoadedBackendWithModelOnDiskForColdStart("m12b", types1); got != "" {
		t.Fatalf("холодный старт не должен брать занятый узел, got %q", got)
	}
	// Переезд: прежнее поведение сохранено.
	if got := p.findLeastLoadedBackendWithModelOnDiskExcluding("m12b", nil); got != "only" {
		t.Fatalf("переезд обязан по-прежнему находить узел с файлом, got %q", got)
	}
}
