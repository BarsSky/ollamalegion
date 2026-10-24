// netlink_r166.go — R166 (2026-10-17): факты о сетевом интерфейсе бэкенда.
//
// ЗАЧЕМ. Пропускная способность канала — ключевой параметр раскладки слоёв между
// хостами: по нему оценивается трафик (composition.EstimateTraffic) и решается
// «layer или row». До R166 оператор вписывал число руками в .env
// (LB_RPC_LINK_MBPS), и оно молча устаревало.
//
// ЧТО ЗДЕСЬ. Бэкенд сам сообщает то, что о своей сети знает ДЁШЕВО и без нагрузки:
// интерфейс, MTU и (если ядро отдаёт) скорость линка из
// /sys/class/net/<iface>/speed.
//
// ЧЕСТНО О ГРАНИЦАХ. В контейнере это обычно veth: его speed либо отсутствует,
// либо равен скорости хостового линка, и НИ ТО НИ ДРУГОЕ не равно реальной
// пропускной способности пути «контейнер ↔ контейнер». Поэтому поле
// помечено source=detected и НЕ подменяет замер: настоящую цифру даёт авто-замер
// балансера (/api/rpc-link-probe, см. internal/balancer/rpc_link_measure_r118.go).
// Бэкенд сообщает факт, балансер — измерение, и в панели они разные.
package main

import (
	"net"
	"os"
	"strconv"
	"strings"
)

// netLinkFacts — что бэкенд сообщает о своей сети.
type netLinkFacts struct {
	Interface  string `json:"interface,omitempty"`
	MTU        int    `json:"mtu,omitempty"`
	SpeedMbps  int    `json:"speed_mbps,omitempty"`
	Source     string `json:"source"`
	Note       string `json:"note,omitempty"`
	IsLoopback bool   `json:"loopback,omitempty"`
}

// detectNetLinkFacts — лучший доступный ответ о сетевом пути бэкенда.
//
// Порядок: интерфейс, через который виден дефолтный маршрут (по адресу хоста
// cppworker'а), иначе первый не-loopback интерфейс. Скорость читаем из sysfs —
// единственный честный источник «Mbps» в Linux без сетевого трафика.
func detectNetLinkFacts() netLinkFacts {
	facts := netLinkFacts{Source: "detected"}

	ifaces, err := net.Interfaces()
	if err != nil {
		facts.Note = "интерфейсы недоступны: " + err.Error()
		return facts
	}

	var chosen *net.Interface
	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		if ifc.MTU > 0 && ifc.MTU < 1500 {
			continue // что-то вроде VPN/туннеля — не наш путь
		}
		chosen = ifc
		if ifc.MTU >= 9000 {
			break // jumbo: самый вероятный путь для раскладки
		}
	}
	if chosen == nil {
		facts.Note = "нет подходящего интерфейса (только loopback)"
		return facts
	}

	facts.Interface = chosen.Name
	facts.MTU = chosen.MTU
	facts.IsLoopback = chosen.Flags&net.FlagLoopback != 0
	if chosen.MTU >= 9000 {
		facts.Note = "MTU ≥ 9000: путь поддерживает jumbo-кадры"
	} else if chosen.MTU > 0 {
		facts.Note = "MTU " + strconv.Itoa(chosen.MTU) + ": jumbo не включён — возможна фрагментация на больших пакетах"
	}

	// Скорость из sysfs (Мбит/с). В контейнере на veth обычно отсутствует —
	// это нормально и должно быть видно как «не сообщено», а не как 0.
	raw, err := os.ReadFile("/sys/class/net/" + chosen.Name + "/speed")
	if err == nil {
		if n, ok := parsePositiveInt(strings.TrimSpace(string(raw))); ok {
			facts.SpeedMbps = n
		}
	} else {
		facts.Note = strings.TrimSpace(facts.Note + " | скорость линка не сообщается (/sys недоступен в контейнере)")
	}
	return facts
}

// parsePositiveInt — маленький парсер без strconv-ошибок на месте вызова.
func parsePositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
