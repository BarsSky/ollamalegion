// outgoing_tool_args_log_r440.go — R440 (2026-11-05): диагностика ТОГО, ЧТО
// уходит клиенту в tool_calls.
//
// ЗАЧЕМ. Живой отказ Cline («Invalid input: expected string, received undefined →
// at new_text») невозможно было разобрать по логам: сводка печатает только
// `tool_arg_shapes` («object»), а сами аргументы — нет. Приходилось
// восстанавливать форму по скриншоту клиента.
//
// Включается переменной CPPWORKER_DEBUG_TOOL_ARGS=1: тогда на каждый
// распознанный вызов в лог уходит имя, форма аргументов, длина и первые
// 400 символов. По умолчанию ВЫКЛЮЧЕНО — это содержимое файлов пользователя.
package main

import (
	"os"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// outgoingToolArgsLogEnabled — включён ли дамп аргументов в лог.
func outgoingToolArgsLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_TOOL_ARGS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logOutgoingToolCalls — записать в лог, что именно уходит клиенту.
func logOutgoingToolCalls(model, surface string, calls []openAIToolCall) {
	if len(calls) == 0 || !outgoingToolArgsLogEnabled() {
		return
	}
	l := logger.Get()
	if l == nil {
		return
	}
	for i, c := range calls {
		args := c.Function.Arguments
		l.Infow("outgoing tool_call arguments (CPPWORKER_DEBUG_TOOL_ARGS=1)",
			"model", model, "surface", surface, "index", i,
			"tool", c.Function.Name, "args_shape", toolArgShape(args),
			"args_len", len(args),
			"args_head", truncateForLog(args, 400))
	}
}
