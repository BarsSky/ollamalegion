// agent_host_keep_r357_test.go — R357 (2026-10-31).
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд). Оператор исправил адрес донора через
// PUT /api/v1/backends/{id} (192.168.13.34 → 100.64.0.7, старый мёртв, новый
// отвечает). Через время адрес САМ вернулся к старому: обработчик регистрации агента
// собирает запись заново и брал Host из payload агента. Агент донора считает своим
// адресом 192.168.13.34, поэтому каждое его переподключение (в том числе после
// перезапуска Docker) откатывало операторскую правку — раскладка снова уходила в
// пустоту, health давал провал, «донор недоступен» возвращалось без действий оператора.
//
// R357: если прежний адрес РАБОЧИЙ (отвечает по TCP), регистрация его не перезаписывает;
// если не отвечает — принимается присланный агентом (после переезда узла прав агент).
//
// Проверяется:
//  1. живой прежний адрес сохраняется, присланный другим — нет;
//  2. неотвечающий прежний адрес заменяется присланным;
//  3. одинаковые адреса (в т.ч. разный регистр) — просто присланный;
//  4. пустые значения с обеих сторон не ломают правило;
//  5. сторож: обработчик регистрации обязан звать resolveRegisteredHostR357, иначе
//     правка R351/R357 снова будет откатываться агентом.
package api

import (
	"os"
	"strings"
	"testing"
)

// stubAgentHostReachR357 — подменить сетевую проверку адреса в тесте.
func stubAgentHostReachR357(t *testing.T, alive map[string]bool) {
	t.Helper()
	prev := agentHostReachableR357
	agentHostReachableR357 = func(host string, _ int) bool { return alive[host] }
	t.Cleanup(func() { agentHostReachableR357 = prev })
}

// TestR357_KeepsWorkingHost — главный сценарий: рабочий адрес не откатывается.
func TestR357_KeepsWorkingHost(t *testing.T) {
	stubAgentHostReachR357(t, map[string]bool{"100.64.0.7": true})

	got, kept := resolveRegisteredHostR357("100.64.0.7", "192.168.13.34", 18092)
	if got != "100.64.0.7" {
		t.Fatalf("host = %q, want 100.64.0.7: агент донора откатил операторскую правку "+
			"адреса — раскладка снова уйдёт в мёртвый узел", got)
	}
	if !kept {
		t.Error("kept = false, want true (случай должен быть виден в логе и тестах)")
	}
}

// TestR357_ReplacesDeadHost — неотвечающий адрес заменяется присланным.
func TestR357_ReplacesDeadHost(t *testing.T) {
	stubAgentHostReachR357(t, map[string]bool{"100.64.0.7": true})

	got, kept := resolveRegisteredHostR357("192.168.13.34", "100.64.0.7", 18092)
	if got != "100.64.0.7" || kept {
		t.Fatalf("host = %q (kept=%v), want 100.64.0.7: после переезда узла прав агент, "+
			"и мёртвый адрес держать незачем", got, kept)
	}
}

// TestR357_SameHostIsFine — одинаковые адреса, в том числе разный регистр.
func TestR357_SameHostIsFine(t *testing.T) {
	stubAgentHostReachR357(t, map[string]bool{"Worker-1": true})

	got, kept := resolveRegisteredHostR357("Worker-1", "worker-1", 18092)
	if got != "worker-1" || kept {
		t.Fatalf("host = %q (kept=%v), want worker-1 без «сохранения»: адреса совпадают", got, kept)
	}
}

// TestR357_EmptyValues — пустые значения не ломают правило.
func TestR357_EmptyValues(t *testing.T) {
	stubAgentHostReachR357(t, map[string]bool{})

	if got, _ := resolveRegisteredHostR357("", "10.0.0.5", 18092); got != "10.0.0.5" {
		t.Errorf("пустой прежний адрес: got %q, want 10.0.0.5", got)
	}
	if got, _ := resolveRegisteredHostR357("10.0.0.5", "", 18092); got != "10.0.0.5" {
		t.Errorf("пустой присланный адрес: got %q, want сохранить 10.0.0.5", got)
	}
	if got, _ := resolveRegisteredHostR357("", "", 18092); got != "" {
		t.Errorf("оба пустые: got %q, want пусто", got)
	}
}

// TestR357_RegistrationUsesResolver_STEWARD — сторож: обработчик регистрации обязан
// использовать правило, иначе исправление обойдётся одной новой точкой сборки записи.
func TestR357_RegistrationUsesResolver_STEWARD(t *testing.T) {
	paths := []string{"handlers_agents.go", "internal/api/handlers_agents.go"}
	var text string
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			text = string(b)
			break
		}
	}
	if text == "" {
		t.Fatalf("не удалось прочитать handlers_agents.go (пробовали %v)", paths)
	}
	if !strings.Contains(text, "resolveRegisteredHostR357(") {
		t.Fatal("обработчик регистрации агента не зовёт resolveRegisteredHostR357: " +
			"адрес, исправленный оператором, снова будет откатываться агентом на каждом " +
			"его переподключении")
	}
}
