package memfit

import (
	"os"
	"strconv"
	"strings"
)

// Budget — снимок ресурсов на момент решения. Ключевое отличие от прежнего кода:
// у каждого поля есть флаг Known, и «неизвестно» нельзя случайно принять за ноль
// (именно так гейт n_ctx становился fail-open, когда лимиты приезжали нулями).
type Budget struct {
	// VRAM (заполняет вызывающий из C-bridge/NVML).
	VRAMFree  Bytes
	VRAMTotal Bytes
	VRAMKnown bool

	// Системная память (обычно из ProbeRAM).
	RAMAvail Bytes
	RAMTotal Bytes
	RAMKnown bool

	// RAMLimit — лимит cgroup, если он есть. MemAvailable внутри контейнера
	// показывает память ВСЕЙ VM, поэтому при заданном mem_limit нужен минимум из двух.
	RAMLimit      Bytes
	RAMLimitKnown bool

	// Резервы: VRAM под CUDA-контекст/буферы, RAM под систему и runtime.
	VRAMReserve Bytes
	RAMReserve  Bytes

	// Source — человекочитаемое происхождение чисел (для лога и диагностики).
	Source string
}

// Policy — одна явная политика вместо разбросанных по коду коэффициентов.
type Policy struct {
	// VRAMUtil — какую долю свободной VRAM (за вычетом резерва) разрешено занять.
	// 1.0 = весь резерв уже учтён в VRAMReserve.
	VRAMUtil float64
	// RAMUtil — ДОПОЛНИТЕЛЬНАЯ доля доступной RAM поверх резерва. По умолчанию 1.0:
	// единственная скидка — RAMReserve.
	//
	// ПОЧЕМУ НЕ 0.8 ПО УМОЛЧАНИЮ (R83, шаг 3). Сначала здесь стояло 0.8, и вместе с
	// резервом 4 GiB это давало двойную скидку: на стенде (21 GiB свободно) бюджет
	// падал до (21 − 4) × 0.8 = 13.6 GiB, и модель 15.7 GiB отказывалась грузиться
	// вовсе — хотя физически она грузится и работает (проверено: 32768 с q4_0
	// обслуживается, RSS 13.97 GiB). Те 0.8 выбирались по числам, посчитанным на
	// ЗАВЫШЕННОМ KV (до исправления KV-слоёв, ×3.38), поэтому двойной запас стал
	// избыточным. RAMUtil остаётся рычагом для тех, кто хочет больше запаса.
	RAMUtil float64
	// UnknownIsFatal: если данные о ресурсах неизвестны, считать это отказом
	// (fail-closed) вместо «пропустить проверку». Прежний код был fail-open.
	UnknownIsFatal bool
}

// DefaultPolicy — резерв на систему и никаких дополнительных долей: одна скидка
// на каждый ресурс (VRAM − 2 GiB, RAM − 4 GiB), как в текущем продовом коде.
func DefaultPolicy() Policy {
	return Policy{VRAMUtil: 1.0, RAMUtil: 1.0, UnknownIsFatal: false}
}

func permille(f float64) int64 {
	switch {
	case f <= 0:
		return 0
	case f >= 1:
		return 1000
	default:
		return int64(f*1000 + 0.5)
	}
}

// UsableVRAM — сколько VRAM реально можно отдать модели.
func (b Budget) UsableVRAM(p Policy) Bytes {
	if !b.VRAMKnown {
		return 0
	}
	return b.VRAMFree.Sub(b.VRAMReserve).Scale(permille(p.VRAMUtil), 1000)
}

// UsableRAM — сколько RAM реально можно отдать модели: доступная память, но не
// больше лимита cgroup, минус системный резерв, умноженная на политику.
//
// Почему НЕ MemTotal: на живом стенде сравнение с установленной памятью
// (24 576 MiB при фактически свободных 20 480) одобрило загрузку 27B с запасом
// 105 MiB. Почему НЕ только MemAvailable: при заданном mem_limit контейнера
// MemAvailable (память VM) завышает доступное.
func (b Budget) UsableRAM(p Policy) Bytes {
	if !b.RAMKnown {
		return 0
	}
	eff := b.RAMAvail
	if b.RAMLimitKnown && b.RAMLimit > 0 && b.RAMLimit < eff {
		eff = b.RAMLimit
	}
	return eff.Sub(b.RAMReserve).Scale(permille(p.RAMUtil), 1000)
}

// RAMProbe — результат чтения системной памяти.
type RAMProbe struct {
	Available  Bytes
	Total      Bytes
	Limit      Bytes // cgroup: сколько ещё можно занять (limit − current)
	LimitKnown bool
	Known      bool
	Source     string
}

// ProbeRAM читает системную память РОВНО один раз и в одном месте.
//
// Приоритет:
//  1. CPPWORKER_AVAILABLE_RAM_BYTES — override (тесты, CI, нестандартные среды).
//  2. /proc/meminfo → MemAvailable (fallback MemFree + Cached).
//  3. cgroup v2 (/sys/fs/cgroup/memory.max) или v1 (memory/memory.limit_in_bytes),
//     из лимита вычитается текущее потребление (memory.current / usage_in_bytes).
//
// Возвращает Known=false, если ничего не прочитано — вызывающий обязан решить
// политикой, что делать с неопределённостью, а не молча считать ноль.
func ProbeRAM() RAMProbe {
	pr := RAMProbe{}

	// 1. ENV override.
	if v := strings.TrimSpace(os.Getenv("CPPWORKER_AVAILABLE_RAM_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			pr.Available = Bytes(n)
			pr.Known = true
			pr.Source = "env:CPPWORKER_AVAILABLE_RAM_BYTES"
		}
	}

	// 2. /proc/meminfo.
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var memFree, cached Bytes
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
					pr.Total = Bytes(kb) * KiB
				}
			case "MemAvailable:":
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 && !pr.Known {
					pr.Available = Bytes(kb) * KiB
					pr.Known = true
					pr.Source = "meminfo:MemAvailable"
				}
			case "MemFree:":
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
					memFree = Bytes(kb) * KiB
				}
			case "Cached:":
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
					cached = Bytes(kb) * KiB
				}
			}
		}
		if !pr.Known && memFree > 0 {
			pr.Available = memFree + cached
			pr.Known = true
			pr.Source = "meminfo:MemFree+Cached"
		}
	}

	// 3. cgroup. Значения «max» и гигантские сентинелы означают отсутствие лимита.
	if limit, cur, ok := readCgroupRAM(); ok {
		if limit > cur {
			pr.Limit = limit - cur
			pr.LimitKnown = true
		}
		if pr.Source == "" {
			pr.Source = "cgroup"
		} else {
			pr.Source += "+cgroup"
		}
	}

	return pr
}

// unlimitedCgroup — сентинел «лимита нет» в v1 (PAGE_COUNTER_MAX ≈ 2^63 − 4096).
const unlimitedCgroup = Bytes(1) << 60

func readCgroupRAM() (limit, current Bytes, ok bool) {
	// cgroup v2.
	if v, found := readUintFile("/sys/fs/cgroup/memory.max"); found {
		if v == 0 || v >= int64(unlimitedCgroup) {
			return 0, 0, false // "max" парсится как 0 → лимита нет
		}
		cur, _ := readUintFile("/sys/fs/cgroup/memory.current")
		return Bytes(v), Bytes(cur), true
	}
	// cgroup v1.
	if v, found := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); found {
		if v <= 0 || v >= int64(unlimitedCgroup) {
			return 0, 0, false
		}
		cur, _ := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		return Bytes(v), Bytes(cur), true
	}
	return 0, 0, false
}

// readUintFile — читает число из файла. "max" (cgroup v2 без лимита) → (0, true).
func readUintFile(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, false
	}
	if s == "max" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
