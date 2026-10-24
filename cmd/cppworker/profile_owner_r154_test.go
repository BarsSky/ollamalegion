package main

import "testing"

// R154 (2026-10-16): профиль ищется у ВЛАДЕЛЬЦА ФАЙЛА, а не у имени запроса.
//
// Живой случай, ради которого тест: загрузка под коротким именем `gemma-4` поднимала
// тот же файл `gemma-4-E4B-it-Q4_K_M.gguf`, но профиль подбирался по имени запроса —
// срабатывала отдельная запись `gemma-4` (ctx=32768) или env-дефолты, и параметры
// оператора (8192/4096/q4_0/reasoning on) молча терялись.
func TestR154ProfileLookupNameIsFileOwner(t *testing.T) {
	cases := []struct {
		requested string
		path      string
		want      string
		why       string
	}{
		{
			requested: "gemma-4",
			path:      "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
			want:      "gemma-4-E4B-it-Q4_K_M",
			why:       "алиас -> профиль канонической модели (живой дефект)",
		},
		{
			requested: "gemma-4-large",
			path:      "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
			want:      "gemma-4-E4B-it-Q4_K_M",
			why:       "любой алиас того же файла -> тот же владелец",
		},
		{
			requested: "gemma-4-E4B-it-Q4_K_M",
			path:      "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
			want:      "gemma-4-E4B-it-Q4_K_M",
			why:       "каноническое имя не меняется",
		},
		{
			requested: "m",
			path:      "/models/other-model.gguf",
			want:      "other-model",
			why:       "даже при несовпадении имени владелец файла главнее",
		},
		{
			requested: "m",
			path:      "",
			want:      "m",
			why:       "путь неизвестен -> не гадаем, оставляем имя запроса",
		},
		{
			requested: "m",
			path:      "/models/",
			want:      "m",
			why:       "каталог вместо файла -> имя запроса",
		},
		{
			requested: "m",
			path:      "C:\\models\\name.gguf",
			want:      "name",
			why:       "windows-путь тоже разбирается",
		},
		{
			requested: "m",
			path:      "/models/noext",
			want:      "noext",
			why:       "файл без расширения — владелец это его имя",
		},
	}
	for _, c := range cases {
		got := profileLookupNameForPath(c.requested, c.path)
		if got != c.want {
			t.Errorf("profileLookupNameForPath(%q, %q) = %q, want %q (%s)",
				c.requested, c.path, got, c.want, c.why)
		}
	}
}

// R157 (2026-10-16): КАНОНИЗАЦИЯ ИМЕНИ при загрузке.
//
// Живой дефект: инстанс, поднятый под алиасом (`gemma-4`), не мог обслуживать запросы
// по каноническому имени — воркер отвечал
//   "chat generate failed: model gemma-4-E4B-it-Q4_K_M not found" → балансер 502.
func TestR157CanonicalModelName(t *testing.T) {
	// Алиас -> имя владельца файла (именно это грузим и регистрируем).
	if got := canonicalModelName("gemma-4", "/app/models/gemma-4-E4B-it-Q4_K_M.gguf"); got != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("алиас не канонизирован: got %q", got)
	}
	// Каноническое имя не меняется (иначе клиент получил бы другое имя без причины).
	if got := canonicalModelName("m", "/models/m.gguf"); got != "m" {
		t.Errorf("каноническое имя изменилось: %q", got)
	}
	// Путь неизвестен -> имя запроса как есть.
	if got := canonicalModelName("alias", ""); got != "alias" {
		t.Errorf("без пути имя должно остаться: %q", got)
	}
	// Путь-каталог владельцем не является (поймано тестом R154).
	if got := canonicalModelName("alias", "/models/"); got != "alias" {
		t.Errorf("каталог не должен давать владельца: %q", got)
	}
}