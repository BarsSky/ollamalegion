// Package main — R101 (S1 плана 2026-10-07): разбор RPC-переменных cppworker.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ФАЙЛ. Разбор `CPPWORKER_RPC_SERVERS` — это то, что оператор
// вводит руками в docker-compose, и ошибка здесь выглядит как «модель грузится
// вечность» (таймаут) вместо понятного сообщения. Поэтому разбор вынесен в
// чистую функцию с тестами: список узлов либо принимается целиком, либо
// отвергается с указанием, ЧТО именно не так.
//
// ЧЕГО В ЭТОЙ СБОРКЕ ПОКА НЕТ. Транспорта: C-мост (`c/bridge`) не прокидывает
// rpc_servers в устройства llama.cpp — проверено, в bridge.c нет ни одного
// упоминания rpc. Поэтому вызов с включённым RPC честно логирует, что раскладка
// не заработает, вместо тихой видимости настройки: оператор не должен гадать,
// почему вторая машина простаивает.
package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// maxRPCServersR101 — предел числа RPC-узлов в списке.
//
// Не техническое ограничение llama.cpp, а защита от опечатки в compose
// (случайно вставленный огромный список превратил бы загрузку в минуты
// таймаутов, а диагностику — в археологию).
const maxRPCServersR101 = 16

// parseRPCServersR101 разбирает `host:port,host:port`.
//
// Возвращает либо полный список, либо ошибку с указанием позиции и причины:
// «третий элемент: нет порта». Частично принятый список здесь опаснее отказа —
// мастер молча считал бы на части узлов, а модель была бы разрезана не так.
func parseRPCServersR101(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("пустой список")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxRPCServersR101 {
		return nil, fmt.Errorf("узлов %d, максимум %d (проверьте, не лишний ли элемент в списке)",
			len(parts), maxRPCServersR101)
	}

	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for i, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			return nil, fmt.Errorf("элемент %d пуст (лишняя запятая?)", i+1)
		}
		host, portStr, err := net.SplitHostPort(entry)
		if err != nil {
			return nil, fmt.Errorf("элемент %d (%q): ожидается host:port — %v", i+1, entry, err)
		}
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, fmt.Errorf("элемент %d (%q): пустой host", i+1, entry)
		}
		port, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil {
			return nil, fmt.Errorf("элемент %d (%q): порт не число", i+1, entry)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("элемент %d (%q): порт %d вне диапазона 1..65535", i+1, entry, port)
		}
		normalized := net.JoinHostPort(host, strconv.Itoa(port))
		if seen[normalized] {
			return nil, fmt.Errorf("элемент %d (%q): узел указан дважды", i+1, entry)
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out, nil
}
