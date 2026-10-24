package types

import "testing"

// R119: state модели cppworker не должен толковаться каждым потребителем
// по-своему. Живой дефект (2026-10-15): cppworker сообщал state="loading",
// context_size=0, а WebUI писал «загружено».
func TestNormalizeModelState_R119(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// cppworker всегда отдаёт state для загруженной модели.
		{"loaded", WireStateLoaded},
		{"LOADED", WireStateLoaded},
		{"  loaded  ", WireStateLoaded},
		{"ready", WireStateLoaded},
		// Пустое значение = старые сборки cppworker: трактуем как loaded,
		// чтобы не «потерять» реально загруженную модель.
		{"", WireStateLoaded},
		{"   ", WireStateLoaded},
		// Загрузка в полёте.
		{"loading", WireStateLoading},
		{"Loading", WireStateLoading},
		// Провал загрузки.
		{"error", WireStateError},
		{"failed", WireStateError},
		// Неизвестное значение проходит как есть (lower-case), чтобы не врать.
		{"unloading", "unloading"},
	}
	for _, tc := range cases {
		if got := NormalizeModelState(tc.in); got != tc.want {
			t.Errorf("NormalizeModelState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ModelStateUsable — единственный критерий «модель обслуживает запросы сейчас».
func TestModelStateUsable_R119(t *testing.T) {
	usable := []string{"", "loaded", "LOADED", "ready"}
	for _, s := range usable {
		if !ModelStateUsable(s) {
			t.Errorf("ModelStateUsable(%q) = false, want true", s)
		}
	}
	notUsable := []string{"loading", "Loading", "error", "failed", "unloading"}
	for _, s := range notUsable {
		if ModelStateUsable(s) {
			t.Errorf("ModelStateUsable(%q) = true, want false (модель ещё/уже не обслуживает)", s)
		}
	}
}
