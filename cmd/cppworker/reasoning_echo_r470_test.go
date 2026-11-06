//go:build llama_stub

// reasoning_echo_r470_test.go — R470 (2026-11-06): на tools-пути размышления
// отдаются клиенту только по явной просьбе.
//
// ЖИВОЙ ДЕФЕКТ. Cline присылал 6 инструментов и НЕ присылал `think`. Мы по
// умолчанию стримили размышления (`message.thinking`), а Cline кладёт их в ТЕКСТ
// сообщения ассистента: в истории следующего запроса assistant-сообщение длиной
// ровно в reasoning предыдущего хода, на английском («The user is in 'act'
// mode…»). Модель видит собственные размышления как свой прошлый ответ — отсюда
// «совмещённый ответ в двух режимах» и плановый голос вместо вызова инструмента.
package main

import "testing"

func boolPtrR470(v bool) *bool { return &v }

// TestR470_AutoEchoOnlyWhenClientAsked — правило по умолчанию.
func TestR470_AutoEchoOnlyWhenClientAsked(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_THINKING", "")
	if toolsThinkingEchoR470(nil) {
		t.Error("think не присылали — эхо размышлений обязано быть выключено (auto)")
	}
	if toolsThinkingEchoR470(boolPtrR470(false)) {
		t.Error("think:false — эхо обязано быть выключено")
	}
	if !toolsThinkingEchoR470(boolPtrR470(true)) {
		t.Error("think:true — клиент попросил явно, эхо обязано быть включено")
	}
}

// TestR470_SwitchOnRestoresOldBehaviour — рубильник возвращает прежнее поведение.
func TestR470_SwitchOnRestoresOldBehaviour(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_THINKING", "on")
	if !toolsThinkingEchoR470(nil) {
		t.Error("CPPWORKER_TOOLS_THINKING=on — прежнее поведение (эхо всегда)")
	}
	if !toolsThinkingEchoR470(boolPtrR470(false)) {
		t.Error("on — эхо даже при think:false (как было до R470)")
	}
}

// TestR470_SwitchOffSuppressesEvenWhenAsked — явный off сильнее просьбы клиента.
func TestR470_SwitchOffSuppressesEvenWhenAsked(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_THINKING", "off")
	if toolsThinkingEchoR470(boolPtrR470(true)) {
		t.Error("off — эхо запрещено даже при think:true")
	}
}

// TestR470_GarbageValueIsAuto — мусор в переменной не ломает поведение.
func TestR470_GarbageValueIsAuto(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_THINKING", "абракадабра")
	if toolsThinkingEchoR470(nil) {
		t.Error("неизвестное значение должно давать auto (без эха, клиент не просил)")
	}
	if !toolsThinkingEchoR470(boolPtrR470(true)) {
		t.Error("неизвестное значение = auto: явную просьбу уважаем")
	}
}
