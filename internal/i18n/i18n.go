// Package i18n — R234 (2026-10-24): локализация логов и ошибок.
//
// ЗАЧЕМ. До R234 тексты логов и сообщений об ошибках были жёстко русскими:
// оператор с англоязычной командой видел `[ENV] LOG_LEVEL=debug переопределяет
// уровень из конфига`, а клиенты — русские `error` в JSON. Перевести это через
// WebUI было нельзя: язык интерфейса (webui/js/i18n) не влияет на Go-процессы.
//
// КАК УСТРОЕНО. Общий каталог сообщений с ключами (`messages.{ru,en}`) и
// глобальным «текущим языком». Источники языка, по приоритету:
//
//	flag -log-lang  >  env (OLLAMALEGION_LOG_LANG / LB_LOG_LANG / <COMPONENT>_LOG_LANG)
//	                >  config.json (logging.language)  >  LANG/LC_ALL  >  ru
//
// Балансер — источник истины: он выставляет язык, отдаёт его через
// GET /api/v1/i18n и принимает смену через PUT (WebUI). Воркеры (cppworker,
// agent) подтягивают язык с балансера (StartRemoteSync), поэтому переключение
// в WebUI действует на весь стенд, а не только на балансер.
//
// ЧЕСТНОЕ ОГРАНИЧЕНИЕ. Каталог растёт постепенно: сообщение, для которого нет
// ключа, остаётся в исходном виде (см. scripts/i18n-scan.py — он считает, сколько
// таких мест осталось, и не даёт числу расти).
package i18n

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Lang — поддерживаемый язык сообщений.
type Lang string

const (
	// RU — русский (исходный язык проекта, поведение по умолчанию).
	RU Lang = "ru"
	// EN — английский.
	EN Lang = "en"
)

// Auto — значение настройки «взять из окружения/локали» (в Get не попадает).
const Auto = "auto"

// currentLang хранит действующий язык. atomic.Value, чтобы переключение из
// WebUI не требовало блокировок на каждом log-вызове.
var currentLang atomic.Value

func init() {
	currentLang.Store(RU)
}

// Supported — список языков, которые умеет отдавать каталог.
func Supported() []string { return []string{string(RU), string(EN)} }

// Parse разбирает значение настройки. Помимо языков принимает auto
// (lang==Auto, ok==true): «выбрать по LANG/LC_ALL, при неясности — RU».
func Parse(v string) (lang Lang, auto bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", string(RU), "russian", "русский":
		if strings.TrimSpace(v) == "" {
			return "", true, true
		}
		return RU, false, true
	case string(EN), "english", "английский":
		return EN, false, true
	case Auto:
		return "", true, true
	default:
		return "", false, false
	}
}

// FromLocale угадывает язык по POSIX-локали (LANG, LC_ALL).
func FromLocale() Lang {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "ru") {
			return RU
		}
		if strings.HasPrefix(v, "en") {
			return EN
		}
	}
	return RU
}

// FromEnv ищет настройку в перечисленных переменных окружения. Возвращает
// язык, найденное значение и переменную-источник (для лога о том, откуда взят).
func FromEnv(names ...string) (Lang, string, string) {
	for _, name := range names {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		lang, auto, ok := Parse(raw)
		if !ok {
			log.Printf("[i18n] ignoring %s=%q: expected ru, en or auto", name, raw)
			continue
		}
		if auto {
			return FromLocale(), raw, name
		}
		return lang, raw, name
	}
	return "", "", ""
}

// Set переключает действующий язык. Неизвестное значение игнорируется,
// чтобы опечатка в env не оставила процесс без сообщений.
func Set(l Lang) {
	if l != RU && l != EN {
		return
	}
	currentLang.Store(l)
}

// Get возвращает действующий язык (всегда RU или EN).
func Get() Lang {
	v, _ := currentLang.Load().(Lang)
	if v != RU && v != EN {
		return RU
	}
	return v
}

// T переводит ключ на действующий язык и подставляет аргументы (fmt).
// Нет перевода на языке — отдаётся русский текст; нет ключа — сам ключ,
// чтобы отсутствие сообщения было видно в логе, а не терялось.
func T(key string, args ...any) string { return Tf(Get(), key, args...) }

// Tf переводит ключ на явно указанный язык.
func Tf(lang Lang, key string, args ...any) string {
	msg, ok := message(key, lang)
	if !ok {
		if msg, ok = message(key, RU); !ok {
			return key
		}
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// Logf печатает переведённое сообщение в стандартный лог.
func Logf(key string, args ...any) { log.Print(T(key, args...)) }
