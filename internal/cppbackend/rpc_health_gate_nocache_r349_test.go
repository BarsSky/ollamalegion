// rpc_health_gate_nocache_r349_test.go — R349 (2026-10-31): СТОРОЖ «гейт перед
// генерацией не кэшируется».
//
// КОНТЕКСТ. В балансере пробы узлов раскладки теперь идут через короткий кэш
// (internal/balancer/rpc_probe_cache_r349.go): один проход guard'а больше не делает
// несколько TCP-подключений к донору. Замер до правки: 10-11 подключений в минуту
// при полностью простаивающем стенде.
//
// ПОЧЕМУ ГЕЙТ ПЕРЕД ГЕНЕРАЦИЕЙ — ИСКЛЮЧЕНИЕ. checkRPCNodesForInference вызывается
// перед КАЖДОЙ генерацией. Если узел умер, а кэш ещё считает его живым, llama.cpp
// при обращении к мёртвому RPC-устройству вызывает abort() — падает ПРОЦЕСС МАСТЕРА,
// а не возвращается ошибка клиенту. Кэш здесь превратил бы защиту в её имитацию.
//
// Что проверяется: тело checkRPCNodesForInference обязано звать
// unreachableRPCServers напрямую и не содержать никакого кэша. Сторож нужен именно
// потому, что «оптимизировать» этот путь захочется: он тоже дёргает узлы на каждом
// запросе.
package cppbackend

import (
	"os"
	"strings"
	"testing"
)

// TestR349_PreGenerationGateIsNotCached_STEWARD — гейт перед генерацией без кэша.
func TestR349_PreGenerationGateIsNotCached_STEWARD(t *testing.T) {
	paths := []string{"rpc_health.go", "internal/cppbackend/rpc_health.go"}
	var text string
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			text = string(b)
			break
		}
	}
	if text == "" {
		t.Fatalf("не удалось прочитать rpc_health.go (пробовали %v)", paths)
	}

	const sig = "func (b *Backend) checkRPCNodesForInference() error {"
	idx := strings.Index(text, sig)
	if idx < 0 {
		t.Fatalf("не найдена функция checkRPCNodesForInference — если её переименовали, "+
			"обновите сторожа, иначе он ничего не проверяет (%s)", sig)
	}
	body := text[idx+len(sig):]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, "unreachableRPCServers") {
		t.Fatal("гейт перед генерацией больше не зовёт unreachableRPCServers — он обязан " +
			"проверять узлы перед КАЖДОЙ генерацией: мёртвый узел здесь это abort() в llama.cpp")
	}
	for _, bad := range []string{"ache", "Cached", "cached", "TTL", "ttl"} {
		if strings.Contains(body, bad) {
			t.Fatalf("в гейте перед генерацией появилось кэширование (%q): мёртвый узел в "+
				"пределах TTL приведёт к падению мастера (abort в llama.cpp), а не к ошибке "+
				"клиенту; тело функции: %s", bad, strings.TrimSpace(body))
		}
	}
}
