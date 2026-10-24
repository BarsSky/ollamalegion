package i18n

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParse — значения настройки, включая auto и мусор.
func TestParse(t *testing.T) {
	cases := []struct {
		in    string
		lang  Lang
		auto  bool
		ok    bool
		title string
	}{
		{"ru", RU, false, true, "ru"},
		{"RU", RU, false, true, "регистр не важен"},
		{" en ", EN, false, true, "пробелы по краям"},
		{"English", EN, false, true, "синоним"},
		{"auto", "", true, true, "auto"},
		{"", "", true, true, "пусто = auto"},
		{"de", "", false, false, "неизвестный язык отвергается"},
	}
	for _, c := range cases {
		lang, auto, ok := Parse(c.in)
		if ok != c.ok {
			t.Fatalf("Parse(%q): ok=%v, want %v (%s)", c.in, ok, c.ok, c.title)
		}
		if !ok {
			continue
		}
		if auto != c.auto {
			t.Fatalf("Parse(%q): auto=%v, want %v (%s)", c.in, auto, c.auto, c.title)
		}
		if !auto && lang != c.lang {
			t.Fatalf("Parse(%q): lang=%q, want %q (%s)", c.in, lang, c.lang, c.title)
		}
	}
}

// TestFromEnvOrder — приоритет переменных: первый заданный побеждает.
func TestFromEnvOrder(t *testing.T) {
	t.Setenv("I18N_T_A", "en")
	t.Setenv("I18N_T_B", "ru")
	lang, raw, src := FromEnv("I18N_T_EMPTY", "I18N_T_A", "I18N_T_B")
	if lang != EN || raw != "en" || src != "I18N_T_A" {
		t.Fatalf("FromEnv = (%q,%q,%q), want (en,en,I18N_T_A)", lang, raw, src)
	}
	// auto берётся из локали: в тестах LANG почти всегда не русский, поэтому
	// проверяем только то, что язык валидный и источник найден.
	t.Setenv("I18N_T_C", "auto")
	lang, _, src = FromEnv("I18N_T_C")
	if src != "I18N_T_C" || (lang != RU && lang != EN) {
		t.Fatalf("auto: got lang=%q src=%q", lang, src)
	}
	if _, _, src := FromEnv("I18N_T_NONE"); src != "" {
		t.Fatalf("expected empty source for unset variables, got %q", src)
	}
}

// TestFromLocale — LANG=ru_* даёт RU, LANG=en_* даёт EN.
func TestFromLocale(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ru_RU.UTF-8")
	if got := FromLocale(); got != RU {
		t.Fatalf("LANG=ru_RU: got %q, want ru", got)
	}
	t.Setenv("LANG", "en_US.UTF-8")
	if got := FromLocale(); got != EN {
		t.Fatalf("LANG=en_US: got %q, want en", got)
	}
	t.Setenv("LANG", "de_DE.UTF-8")
	if got := FromLocale(); got != RU {
		t.Fatalf("unknown locale must fall back to ru, got %q", got)
	}
}

// TestTranslationFallbackAndArgs — перевод, подстановка аргументов, фолбэк.
func TestTranslationFallbackAndArgs(t *testing.T) {
	Set(EN)
	defer Set(RU)
	if got := T("balancer.env.log_level", "debug"); !strings.Contains(got, "debug") ||
		!strings.Contains(got, "overrides") {
		t.Fatalf("EN translation not applied: %q", got)
	}
	Set(RU)
	if got := T("balancer.env.log_level", "debug"); !strings.Contains(got, "переопределяет") {
		t.Fatalf("RU translation not applied: %q", got)
	}
	// Неизвестный ключ возвращается как есть: молчаливая пустая строка скрыла бы
	// отсутствующий перевод в логе.
	if got := T("no.such.key"); got != "no.such.key" {
		t.Fatalf("unknown key must be returned verbatim, got %q", got)
	}
}

// TestCatalogVerbParity — набор fmt-глаголов в ru и en обязан совпадать,
// иначе %!s(MISSING) или лишний аргумент в одном из языков.
func TestCatalogVerbParity(t *testing.T) {
	verb := regexp.MustCompile(`%[-+#0-9.*]*[a-zA-Z]`)
	for key, entry := range messages {
		ru, okRU := entry[RU]
		en, okEN := entry[EN]
		if !okRU || ru == "" {
			t.Fatalf("key %q: missing RU text", key)
		}
		if !okEN || en == "" {
			t.Fatalf("key %q: missing EN text", key)
		}
		ruVerbs := strings.Join(verb.FindAllString(ru, -1), ",")
		enVerbs := strings.Join(verb.FindAllString(en, -1), ",")
		if ruVerbs != enVerbs {
			t.Fatalf("key %q: verb mismatch ru=[%s] en=[%s]", key, ruVerbs, enVerbs)
		}
	}
}

// TestSetIgnoresUnknown — опечатка в env не должна ломать действующий язык.
func TestSetIgnoresUnknown(t *testing.T) {
	Set(RU)
	Set(Lang("klingon"))
	if Get() != RU {
		t.Fatalf("Set(unknown) changed the language to %q", Get())
	}
	Set(EN)
	if Get() != EN {
		t.Fatalf("Set(en) did not apply, got %q", Get())
	}
	Set(RU)
}

// TestConcurrentSetAndT — переключение из WebUI не должно гонять с логами.
func TestConcurrentSetAndT(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				Set(EN)
			} else {
				Set(RU)
			}
		}(i)
		go func() {
			defer wg.Done()
			_ = T("balancer.env.log_level", "debug")
		}()
	}
	wg.Wait()
	Set(RU)
}

// TestRemoteSyncAppliesBalancerLanguage — воркер подхватывает язык с балансера
// и не меняет его, если балансер недоступен.
func TestRemoteSyncAppliesBalancerLanguage(t *testing.T) {
	Set(RU)
	defer Set(RU)

	var answer atomicString
	answer.set("en")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/i18n" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Token") != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"language": answer.get()})
	}))
	defer srv.Close()

	rs := NewRemoteSync(srv.URL, "tok", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := rs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return Get() == EN })

	answer.set("ru")
	rs.applyOnce(ctx)
	if Get() != RU {
		t.Fatalf("language not switched back to ru, got %q", Get())
	}

	// Балансер «выключился»: язык не должен сбрасываться на локальный.
	srv.Close()
	answer.set("en")
	rs.applyOnce(ctx)
	if Get() != RU {
		t.Fatalf("unreachable balancer must not change language, got %q", Get())
	}
}

// TestStartFromBalancerEmptyURL — без адреса синхронизация не запускается.
func TestStartFromBalancerEmptyURL(t *testing.T) {
	cancel, err := StartFromBalancer(context.Background(), "", "")
	if err != nil || cancel == nil {
		t.Fatalf("StartFromBalancer(empty) = (%v,%v), want (func,nil)", cancel, err)
	}
	cancel()
}

// TestBootstrapFromEnv — локальная настройка применяется до синхронизации.
func TestBootstrapFromEnv(t *testing.T) {
	Set(RU)
	t.Setenv("CPPWORKER_LOG_LANG", "en")
	defer Set(RU)
	if got := BootstrapFromEnv("cppworker"); got != EN {
		t.Fatalf("BootstrapFromEnv = %q, want en", got)
	}
	if Get() != EN {
		t.Fatalf("BootstrapFromEnv did not set the global language")
	}
}

// TestNoDuplicateKeys — один ключ не может быть зарегистрирован дважды:
// иначе один текст молча затирает другой (Register это отслеживает).
func TestNoDuplicateKeys(t *testing.T) {
	if dups := Duplicates(); len(dups) > 0 {
		t.Fatalf("duplicate message keys: %v", dups)
	}
}

// TestAllKeysHaveTranslations — каталог не должен содержать ключей без EN.
func TestAllKeysHaveTranslations(t *testing.T) {
	for key, entry := range messages {
		if _, ok := entry[EN]; !ok {
			t.Fatalf("key %q has no EN translation", key)
		}
	}
	if len(messages) == 0 {
		t.Fatal("catalog is empty")
	}
}

// --- helpers ---------------------------------------------------------------

type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// ссылка на log и fmt, чтобы не потерялись при будущих правках теста
var _ = log.Printf
var _ = fmt.Sprintf
var _ = os.Getenv
