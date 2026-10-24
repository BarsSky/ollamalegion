package main

import (
	"net/http"
	"strconv"
	"strings"
)

// handlers_link_probe.go — R118.15 (2026-10-14): замер пропускной способности
// канала до узла раскладки СРЕДСТВАМИ ПРОДУКТА.
//
// ЗАЧЕМ. До этой правки скорость канала до донора показывалась из значения
// оператора в .env (LB_RPC_LINK_MBPS), которое он получал скриптом
// scripts/measure-network.ps1 вручную. Значение устаревало молча: сеть менялась,
// а в панели оставалась прежняя цифра — и по ней оценивался бюджет раскладки.
//
// ЧТО ДЕЛАЕТ ЭТОТ ОБРАБОТЧИК. Отдаёт N байт «несжимаемого» потока по HTTP. Сам он
// ничего не измеряет: измеряет вызывающий (балансер), засекая время скачивания
// известного объёма до узла. Замер идёт по ТОЙ ЖЕ сети и до ТОГО ЖЕ хоста, где
// живёт rpc-server раскладки (сам rpc-server говорит по бинарному протоколу
// ggml-rpc, HTTP-замеру он недоступен; машина и канал те же).
//
//	GET /api/rpc-link-probe?bytes=8388608
//
// Ограничения: минимум 64 КиБ, максимум 64 МиБ — замер не должен сам превращаться
// в нагрузку на канал. Ответ: application/octet-stream + Content-Length, поток
// отдаётся чанками с flush, чтобы клиент мерил передачу, а не буферизацию.
func handleLinkProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"error":"GET only"}`))
		return
	}

	const (
		defaultBytes = 8 << 20
		minBytes     = 64 << 10
		maxBytes     = 64 << 20
	)
	n := defaultBytes
	if v := strings.TrimSpace(r.URL.Query().Get("bytes")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bytes must be an integer"}`))
			return
		}
		n = parsed
	}
	if n < minBytes {
		n = minBytes
	}
	if n > maxBytes {
		n = maxBytes
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(n))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	// Буфер заполняем «шумом»: сжимаемый нулевой поток промежуточное оборудование
	// могло бы сжать, и замер показал бы завышенную скорость.
	const chunkSize = 64 << 10
	buf := make([]byte, chunkSize)
	seed := uint32(0x9e3779b9)
	for i := range buf {
		seed = seed*1664525 + 1013904223
		buf[i] = byte(seed >> 24)
	}

	flusher, _ := w.(http.Flusher)
	for n > 0 {
		chunk := chunkSize
		if n < chunk {
			chunk = n
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return
		}
		n -= chunk
		if flusher != nil {
			flusher.Flush()
		}
	}
}
