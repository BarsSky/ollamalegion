// flash_attn_kv_guard_r367_test.go — R367 (2026-10-31).
//
// Проверяется правило: «flash attention выключен ЯВНО + квантованный KV-cache»
// отвергается ДО загрузки с внятной причиной. Живой случай, из которого правило
// выросло: профиль `flashAttn:false` + `kvCacheType:q4_0` давал 500 «failed to
// create context», а llama.cpp в логе объяснял: quantized V cache requires
// flash_attn to be enabled. Причина известна заранее — значит её надо назвать.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestR367_ConflictMatrix — какие пары считаются противоречием.
func TestR367_ConflictMatrix(t *testing.T) {
	cases := []struct {
		name          string
		flashAttnType int
		kvCacheType   string
		wantConflict  bool
	}{
		// Боевой случай: профиль выключил flash attention, KV квантованный.
		{"off + q4_0", 0, "q4_0", true},
		{"off + q8_0", 0, "q8_0", true},

		// Допустимые пары: llama.cpp сам включает flash attention в auto, а с f16
		// квантование не используется вовсе.
		{"auto + q4_0", -1, "q4_0", false},
		{"auto + q8_0", -1, "q8_0", false},
		{"on + q4_0", 1, "q4_0", false},
		{"off + f16", 0, "f16", false},
		{"off + inherit", 0, "", false},
		{"auto + inherit", -1, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := flashAttnKVCacheConflictR367(c.flashAttnType, c.kvCacheType)
			if c.wantConflict && err == nil {
				t.Fatalf("пара (%d, %q) обязана считаться противоречием: иначе llama.cpp упадёт "+
					"с невнятным \"failed to create context\"", c.flashAttnType, c.kvCacheType)
			}
			if !c.wantConflict && err != nil {
				t.Errorf("пара (%d, %q) допустима, а отвергается: %v", c.flashAttnType, c.kvCacheType, err)
			}
		})
	}
}

// TestR367_ErrorMessageNamesBothFields — сообщение обязано называть причину и
// способ починки, иначе оператор снова пойдёт читать лог llama.cpp.
func TestR367_ErrorMessageNamesBothFields(t *testing.T) {
	err := flashAttnKVCacheConflictR367(0, "q4_0")
	if err == nil {
		t.Fatal("конфликт не обнаружен")
	}
	msg := err.Error()
	for _, want := range []string{"flashAttn=false", "kvCacheType=q4_0", "flash attention", "f16"} {
		if !containsR367(msg, want) {
			t.Errorf("в сообщении нет %q: %s", want, msg)
		}
	}
}

// TestR367_HTTPGuardAnswers400 — HTTP-обёртка отвечает 400, а не 500.
func TestR367_HTTPGuardAnswers400(t *testing.T) {
	rec := httptest.NewRecorder()
	if checkFlashAttnKVCacheR367(rec, "m1", 0, "q4_0") {
		t.Fatal("гард обязан вернуть false при конфликте")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("код = %d, want 400", rec.Code)
	}
	if !containsR367(rec.Body.String(), "flashAttn") {
		t.Errorf("тело ответа не называет поле: %s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	if !checkFlashAttnKVCacheR367(rec2, "m1", 1, "q4_0") {
		t.Fatal("допустимая пара не должна отвергаться")
	}
	if rec2.Code != http.StatusOK && rec2.Body.Len() != 0 {
		t.Errorf("для допустимой пары гард ничего не пишет, got code=%d body=%q", rec2.Code, rec2.Body.String())
	}
}

// TestR367STEWARD_AllLoadPathsAskGuard — стюард: каждый путь, который доводит
// загрузку до llama.cpp, обязан спросить гард, иначе отказ снова станет невнятным.
func TestR367STEWARD_AllLoadPathsAskGuard(t *testing.T) {
	files := parsePackageFilesR364(t)

	wantPaths := []string{
		"handleLoadModel",
		"handleLoadWithParams",
		"handleReloadModel",
		"applySyncedProfileToLazyLoadOpts",
	}
	seen := map[string]bool{}
	for _, pf := range files {
		for _, fn := range funcsR364(pf.file) {
			calls := callNamesR364(fn)
			if calls["checkFlashAttnKVCacheR367"] || calls["flashAttnKVCacheConflictR367"] {
				seen[fn.Name.Name] = true
			}
		}
	}
	for _, want := range wantPaths {
		if !seen[want] {
			t.Errorf("%s: путь загрузки не проверяет пару «flash attention + kvCacheType».\n"+
				"    При flashAttn=false и kvCacheType=q4_0 клиент получит невнятное «failed to create context»\n"+
				"    вместо причины — ровно дефект R367.", want)
		}
	}
}

// containsR367 — подстрока без импорта strings.
func containsR367(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
