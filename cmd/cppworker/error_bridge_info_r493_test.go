// error_bridge_info_r493_test.go — R493 (2026-11-17): «нули» в bridge_info
// больше не выдаются за измерение.
//
// ЖИВОЙ ДЕФЕКТ (архив второго стенда): клиент получал
//
//	"bridge_info": {"current_n_ctx": 0, "required_n_ctx": 0, "max_vram_n_ctx": 443530}
//
// при том, что узел в тот же момент показывал maxVramNCtx=156444 (bundled) и
// 41472 (A10). Причина: writeCppWorkerErrorWithBridgeInfo берёт числа у
// ПОСЛЕДНЕЙ ошибки C-моста, но вызывается и для ошибок, родившихся вне моста
// (llama_decode вернул ненулевой код, таймаут, проверка на стороне Go). Тогда
// числа — нули и остатки прошлой ошибки, а в JSON они лежат рядом с настоящим
// code и читаются как факт: «окно 0», «нужно 0».
//
// Проверка: у блока обязан быть признак достоверности known и объяснение note,
// когда мост чисел не дал. Значение known=true проверяется там, где мост реально
// сообщает ошибку (тесты c/bridge); в stub-сборке мост всегда «OK».
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestR493_BridgeInfoMarksUnknownInsteadOfZeros(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCppWorkerErrorWithBridgeInfo(rec, http.StatusInternalServerError, "test", errors.New("boom"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код ответа = %d, ожидался 500", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (тело: %s)", err, rec.Body.String())
	}
	raw, ok := body["bridge_info"]
	if !ok {
		t.Fatal("нет bridge_info — клиент не увидит ни чисел, ни причины")
	}
	bi, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("bridge_info не объект: %T", raw)
	}
	known, ok := bi["known"].(bool)
	if !ok {
		t.Fatal("нет поля known: читатель снова не отличит «неизвестно» от «ноль»")
	}
	if known {
		t.Fatalf("known=true при пустых числах моста: %v", bi)
	}
	note, _ := bi["note"].(string)
	if note == "" {
		t.Fatal("нет пояснения note: непонятно, почему числа нулевые")
	}
	// Совместимость: ключи с числами остаются (их читает разбор на стороне
	// балансера), но сопровождаются признаком достоверности.
	for _, key := range []string{"current_n_ctx", "required_n_ctx", "max_vram_n_ctx"} {
		if _, ok := bi[key]; !ok {
			t.Fatalf("поле %q пропало — сломается разбор на стороне балансера", key)
		}
	}
	// Верхний уровень: code и человекочитаемая ошибка на месте.
	if _, ok := body["code"]; !ok {
		t.Fatal("нет top-level code")
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatal("нет текста ошибки")
	}
}
