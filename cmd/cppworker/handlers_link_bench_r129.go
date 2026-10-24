// handlers_link_bench_r129.go — R129 (2026-10-16, пункт 3 разбора стенда).
//
//	POST /api/v1/cppworker/link-bench — ПРИЁМНИК замера: читает тело и отбрасывает
//	     его, отвечает {"bytes":N,"durationMs":M,"mbps":X}.
//	GET  /api/v1/cppworker/link-bench — что это и как этим пользоваться (без замера).
//
// ЗАЧЕМ ПРИЁМНИК В ВОРКЕРЕ. Пропускную способность между машинами нельзя измерить
// на порту rpc-server (у него свой бинарный протокол и нет эхо), поэтому замер
// идёт по HTTP-поверхности cppworker: это ТОТ ЖЕ физический путь, по которому
// пойдёт трафик раскладки.
//
// ПОЧЕМУ НЕ РУЧКА ЗДОРОВЬЯ. Это не health-check: он грузит сеть на десятки
// мегабайт и запускается только по явному запросу оператора/мастера, а не
// поллером. Тот же принцип, что у rpc-peers: дорогую проверку не подмешиваем в
// фоновые чтения.
package main

import (
	"io"
	"net/http"
	"time"
)

// linkBenchMaxMB — верхняя граница одного замера: защита от того, что кто-то
// попросит «залей 10 ГБ». 512 МБ хватает, чтобы измерить гигабит (≈112 МБ/с).
const linkBenchMaxMB = 512

func handleCppWorkerLinkBench(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"usage": "POST тело произвольного размера; воркер читает и отбрасывает его, " +
				"возвращая bytes/durationMs/mbps",
			"maxMb": linkBenchMaxMB,
			"note": "измеряется пропускная способность TCP между узлами; " +
				"это не health-check и не замер протокола llama.cpp RPC",
		})
		return
	case http.MethodPost:
		// читаем и отбрасываем
	default:
		writeError(w, http.StatusMethodNotAllowed, "use POST (замер) or GET (описание)")
		return
	}

	started := time.Now()
	n, err := io.Copy(io.Discard, r.Body)
	elapsed := time.Since(started)
	if err != nil {
		writeError(w, http.StatusBadRequest, "тело не прочитано: "+err.Error())
		return
	}
	// Нулевой интервал возможен: на локальном loopback чтение мегабайтов из
	// памяти быстрее разрешения таймера, и деление на ноль дало бы мусор вместо
	// скорости. Берём 1 нс как нижнюю границу разрешения — число остаётся
	// конечным и честно помечено как «быстрее таймера».
	if elapsed <= 0 {
		elapsed = time.Nanosecond
	}
	mbps := float64(n) / elapsed.Seconds() / (1 << 20)
	mbps = float64(int(mbps*10+0.5)) / 10
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bytes":      n,
		"durationMs": elapsed.Milliseconds(),
		"mbps":       mbps,
	})
}
