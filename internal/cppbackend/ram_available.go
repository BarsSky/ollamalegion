// ram_available.go — R83 (живая проверка 2026-09-25): сколько RAM реально можно
// отдать под модель.
//
// ЗАЧЕМ. Решение о RAM-fallback (CalculateOptimalGPULayers) сравнивало
// потребность CPU-части модели с УСТАНОВЛЕННОЙ памятью (MemTotal), а не со
// свободной, и печатало её в лог под именем ramAvailableMB. На живом стенде
// (RTX 3070 8 GB, контейнер с лимитом WSL2 24.5 GB) это дало:
//
//	optimal GPU layers found with RAM fallback:
//	  cpuMemoryMB:19556  ramAvailableMB:24576  <- это MemTotal
//	  kvCacheMB:8565  useMmap:false  kvCPUFraction:1
//
// при фактически свободных 20480 MB. Проверка «19556 < 24576*0.8 = 19660»
// прошла с запасом 105 MB, после чего модель 16.5 GB читалась в память до
// 13.97 GiB resident и грузилась минутами при CPU 0.13 % (упор в I/O) — то есть
// формально «загружено», практически непригодно. По свободной памяти проверка
// обязана была отказать и вернуть диагностику с числами.
//
// ПОЧЕМУ НЕ MemTotal. MemTotal не меняется, пока машина занята: контейнеры
// видят память друг друга (MemAvailable общий для ядра), а фиксированный
// «резерв 4 GB» в CalculateResourceLimits этого не отражает.
//
// ЧЕГО ЗДЕСЬ НЕТ. Лимит cgroup (memory.max) не читается: MemAvailable уже
// учитывает реальную занятость всего ядра/VM, а per-container mem_limit в этом
// стеке не задан. Если появится mem_limit, сюда нужно добавить
// min(memory.max - memory.current, MemAvailable).
package cppbackend

import (
	"os"
	"strconv"
	"strings"
)

// availableRAMForLoadMB возвращает RAM (в MB), которую реально можно занять
// моделью, и источник значения для лога.
//
// Приоритет:
//  1. CPPWORKER_AVAILABLE_RAM_BYTES — override (тесты, CI, нестандартные
//     окружения). Значение в БАЙТАХ, как у одноимённого хука в cmd/cppworker
//     (там его читает availableRAMBytes для AutoTuneNCtx) — один операторский
//     рычаг на оба места.
//  2. /proc/meminfo → MemAvailable (Linux) — реально свободная память.
//  3. fallbackMB — прежнее поведение вызывающего (MemTotal на Linux, 8192 на
//     Windows), чтобы не менять поведение там, где /proc/meminfo недоступен.
//
// ОТЛИЧИЕ ОТ cmd/cppworker.availableRAMBytes: там некорректный env даёт 0
// («достоверно неизвестно» → RAM-проверка пропускается). Здесь 0 означал бы
// «памяти нет» и приводил бы к отказу всех загрузок, поэтому некорректное
// значение мы игнорируем и падаем в автоопределение — то есть ведём себя как
// «хук не задан».
func availableRAMForLoadMB(fallbackMB uint64) (uint64, string) {
	if v := strings.TrimSpace(os.Getenv("CPPWORKER_AVAILABLE_RAM_BYTES")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			return n / 1024 / 1024, "env"
		}
	}

	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "MemAvailable:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil && kb > 0 {
				return kb / 1024, "meminfo"
			}
			break
		}
	}

	return fallbackMB, "fallback"
}
