// client_tool_guidance_r455.go — R455/R456/R457 (2026-11-06): подсказки для
// трёх живых классов «инструменты перестали работать».
//
// ЖИВОЙ ПРОГОН (Cline + Qwen3.8-9B, стенд, 2026-11-06). Модель:
//  1. запустила команду `Get-ChildItem ... && Get-Content .gitignore` → exit 1
//     («Cannot find path»), и ПОВТОРИЛА её ещё раз;
//  2. после этого перестала пользоваться инструментами и выдала целую
//     HTML-страницу ТЕКСТОМ в чат (`tool_calls=0`, 8587 символов, stop).
//
// Почему существующая подсказка R445 не помогла: она ищет только ОТКАЗЫ клиента
// (`old_text was omitted`, `already exists`, `editor input too large`). Ошибки
// команд, повтор одинакового вызова и «файл текстом вместо вызова» в неё не
// входят — модель не получила ни одной инструкции и зациклилась.
//
// Правило, которому подчинены все три детектора: НЕ ВЫДУМЫВАТЬ действий за
// модель. Мы ничего не переписываем в её ответе и не подменяем результаты
// инструментов — только добавляем одну короткую инструкцию в system, привязанную
// к факту из истории. Данные клиента модель по-прежнему видит как есть.
package main

import (
	"fmt"
	"strings"
)

const (
	// guidanceTailMessages — сколько последних результатов инструментов смотрим.
	guidanceTailMessages = 8

	// fileBodyInChatMinLen — с какой длины текст считается «телом файла».
	// Меньше 1500 символов — это обычный ответ или короткий пример, трогать не
	// надо (подсказка была бы шумом).
	fileBodyInChatMinLen = 1500
)

// commandFailedR455 — завершилась ли команда в этом результате с ошибкой.
//
// Разбираем «exited with code N» с N != 0 (код 0 — успех, это не ошибка), плюс
// типовые причины, при которых код может быть любым: файла нет, команда не
// найдена, каталог не найден.
func commandFailedR455(content string) bool {
	low := strings.ToLower(content)
	for _, marker := range []string{
		"cannot find path",
		"cannot find drive",
		"is not recognized as the name of a cmdlet",
		"commandnotfoundexception",
		"no such file or directory",
		"pathnotfoundexception",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	// «exited with code N» / «exit code N» — ошибка только при N != 0.
	for _, marker := range []string{"exited with code", "exit code"} {
		idx := 0
		for {
			pos := strings.Index(low[idx:], marker)
			if pos < 0 {
				break
			}
			at := idx + pos + len(marker)
			idx = at
			rest := strings.TrimLeft(low[at:], " :=")
			digits := 0
			for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
				digits++
			}
			if digits == 0 {
				continue
			}
			if rest[:digits] != "0" {
				return true
			}
		}
	}
	return false
}

// commandFailureHintR455 — инструкция, если последние команды падали.
func commandFailureHintR455(msgs []chatMessage) string {
	seen := 0
	for i := len(msgs) - 1; i >= 0 && seen < guidanceTailMessages; i-- {
		if !strings.EqualFold(msgs[i].Role, "tool") {
			continue
		}
		seen++
		if commandFailedR455(msgs[i].Content) {
			return "Последние команды завершились ОШИБКОЙ (ненулевой код возврата или файл не найден). " +
				"Не повторяй ту же команду дословно: сначала проверь, существует ли файл или каталог " +
				"(Test-Path / dir / ls), исправь путь — либо используй другой инструмент, если файла действительно нет."
		}
	}
	return ""
}

// toolCallSignatureR456 — сигнатура вызова для сравнения повторов: имя + аргументы.
func toolCallSignatureR456(tc ollamaToolCall) string {
	args := strings.TrimSpace(string(tc.Function.Arguments))
	// Пробелы/переводы строк в JSON не делают вызов другим — сравниваем
	// компактную форму.
	if compact := normalizeToolArgsForCompareR456(args); compact != "" {
		args = compact
	}
	return strings.ToLower(tc.Function.Name) + "|" + args
}

// normalizeToolArgsForCompareR456 — убрать пробелы ВНЕ строковых литералов.
func normalizeToolArgsForCompareR456(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for _, r := range s {
		if inString {
			b.WriteRune(r)
			if escaped {
				escaped = false
				continue
			}
			switch r {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			inString = true
		}
		b.WriteRune(r)
	}
	return b.String()
}

// repeatedToolCallHintR456 — инструкция, если два последних вызова совпали.
//
// Живой факт: модель запускала ОДНУ И ТУ ЖЕ команду четыре раза, получая один и
// тот же отказ. Такой цикл в логах не виден вообще — отсюда и эта проверка.
func repeatedToolCallHintR456(msgs []chatMessage) string {
	sigs := make([]string, 0, guidanceTailMessages)
	for i := len(msgs) - 1; i >= 0 && len(sigs) < guidanceTailMessages; i-- {
		if !strings.EqualFold(msgs[i].Role, "assistant") || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		// Идём по истории назад: внутри одного сообщения вызовы идут в прямом
		// порядке, поэтому добавляем их в обратном — так первым в sigs окажется
		// САМЫЙ ПОЗДНИЙ вызов.
		for j := len(msgs[i].ToolCalls) - 1; j >= 0; j-- {
			sigs = append(sigs, toolCallSignatureR456(msgs[i].ToolCalls[j]))
		}
	}
	if len(sigs) < 2 {
		return ""
	}
	if sigs[0] == "" || sigs[0] != sigs[1] {
		return ""
	}
	return "Ты уже вызывал этот инструмент с ТЕМИ ЖЕ аргументами, и результат не изменился. " +
		"Не повторяй вызов: измени аргументы (проверь путь/имя) или выбери другой инструмент."
}

// looksLikeFileBodyR457 — похож ли текст ответа на содержимое файла целиком.
//
// Узкое правило: текст С НАЧАЛА выглядит как документ или как блок кода в
// ограде, и он длинный. Короткий пример в ответе (10 строк кода) подсказку не
// вызывает — иначе мы мешали бы нормальным объяснениям.
func looksLikeFileBodyR457(content string) bool {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) < fileBodyInChatMinLen {
		return false
	}
	low := strings.ToLower(trimmed)
	for _, prefix := range []string{
		"<!doctype html",
		"<html",
		"<?xml",
		"```html",
		"```htm",
		"```css",
		"```javascript",
		"```js",
		"```typescript",
		"```ts",
		"```json",
		"package main",
		"#!/usr/bin/env",
	} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	// Текст с «вот файл» и целым документом дальше: документ внутри ответа.
	if idx := strings.Index(low, "<!doctype html"); idx >= 0 && idx < 400 {
		return true
	}
	return false
}

// fileBodyAnswerHintR457 — инструкция, если предыдущий ответ был файлом текстом.
func fileBodyAnswerHintR457(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, "assistant") {
			continue
		}
		// Вызов инструмента — это нормальный путь, подсказка не нужна.
		if len(msgs[i].ToolCalls) > 0 {
			return ""
		}
		if looksLikeFileBodyR457(msgs[i].Content) {
			return "В предыдущем ответе содержимое файла было выведено ТЕКСТОМ в чат, и файл не создан. " +
				"Не выводи содержимое файлов в ответ: создавай и изменяй их инструментом (editor / write_to_file), " +
				"а в тексте пиши только короткое описание и следующий шаг."
		}
		return ""
	}
	return ""
}

// clientToolGuidanceHint — слить все найденные инструкции в один блок.
//
// Один блок на запрос: истории свойственно накапливать прошлые проблемы, и
// повторять инструкции на каждую было бы шумом в промпте.
func clientToolGuidanceHint(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	parts := make([]string, 0, 3)
	if h := commandFailureHintR455(msgs); h != "" {
		parts = append(parts, "R455: "+h)
	}
	if h := repeatedToolCallHintR456(msgs); h != "" {
		parts = append(parts, "R456: "+h)
	}
	if h := fileBodyAnswerHintR457(msgs); h != "" {
		parts = append(parts, "R457: "+h)
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Инструкции по инструментам (найдено в истории, выполни прежде чем отвечать):\n- %s",
		strings.Join(parts, "\n- "))
}
