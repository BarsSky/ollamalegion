// log_buffer_r390.go — R390 (2026-11-02): размер ring-buffer'а логов балансера.
//
// ЗАЧЕМ НАСТРОЙКА. Буфер был жёстко 100 записей: этого хватает для «последних
// строк в панели», но бесполезно для выгрузки файла «пришлите логи» — 100 строк
// это несколько секунд работы стенда. Дефолт поднят до 2000, а значение можно
// задать через LB_LOG_BUFFER_ENTRIES (0/мусор = дефолт).
package main

import (
	"os"
	"strconv"
	"strings"
)

const (
	// logBufferDefaultR390 — сколько записей держим по умолчанию (~2000 строк
	// по 200-300 байт = меньше мегабайта памяти).
	logBufferDefaultR390 = 2000
	// logBufferMaxR390 — верхняя граница, чтобы опечатка в env не съела память.
	logBufferMaxR390 = 20000
)

// logBufferEntriesR390 — действующий размер буфера логов.
func logBufferEntriesR390() int {
	raw := strings.TrimSpace(os.Getenv("LB_LOG_BUFFER_ENTRIES"))
	if raw == "" {
		return logBufferDefaultR390
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return logBufferDefaultR390
	}
	if n > logBufferMaxR390 {
		n = logBufferMaxR390
	}
	return n
}
