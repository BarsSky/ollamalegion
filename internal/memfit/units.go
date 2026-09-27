// Package memfit — единая точка правды для ответа на вопрос «поместится ли
// модель и с каким контекстом».
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ. Живая проверка (2026-09-25) показала, что оценки
// памяти были размазаны по пяти местам с разными допущениями, и они разошлись:
//
//   - CalculateResourceLimits: max_ram_n_ctx = (MemTotal − 4 GiB) / kvPerToken —
//     по УСТАНОВЛЕННОЙ памяти и без весов модели;
//   - CalculateOptimalGPULayers: сравнение CPU-части с MemTotal * 0.8 и KV-cache
//     там же считается отдельной формулой;
//   - calculateLazyLoadOpts: сравнение размера ФАЙЛА с MemAvailable по своему
//     ридеру и без отказа (только метка fallback_no_fit);
//   - EstimateGPUMemoryForModel: веса × 0.7 и KV-cache как 256 БАЙТ на токен при
//     комментарии «256 КБ» (в 1000 раз больше);
//   - C-bridge: своя оценка (VRAM-estimated max n_ctx), самая корректная.
//
// ИСТОРИЧЕСКАЯ СПРАВКА (R83 §9.4, 2026-09-27): перечисленные выше оценки живы уже
// не все — CalculateResourceLimits, CalculateOptimalGPULayers и
// EstimateGPUMemoryForModel удалены, их заменил этот пакет, а KV в оставшихся
// местах считается через memfit.KVBytesPerElement/KVBytesPerToken. Список
// оставлен как описание КЛАССА дефекта («одна величина — пять формул»), ради
// которого пакет и появился.
//
// Итог на реальном железе: при 8 GiB VRAM и 15.7 GiB весов система сообщала
// max_vram_n_ctx = 103389, гейт n_ctx работал fail-open для холодной модели, а
// загрузка одобрялась с запасом 105 MiB по MemTotal — модель грузилась 9 минут
// целиком на CPU.
//
// ПРИНЦИПЫ ЭТОГО ПАКЕТА (именно они исключают повторение класса ошибок):
//
//  1. ОДНА формула на каждый вопрос. KV-cache, веса, потолки и вердикт
//     считаются здесь, а вызывающие только передают данные и читают Verdict.
//  2. Единицы типизированы (Bytes). Нет неявных «МБ/МиБ/КБ/байт»: ошибка
//     масштаба в 1000×, из-за которой ctxSize*256/1024/1024 выдавал 8 МиБ, где
//     комментарий обещал 8 ГиБ, невозможна по типу, а не по внимательности.
//  3. «НЕИЗВЕСТНО» — отдельное состояние, а не ноль. Budget несёт Known-флаги,
//     Stage имеет explicit unknown; политика задаёт, считать ли неопределённость
//     фатальной (fail-closed) — вместо сегодняшнего неявного fail-open.
//  4. Вердикт структурирован (Stage + список Reason с машинными кодами), а текст
//     для пользователя СТРОИТСЯ из него. Один источник для HTTP-кода, лога,
//     /api/models, load_failure и уведомления в WebUI — расхождение «гейт сказал
//     одно, загрузка другое» становится невозможным по построению.
//  5. Функция Evaluate — чистая: никакого чтения окружения и никаких побочных
//     эффектов. Поэтому её можно проверять golden-тестами на реальных числах
//     железа (см. fit_test.go) — тестами, которые ловят именно те ошибки, что
//     жили годами: 0.7, 256 Б/токен, MemTotal вместо MemAvailable, KV без весов.
package memfit

import "fmt"

// Bytes — объём в байтах. Отдельный тип, чтобы «МБ», «МиБ» и «байты» нельзя было
// перепутать неявно: компилятор не даст сложить Bytes с int64 без явного намерения.
type Bytes int64

const (
	KiB Bytes = 1024
	MiB Bytes = 1024 * KiB
	GiB Bytes = 1024 * MiB
)

// MiBOf/GiBOf — конструкторы из чисел: MiBOf(15_701).
func MiBOf(n int64) Bytes { return Bytes(n) * MiB }
func GiBOf(n int64) Bytes { return Bytes(n) * GiB }

func (b Bytes) MiB() int64     { return int64(b) / int64(MiB) }
func (b Bytes) GiB() float64   { return float64(b) / float64(GiB) }
func (b Bytes) IsZero() bool   { return b == 0 }
func (b Bytes) Positive() bool { return b > 0 }

// Sub — вычитание с насыщением на нуле (объёмы не бывают отрицательными).
func (b Bytes) Sub(other Bytes) Bytes {
	if b <= other {
		return 0
	}
	return b - other
}

// Scale — пропорциональная доля: Scale(num, den) = b * num / den.
func (b Bytes) Scale(num, den int64) Bytes {
	if den == 0 || num <= 0 {
		return 0
	}
	return Bytes(int64(b) * num / den)
}

// Mul — умножение на число (например, kvPerToken × n_ctx).
func (b Bytes) Mul(n int64) Bytes { return Bytes(int64(b) * n) }

func (b Bytes) String() string {
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.2f GiB", b.GiB())
	case b >= MiB:
		return fmt.Sprintf("%d MiB", b.MiB())
	default:
		return fmt.Sprintf("%d B", int64(b))
	}
}
