// handlers_effective_r116_test.go — R116: контракт приоритета параметров.
//
// Приоритет — это правило, а не деталь реализации: «параметр запроса > профиль
// модели > дефолт бэкенда», причём последнее слово за пользователем (значение
// профиля не подменяется дефолтом). Поэтому проверяется именно правило, а не
// форма ответа.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestR116_PrecedenceRequestWins(t *testing.T) {
	got, src := resolveEffectiveInt(32768, 8192, 2048)
	if got != 32768 || src != sourceRequest {
		t.Errorf("R116: запрос обязан побеждать, получено %d/%s", got, src)
	}
	if s, src := resolveEffectiveStr("q8_0", "q4_0", "f16"); s != "q8_0" || src != sourceRequest {
		t.Errorf("R116: KV из запроса обязан побеждать, получено %q/%s", s, src)
	}
}

func TestR116_PrecedenceProfileBeatsDefault(t *testing.T) {
	// Главный случай жалобы: в настройках стоит одно, а применяется профиль.
	// Теперь это не «подмена», а явно названный источник.
	got, src := resolveEffectiveInt(0, 8192, 65536)
	if got != 8192 || src != sourceProfile {
		t.Errorf("R116: профиль обязан побеждать дефолт, получено %d/%s", got, src)
	}
	if s, src := resolveEffectiveStr("", "q4_0", "f16"); s != "q4_0" || src != sourceProfile {
		t.Errorf("R116: KV из профиля обязан побеждать дефолт, получено %q/%s", s, src)
	}
}

func TestR116_PrecedenceDefaultWhenNothingElse(t *testing.T) {
	got, src := resolveEffectiveInt(0, 0, 4096)
	if got != 4096 || src != sourceDefault {
		t.Errorf("R116: без запроса и профиля действует дефолт, получено %d/%s", got, src)
	}
	if _, src := resolveEffectiveInt(0, 0, 0); src != sourceUnset {
		t.Errorf("R116: «нигде не задано» обязано называться unset, получено %s", src)
	}
	if _, src := resolveEffectiveStr("", "", ""); src != sourceUnset {
		t.Errorf("R116: пустая строка без источников = unset, получено %s", src)
	}
}

func TestR116_EffectiveHandlerRequiresModel(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models/effective", nil)
	w := httptest.NewRecorder()
	handleModelEffective(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("R116: без ?model= ожидался 400, получено %d", w.Code)
	}
}

func TestR116_EffectiveHandlerRejectsWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/models/effective?model=m", nil)
	w := httptest.NewRecorder()
	handleModelEffective(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("R116: POST не поддерживается, ожидался 405, получено %d", w.Code)
	}
}

func TestR116_EffectiveHandlerWithoutBackend(t *testing.T) {
	prev := backend
	backend = nil
	defer func() { backend = prev }()

	req := httptest.NewRequest(http.MethodGet, "/api/models/effective?model=m", nil)
	w := httptest.NewRecorder()
	handleModelEffective(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("R116: без бэкенда ожидался 503, получено %d", w.Code)
	}
}

func TestR116_ProfileMapIsEmptyWithoutProfile(t *testing.T) {
	// Отсутствие профиля — законное состояние: эндпоинт обязан показать пусто,
	// а не подставить дефолт под видом профиля.
	m := profileAsMap("модель-которой-нет-в-профилях")
	if len(m) != 0 {
		t.Errorf("R116: без профиля ожидалась пустая карта, получено %v", m)
	}
	b, err := json.Marshal(m)
	if err != nil || string(b) != "{}" {
		t.Errorf("R116: пустой профиль должен сериализоваться в {}, получено %s (err=%v)", b, err)
	}
}
