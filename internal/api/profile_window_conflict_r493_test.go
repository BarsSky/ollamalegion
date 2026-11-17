// profile_window_conflict_r493_test.go — R493 (2026-11-17): противоречие
// «заявленное окно больше потолка» больше не сохраняется молча.
//
// ЖИВОЙ ФАКТ (второй стенд, архив 2026-11-16): профиль с
// contextLength=194296 при contextLengthMax=131072. R400 такого оператора не
// переписывает (заявленное окно выигрывает), и профиль сохранялся МОЛЧА. Итог на
// стенде: два узла поднялись с разными окнами для одной модели (194304 и 131072),
// и запрос, помещавшийся на одном, на другом получал отказ/перезагрузку.
//
// Проверка: правило одно (contextWindowConflictMessage) и оно вызывается из
// PUT-обработчика; ответ содержит warnings[].code = context_length_exceeds_max.
package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestR493_ContextWindowConflictMessage(t *testing.T) {
	cases := []struct {
		name    string
		decl    int
		max     int
		wantMsg bool
	}{
		{"равные значения — противоречия нет", 131072, 131072, false},
		{"заявленное меньше потолка — норма", 65536, 131072, false},
		{"живой случай второго стенда", 194296, 131072, true},
		{"потолок не задан (0) — сравнивать нечего", 194296, 0, false},
		{"заявленное не задано (0) — сравнивать нечего", 0, 131072, false},
	}
	for _, c := range cases {
		msg := contextWindowConflictMessage(c.decl, c.max)
		if c.wantMsg && msg == "" {
			t.Errorf("%s: ожидалось предупреждение для %d/%d, получили пусто", c.name, c.decl, c.max)
		}
		if !c.wantMsg && msg != "" {
			t.Errorf("%s: предупреждения быть не должно для %d/%d, получили %q", c.name, c.decl, c.max, msg)
		}
		if c.wantMsg {
			// Сообщение должно быть actionable: назвать оба числа и что делать.
			for _, want := range []string{"194296", "131072", "равными"} {
				if !strings.Contains(msg, want) {
					t.Errorf("%s: в тексте нет %q: %s", c.name, want, msg)
				}
			}
		}
	}
}

// TestR493_ProfileUpsertWarnsOnWindowConflict — обработчик PUT обязан положить
// warnings в ответ. Проверяется на уровне правила + наличия вызова в обработчике
// (полный HTTP-путь требует конфигурации балансера и покрыт отдельно).
func TestR493_ProfileUpsertWarnsOnWindowConflict(t *testing.T) {
	if contextWindowConflictMessage(194296, 131072) == "" {
		t.Fatal("правило не считает живой случай противоречием")
	}
	// Файл читаем как остальные стражи репозитория (см. apply_scope_r398_test.go):
	// проверяется наличие ВЫЗОВА правила в обработчике, а не только существование
	// функции — иначе правило можно оставить мёртвым кодом.
	raw, err := os.ReadFile("handlers_cppworker_profiles.go")
	if err != nil {
		t.Fatalf("не прочитать handlers_cppworker_profiles.go: %v", err)
	}
	src := string(raw)
	// Проверяем именно РАБОЧУЮ форму вызова: if conflict := <правило>(...); conflict != "".
	// Поиск одного имени функции проходит и на мутации «условие стало false &&» —
	// тогда вызов есть, а предупреждения нет (проверено мутационным прогоном).
	reachable := regexp.MustCompile(`if conflict := contextWindowConflictMessage\([^)]*\); conflict != "" \{`)
	if !reachable.MatchString(src) {
		t.Fatal("обработчик PUT не вызывает contextWindowConflictMessage рабочей ветвью — предупреждение не дойдёт до оператора")
	}
	if !strings.Contains(src, `"context_length_exceeds_max"`) {
		t.Fatal("в ответе нет машинного кода предупреждения context_length_exceeds_max")
	}
}
