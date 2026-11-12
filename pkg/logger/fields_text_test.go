// fields_text_test.go — R493-12a-dump: общий формат полей для текстовых дампов
// логов (балансер и воркер).
//
// ЖИВОЙ ФАКТ. В архиве стенда 2026-11-12 дампы логов содержали только текст
// сообщения, а поля терялись: «R482: удержание вывода снято» без
// held_ms/held_bytes, «R83 гейт n_ctx» без значения, «stream response summary» без
// счётчиков токенов. То есть причины и числа — ровно то, ради чего архив
// собирают, — в файл не попадали.
package logger

import (
	"errors"
	"strings"
	"testing"
)

func TestFieldsTextStableOrderAndQuotes(t *testing.T) {
	got := FieldsText(map[string]interface{}{
		"held_ms":   int64(20032),
		"model":     "Qwen3.8-27B-UD-Q4_K_M",
		"detail":    "hold_timeout held_bytes=1137",
		"backend":   "A10-126",
		"failed":    false,
		"err":       errors.New("context canceled"),
		"nested":    map[string]interface{}{"a": 1},
		"empty_str": "",
	})
	want := ` backend=A10-126 detail="hold_timeout held_bytes=1137" empty_str="" err="context canceled" ` +
		`failed=false held_ms=20032 model=Qwen3.8-27B-UD-Q4_K_M nested={"a":1}`
	if got != want {
		t.Fatalf("FieldsText = %q\nожидалось      %q", got, want)
	}
	// Строки с пробелами обязаны быть в кавычках, иначе поле «расползётся» по
	// нескольким токенам и diff архива станет нечитаемым.
	if !strings.Contains(got, `detail="hold_timeout`) {
		t.Fatalf("строка с пробелами не закавычена: %q", got)
	}
}

func TestFieldsTextEmpty(t *testing.T) {
	if got := FieldsText(nil); got != "" {
		t.Fatalf("nil-карта: ожидалась пустая строка, получено %q", got)
	}
	if got := FieldsText(map[string]interface{}{}); got != "" {
		t.Fatalf("пустая карта: ожидалась пустая строка, получено %q", got)
	}
}

func TestFieldValueTextNilAndNumbers(t *testing.T) {
	cases := map[string]string{
		"null":  FieldValueText(nil),
		"0":     FieldValueText(0),
		"-5":    FieldValueText(-5),
		"true":  FieldValueText(true),
		"1.5":   FieldValueText(1.5),
		"1206":  FieldValueText(int64(1206)),
		`""`:    FieldValueText(""),
		"текст": FieldValueText("текст"),
	}
	for want, got := range cases {
		if got != want {
			t.Fatalf("FieldValueText = %q, ожидалось %q", got, want)
		}
	}
}
