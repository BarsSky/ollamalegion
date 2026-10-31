// agent_host_keep_r357.go — R357 (2026-10-31): регистрация агента не перезаписывает
// РАБОЧИЙ адрес узла.
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд). Оператор исправил адрес донора через
// PUT /api/v1/backends/{id}: 192.168.13.34 → 100.64.0.7 (старый адрес мёртв, новый
// отвечает). Через некоторое время адрес САМ вернулся к 192.168.13.34, раскладка
// снова ушла в пустоту, health дал провал.
//
// Причина — в обработчике регистрации агента (handlers_agents.go): он собирает запись
// бэкенда ЗАНОВО, и `Host` берётся из payload агента (`req.Host` / `Hostname` /
// `RemoteAddr`). Агент донора считает своим адресом 192.168.13.34 (у него так задано
// локально), поэтому каждое его переподключение откатывало операторскую правку.
//
// ПРАВИЛО. Если у записи уже есть адрес и он СЕЙЧАС ОТВЕЧАЕТ, а пришедший отличается —
// оставляем рабочий адрес и пишем предупреждение. Если рабочий адрес не отвечает,
// пришедший принимается: именно агент знает правду после переезда узла. Это то же
// рассуждение, что в R350 (сообщённый адрес не должен побеждать живой), но на уровне
// записи, а не раскладки.
package api

import (
	"net"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// agentHostDialTimeoutR357 — сколько ждём TCP-подключение при проверке адреса.
// Регистрация агента — редкое событие, но и держать её минутами нельзя.
const agentHostDialTimeoutR357 = 1500 * time.Millisecond

// agentHostReachableR357 — отвечает ли адрес узла (TCP-подключение к порту воркера).
//
// Подменяется в тестах: реальный dial в юнит-тестах означал бы зависимость от сети.
var agentHostReachableR357 = func(host string, port int) bool {
	if host == "" || port <= 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), agentHostDialTimeoutR357)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// resolveRegisteredHostR357 — какой адрес записать при регистрации агента.
//
// Возвращает адрес и признак «оставили прежний» (для лога и для тестов).
func resolveRegisteredHostR357(existingHost, incomingHost string, port int) (string, bool) {
	existing := strings.TrimSpace(existingHost)
	incoming := strings.TrimSpace(incomingHost)

	if incoming == "" {
		return existing, existing != ""
	}
	if existing == "" || strings.EqualFold(existing, incoming) {
		return incoming, false
	}
	// Адреса разные: верим тому, который отвечает. Живой прежний адрес — операторская
	// правка, и регистрация агента не имеет права её откатывать.
	if agentHostReachableR357(existing, port) {
		if logger.Get() != nil {
			logger.Get().Warnw("registration: адрес узла не перезаписан — прежний отвечает",
				"existing_host", existing, "incoming_host", incoming, "port", port,
				"hint", "агент сообщает свой адрес, но рабочий адрес записи другой; "+
					"сменить адрес можно через PUT /api/v1/backends/{id}")
		}
		return existing, true
	}
	if logger.Get() != nil {
		logger.Get().Infow("registration: адрес узла обновлён по регистрации",
			"existing_host", existing, "incoming_host", incoming, "port", port)
	}
	return incoming, false
}
