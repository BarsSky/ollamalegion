// timeout_policy.go — R83/v67 (2026-10-02). Единое место доктрины таймаутов.
//
// ПРАВИЛО СТЕНДА.
//
//	Разрешены только таймауты ОПРОСА СОСТОЯНИЯ — «каждые N секунд проверить,
//	готово ли» (poll interval, health probe, метрики, heartbeat, idle-соединение
//	между запросами). Это НЕ лимит на работу: такой таймер лишь задаёт частоту
//	взгляда и никогда не отменяет саму операцию.
//
//	Запрещены duration-капы на РАБОТУ: любой time.After/context.WithTimeout/
//	http.Client.Timeout, который по истечении срока ОБРЫВАЕТ загрузку модели,
//	генерацию, pull, reload, unload. Признак дефекта: сервер возвращает ошибку,
//	а upstream продолжает работу — состояние расходится, оператор видит
//	«модель не грузится / ответ обрезан / context canceled», и причину ищут
//	вслупую.
//
//	Вместо капа — ждать ТЕРМИНАЛЬНОГО СОСТОЯНИЯ (успех / явная ошибка /
//	недоступность бэкенда) и возвращать конкретную ошибку.
//
// ОПТ-ИН. Если кап всё-таки нужен (защита от багованного upstream), он
// включается ТОЛЬКО явной переменной окружения и при взведении печатает
// громкий WARN с именем переменной — чтобы «почему оборвалось» имело ответ в
// логе. Реестр переменных:
//
//	LB_ALLOW_PROFILE_TIMEOUTS        — таймауты из per-model профилей (WebUI)
//	LB_ALLOW_REQUEST_TIMEOUTS        — адаптивный per-request таймаут (proxy)
//	LB_ALLOW_MODEL_OP_TIMEOUT_SEC    — HTTP-клиент операций с моделью (pull/push)
//	LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC  — HTTP-запрос загрузки модели
//	LB_ALLOW_LOAD_WAIT_CAP_SEC       — ожидание готовности модели (poll-циклы)
//	LB_ALLOW_NCTX_RELOAD_TIMEOUT     — reload/preflight контекста
//	NCTX_PREFLIGHT_WAIT_TIMEOUT_SEC  — ожидание preflight-диалога (0 = ждать)
//	CPPWORKER_WRITE_TIMEOUT          — HTTP WriteTimeout сервера cppworker
//	CPPWORKER_READ_TIMEOUT           — HTTP ReadTimeout сервера cppworker
//	CPPWORKER_LAZY_LOAD_TIMEOUT_SEC  — контекст ленивой загрузки модели
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// OptInTimeoutSeconds — экспорт optInTimeoutSeconds для соседних пакетов
// (internal/api проксирует WebUI → cppworker и обязан подчиняться той же
// доктрине: кап только явным opt-in и с WARN в логе).
func OptInTimeoutSeconds(envName, what string) time.Duration {
	return optInTimeoutSeconds(envName, what)
}

// optInTimeoutSeconds — читает opt-in кап из переменной окружения.
//
// Пусто/некорректно/<=0 → 0, то есть «без капа» (ждать терминального
// состояния). Ненулевое значение печатает WARN с именем переменной: взведённый
// кап обязан быть виден в логе.
func optInTimeoutSeconds(envName, what string) time.Duration {
	v := strings.TrimSpace(os.Getenv(envName))
	if v == "" {
		return 0
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec <= 0 {
		if logger.Get() != nil {
			logger.Get().Warnw("timeout opt-in ignored (invalid value)",
				"env", envName, "value", v, "what", what,
				"hint", "ожидалось положительное число секунд; действует режим «без капа»")
		}
		return 0
	}
	if logger.Get() != nil {
		logger.Get().Warnw("timeout armed (operator opt-in)",
			"env", envName, "timeout_sec", sec, "what", what,
			"hint", "по умолчанию капа нет: ждём терминального состояния (успех/ошибка/недоступность)")
	}
	return time.Duration(sec) * time.Second
}
