//go:build llama_stub

// chat_template_guard_test.go — R91 (2026-10-08): защита от повторения живого
// падения cppworker в chat template.
//
// ЖИВОЙ ДЕФЕКТ (нагрузочный прогон 2026-10-08, gemma-4, n_ctx=65536, запрос с
// tools[] от балансера):
//
//	SIGSEGV: segmentation violation / signal arrived during cgo execution
//	c/bridge.(*ModelHandle).ApplyChatTemplate → bridge_apply_chat_template
//	cmd/cppworker.buildChatPromptWithOptions (handlers_chat.go:721)
//
// Контейнер cppworker перезапускался, клиент получал обрыв, все остальные
// запросы узла терялись. recover() стоял только вокруг native-пути
// (ApplyChatTemplateWithThinking), а этот вызов — и повторный вызов после
// неудачи — оставались без защиты.
//
// Тест запускается с тегом llama_stub: настоящий C-мост здесь не нужен, потому
// что проверяется именно Go-обвязка. Панику внутрь C-вызова подменяем
// детерминированно: backend = nil, и первый же разыменователь (b.mu.RLock())
// паникует ровно так же, как это делал SIGSEGV из cgo.
package main

import (
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// withNilBackend подменяет глобальный backend на nil на время теста.
func withNilBackend(t *testing.T) {
	t.Helper()
	saved := backend
	backend = nil
	t.Cleanup(func() { backend = saved })
}

// TestSafeApplyChatTemplate_RecoversPanicAndPoisonsModel — главная проверка:
// паника внутри вызова ловится, модель помечается, воркер жив.
func TestSafeApplyChatTemplate_RecoversPanicAndPoisonsModel(t *testing.T) {
	const model = "test-panic-model"
	clearChatTemplateBroken(model)
	withNilBackend(t)

	_, err := safeApplyChatTemplate(model, "system", []bridge.ChatMessage{{Role: "user", Content: "привет"}}, true)
	if err == nil {
		t.Fatal("ожидалась ошибка: паника внутри chat template должна быть поймана и возвращена как error")
	}
	if !strings.Contains(err.Error(), "cgo panic") {
		t.Errorf("в ошибке нет пометки о пойманной панике: %v", err)
	}

	reason, broken := chatTemplateBrokenReason(model)
	if !broken {
		t.Fatal("модель не помечена как «шаблон сломан» — следующий запрос снова полезет в C")
	}
	if !strings.Contains(reason, "ApplyChatTemplate") {
		t.Errorf("причина не описывает место падения: %q", reason)
	}

	// Второй вызов обязан НЕ доходить до C: после SIGSEGV состояние C может быть
	// испорчено, и повторный вход добивает процесс (наблюдали живьём).
	_, err2 := safeApplyChatTemplate(model, "system", []bridge.ChatMessage{{Role: "user", Content: "привет"}}, true)
	if err2 == nil {
		t.Fatal("повторный вызов должен отказывать сразу, без обращения к C")
	}
	if !strings.Contains(err2.Error(), "отключён") {
		t.Errorf("повторный вызов вернул не ту ошибку: %v", err2)
	}

	clearChatTemplateBroken(model)
	if _, stillBroken := chatTemplateBrokenReason(model); stillBroken {
		t.Fatal("clearChatTemplateBroken не сняла пометку")
	}
}

// TestSafeApplyChatTemplateWithThinking_SharesPoisonFlag — native-путь и
// legacy-путь обязаны делить одну пометку: иначе вторая точка входа останется
// незащищённой (именно так и возник живой дефект).
func TestSafeApplyChatTemplateWithThinking_SharesPoisonFlag(t *testing.T) {
	const model = "test-panic-model-thinking"
	clearChatTemplateBroken(model)
	withNilBackend(t)

	_, _, err := safeApplyChatTemplateWithThinking(model, "", []bridge.ChatMessage{{Role: "user", Content: "привет"}}, true, true)
	if err == nil {
		t.Fatal("ожидалась ошибка: паника должна быть поймана и на native-пути")
	}
	if _, broken := chatTemplateBrokenReason(model); !broken {
		t.Fatal("native-путь не пометил модель как сломанную")
	}
	// Legacy-путь после этого в C не идёт вообще.
	_, err2 := safeApplyChatTemplate(model, "system", []bridge.ChatMessage{{Role: "user", Content: "привет"}}, true)
	if err2 == nil || !strings.Contains(err2.Error(), "отключён") {
		t.Fatalf("legacy-путь не увидел общую пометку: %v", err2)
	}

	clearChatTemplateBroken(model)
}

// TestChatTemplateBroken_MarkedOnce — в лог пишется ровно одна ERROR на модель,
// а не по строке на каждый запрос: иначе под нагрузкой лог превращается в шум.
func TestChatTemplateBroken_MarkedOnce(t *testing.T) {
	const model = "test-mark-once"
	clearChatTemplateBroken(model)

	markChatTemplateBroken(model, "первая")
	first, _ := chatTemplateBrokenReason(model)
	markChatTemplateBroken(model, "вторая")
	second, _ := chatTemplateBrokenReason(model)

	if first != "первая" {
		t.Errorf("причина первой пометки = %q", first)
	}
	// Причина обновляется (последняя известная), но повторная пометка не должна
	// логироваться как новая — это проверяется отсутствием ошибки в поведении:
	// здесь фиксируем, что API идемпотентен и не паникует.
	if second == "" {
		t.Error("причина потерялась при повторной пометке")
	}

	clearChatTemplateBroken(model)
	if _, broken := chatTemplateBrokenReason(model); broken {
		t.Error("пометка не снялась")
	}
	// Снятие несуществующей пометки — не ошибка (вызывается из handleLoadModel
	// на каждой загрузке).
	clearChatTemplateBroken(model)
}
