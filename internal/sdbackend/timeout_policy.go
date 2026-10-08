// timeout_policy.go — R88 (2026-10-08). Единое место доктрины таймаутов для
// image-воркера: ТА ЖЕ логика, что уже работает у текстового бэкенда
// (internal/balancer/timeout_policy.go, R83/v67).
//
// ПРАВИЛО.
//
//	Разрешены только таймауты ОПРОСА СОСТОЯНИЯ — «каждые N секунд проверить,
//	готово ли» (интервал поллинга readiness/джобы, heartbeat, idle-соединение).
//	Такой таймер задаёт частоту взгляда и НЕ отменяет работу.
//
//	Запрещены duration-капы на РАБОТУ: любой context.WithTimeout /
//	http.Client.Timeout, который по истечении срока ОБРЫВАЕТ загрузку модели
//	или генерацию. Признак дефекта ровно тот, что видел оператор: воркер
//	возвращает ошибку (`generation_timeout` → HTTP 504), а sd-server продолжает
//	считать; клиент получает «HTTP 504» без объяснения, картинка теряется, и
//	состояние расходится с ответом.
//
//	Вместо капа — ждать ТЕРМИНАЛЬНОГО СОСТОЯНИЯ (completed / failed /
//	cancelled / смерть процесса sd-server) и вернуть конкретную ошибку.
//
// ОПТ-ИН. Кап всё-таки можно взвести — но только явно, и тогда он обязан быть
// виден в логе (WarnArmedCaps печатает WARN на старте воркера):
//
//	SDWORKER_GENERATION_TIMEOUT_SEC — общий кап ожидания джобы генерации;
//	SDWORKER_STARTUP_TIMEOUT_SEC    — кап ожидания readiness sd-server;
//	SDWORKER_ALLOW_PROFILE_TIMEOUTS — разрешить per-model profile.timeoutSec.
//	                                  По умолчанию профильный кап ИГНОРИРУЕТСЯ:
//	                                  значение в profile.json не должно молча
//	                                  обрезать легитимную генерацию (в точности
//	                                  как LB_ALLOW_PROFILE_TIMEOUTS у текста).
package sdbackend

import (
	"context"
	"os"
	"strings"
	"time"
)

// Имена переменных окружения — экспортируются, чтобы cmd/sdworker и WebUI
// показывали оператору те же имена, что читает код.
const (
	EnvGenerationTimeout    = "SDWORKER_GENERATION_TIMEOUT_SEC"
	EnvStartupTimeout       = "SDWORKER_STARTUP_TIMEOUT_SEC"
	EnvAllowProfileTimeouts = "SDWORKER_ALLOW_PROFILE_TIMEOUTS"
)

// envTruthy — on/1/true/yes (регистр не важен).
func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// ProfileTimeoutsAllowed — разрешены ли per-model таймауты из profile.json.
//
// По умолчанию НЕТ: профиль описывает модель (роли файлов, дефолты генерации,
// плейсмент), а не политику таймаутов стенда. Кап из профиля включается только
// явным SDWORKER_ALLOW_PROFILE_TIMEOUTS=on.
func ProfileTimeoutsAllowed() bool {
	return envTruthy(EnvAllowProfileTimeouts)
}

// WarnArmedCaps — печатает WARN по каждому взведённому капу.
//
// ЗАЧЕМ: главный вопрос оператора при обрыве — «почему оборвалось». Если кап
// взведён, ответ обязан быть в логе при старте, а не выясняться чтением кода.
// Вызывается из cmd/sdworker после загрузки конфига.
func WarnArmedCaps(cfg Config) {
	log := sdLog()
	if log == nil {
		return
	}
	if cfg.GenerationTimeoutSec > 0 {
		log.Warnw("generation timeout armed (operator opt-in)",
			"env", EnvGenerationTimeout, "timeout_sec", cfg.GenerationTimeoutSec,
			"hint", "по умолчанию капа нет: ждём терминального состояния джобы (completed/failed/cancelled)")
	}
	if cfg.StartupTimeoutSec > 0 {
		log.Warnw("startup (readiness) timeout armed (operator opt-in)",
			"env", EnvStartupTimeout, "timeout_sec", cfg.StartupTimeoutSec,
			"hint", "по умолчанию ждём ready ИЛИ смерти процесса sd-server (это терминальное состояние с внятной ошибкой)")
	}
	if ProfileTimeoutsAllowed() {
		log.Warnw("per-model profile timeouts allowed (operator opt-in)",
			"env", EnvAllowProfileTimeouts,
			"hint", "profile.timeoutSec из profile.json будет ОБРЫВАТЬ генерацию; уберите флаг, чтобы кап профиля игнорировался")
	}
}

// WithOptionalTimeout — context с дедлайном ТОЛЬКО если кап взведён.
//
// d <= 0 → обычный cancel-context без дедлайна: работа идёт до терминального
// состояния (или до отмены родителя). Именно так доктрина заменяет «кап»:
// таймер не отменяет работу сам по себе.
func WithOptionalTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(parent, d)
	}
	return context.WithCancel(parent)
}
