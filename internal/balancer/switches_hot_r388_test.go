package balancer

// switches_hot_r388_test.go — R388 (2026-11-02): поведенческие выключатели
// живут в реестре горячих настроек (панель WebUI), а не только в .env.
//
// Проверяем ДВА свойства каждого:
//  1. умолчание совпадает с прежним поведением (нельзя молча поменять
//     «фича выключена» на «включена» и наоборот);
//  2. env продолжает работать (0/false), потому что реестр читает его как
//     источник ниже переопределения.

import (
	"testing"
)

// TestR388_BehaviorSwitchDefaults — умолчания не поехали.
func TestR388_BehaviorSwitchDefaults(t *testing.T) {
	t.Setenv("LB_LAYOUT_HEAL", "")
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "")
	t.Setenv("LB_OPENAI_AUTO_STREAM", "")
	t.Setenv("LB_AUTO_CONTINUE_ON_TRUNCATION", "")
	t.Setenv("LB_OLLAMA_NATIVE_PATH", "")

	if !layoutHealEnabledR383() {
		t.Error("LB_LAYOUT_HEAL: автолечение раскладки должно быть включено по умолчанию (R383)")
	}
	if !streamGrowthEnabledR385() {
		t.Error("LB_NCTX_GROW_FOR_STREAM: рост окна на стриме должен быть включён по умолчанию (R385)")
	}
	if IsOpenAIAutoStreamEnabled() {
		t.Error("LB_OPENAI_AUTO_STREAM: по умолчанию выключено (backward compat)")
	}
	if IsAutoContinueOnTruncationEnabled() {
		t.Error("LB_AUTO_CONTINUE_ON_TRUNCATION: по умолчанию выключено (опция оператора)")
	}
	if !IsOllamaNativePathEnabled() {
		t.Error("LB_OLLAMA_NATIVE_PATH: нативная поверхность включена по умолчанию")
	}
}

// TestR388_BehaviorSwitchEnvStillWorks — env остаётся источником ниже
// переопределения; «0»/«false» обязаны выключать.
func TestR388_BehaviorSwitchEnvStillWorks(t *testing.T) {
	t.Setenv("LB_LAYOUT_HEAL", "0")
	if layoutHealEnabledR383() {
		t.Error("LB_LAYOUT_HEAL=0 обязан выключать автолечение")
	}
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "false")
	if streamGrowthEnabledR385() {
		t.Error("LB_NCTX_GROW_FOR_STREAM=false обязан выключать рост окна")
	}
	t.Setenv("LB_OPENAI_AUTO_STREAM", "true")
	if !IsOpenAIAutoStreamEnabled() {
		t.Error("LB_OPENAI_AUTO_STREAM=true обязан включать внутренний стрим")
	}
	t.Setenv("LB_AUTO_CONTINUE_ON_TRUNCATION", "1")
	if !IsAutoContinueOnTruncationEnabled() {
		t.Error("LB_AUTO_CONTINUE_ON_TRUNCATION=1 обязан включать авто-продолжение")
	}
	t.Setenv("LB_OLLAMA_NATIVE_PATH", "off")
	if IsOllamaNativePathEnabled() {
		t.Error("LB_OLLAMA_NATIVE_PATH=off обязан выключать нативный путь")
	}
}
