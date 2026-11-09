// log_buffer_r390.go — R390 (2026-11-02): размер ring-buffer'а логов балансера.
//
// ЗАЧЕМ НАСТРОЙКА. Буфер был жёстко 100 записей: этого хватает для «последних
// строк в панели», но бесполезно для выгрузки файла «пришлите логи» — 100 строк
// это несколько секунд работы стенда. Дефолт поднят до 2000, а значение можно
// задать через LB_LOG_BUFFER_ENTRIES (0/мусор = дефолт).
//
// R476 (2026-11-07): у переменной появился псевдоним LB_DIAG_RING — им оператор
// называл размер буфера, когда просил выгрузку логов. Два имени на одну
// настройку — это риск «задал одно, работает другое», поэтому правило жёсткое:
// LB_LOG_BUFFER_ENTRIES главнее, LB_DIAG_RING читается только если первая не
// задана. Обе пишутся в диагностическую сводку, так что видно, что применилось.
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
	names := []string{"LB_LOG_BUFFER_ENTRIES", "LB_DIAG_RING"}
	for _, name := range names {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			// Задано мусором — следующий по приоритету, а не дефолт: иначе
			// опечатка в главной переменной молча отменяла бы псевдоним.
			continue
		}
		if n > logBufferMaxR390 {
			n = logBufferMaxR390
		}
		return n
	}
	return logBufferDefaultR390
}
