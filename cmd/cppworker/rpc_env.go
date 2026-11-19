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
//
// R234: тексты ошибок вынесены в каталог i18n (internal/i18n), поэтому оператор
// с английской локалью видит английское сообщение, а не русское в логе.
package main

import (
	"errors"
	"net"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/i18n"
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
		return nil, errors.New(i18n.T("cppworker.rpc_env.empty_list"))
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxRPCServersR101 {
		return nil, errors.New(i18n.T("cppworker.rpc_env.too_many", len(parts), maxRPCServersR101))
	}

	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for i, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			return nil, errors.New(i18n.T("cppworker.rpc_env.empty_item", i+1))
		}
		host, portStr, err := net.SplitHostPort(entry)
		if err != nil {
			return nil, errors.New(i18n.T("cppworker.rpc_env.bad_hostport", i+1, entry, err))
		}
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, errors.New(i18n.T("cppworker.rpc_env.empty_host", i+1, entry))
		}
		port, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil {
			return nil, errors.New(i18n.T("cppworker.rpc_env.bad_port", i+1, entry))
		}
		if port < 1 || port > 65535 {
			return nil, errors.New(i18n.T("cppworker.rpc_env.port_range", i+1, entry, port))
		}
		normalized := net.JoinHostPort(host, strconv.Itoa(port))
		if seen[normalized] {
			return nil, errors.New(i18n.T("cppworker.rpc_env.duplicate_host", i+1, entry))
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out, nil
}
