// feasible_memory_r521.go — R521 (2026-11-19, §8.3.1 плана): потолок окна по
// ПАМЯТИ, а не «половина свободной VRAM».
//
// ЧТО БЫЛО. feasible.go считал `maxVRAMCtx = (freeVRAM/2) / kvPerToken`. Половина
// свободной VRAM не вычитала ни веса модели, ни compute buffer, ни CUDA-контекст,
// поэтому на A10 24 ГБ для 27B Q4_K_M (16.46 ГБ весов) расчёт разрешал окно
// 131072. После загрузки осталось 2508 МБ, и первый же llama_decode на
// 80k-промпте падал с CUDA OOM — «код 1 без причины» из архива 2026-11-18.
//
// ЧТО СТАЛО. Из свободной VRAM вычитаются:
//   - веса модели целиком (расчёт отвечает на вопрос «влезет ли ВСЁ на GPU»);
//   - compute buffer — буферы графа и логитов; зависит от n_batch и n_vocab,
//     для больших моделей это 1.5–2 ГБ (в архиве при 2508 МБ свободных декод
//     всё равно упал, то есть 2.5 ГБ — ещё не запас);
//   - запас на фрагментацию VRAM и CUDA-контекст (доля от весов, но не больше
//     512 МБ: у маленьких моделей контекст не растёт пропорционально весам).
//
// Если после вычета не осталось ничего, результат 0 — честное «не влезает», а не
// придуманное число. Вызывающий (объявление окна, гейт загрузки) обязан трактовать
// 0 как «места нет», а не как «неизвестно» — для этого у воркера есть отдельные
// поля known/vram_known.
package cppbackend

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// computeBufferFloorMB — минимальный резерв на compute buffer.
//
// Число из живого факта, а не из головы: после загрузки 27B на A10 осталось
// 2508 МБ, и декод 80k-промпта упал. То есть одного KV-кэша (он уже посчитан в
// limit) и 2.5 ГБ свободных не хватило — буферам графа нужно больше. 1536 МБ —
// нижняя граница для моделей до 12 ГБ весов, 2048 МБ — для тяжёлых.
const computeBufferFloorMB = 1536

// computeBufferHeavyMB — резерв для моделей ≥ 12 ГБ весов: n_vocab и граф у них
// больше, и на 27B живой отказ случился именно с ~2.5 ГБ свободных.
const computeBufferHeavyMB = 2048

// computeBufferHeavyWeightsMB — с какого размера весов включается тяжёлый резерв.
const computeBufferHeavyWeightsMB = 12 * 1024

// fragmentationReserveMB — запас на фрагментацию VRAM и CUDA-контекст.
// Доля от весов (5 %), но не больше 512 МБ: у 4B-модели CUDA-контекст тот же,
// что и у 27B, поэтому процент от весов дал бы несправедливо мало.
func fragmentationReserveMB(weightsMB uint64) uint64 {
	share := weightsMB / 20
	if share > 512 {
		return 512
	}
	if share == 0 {
		return 64
	}
	return share
}

// computeBufferReserveMB — резерв на compute buffer (с учётом env-override).
//
// CPPWORKER_COMPUTE_BUFFER_MB позволяет оператору поднять/опустить резерв, не
// пересобирая образ: на разных архитектурах (MoE против dense) буферы различаются.
func computeBufferReserveMB(weightsMB uint64) uint64 {
	if v := envUint64MB("CPPWORKER_COMPUTE_BUFFER_MB"); v > 0 {
		return v
	}
	if weightsMB >= computeBufferHeavyWeightsMB {
		return computeBufferHeavyMB
	}
	return computeBufferFloorMB
}

// envUint64MB — прочитать положительное число МБ из переменной окружения.
func envUint64MB(name string) uint64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return uint64(v)
}

// maxVRAMCtxFromMemory — чистая формула потолка окна (R521).
//
// freeMB — свободная VRAM СЕЙЧАС, weightsMB — размер весов модели (0 = неизвестен),
// reserveMB — compute buffer, kvPerToken — байт KV на токен, ggufMax — потолок
// модели (0 = неизвестен).
//
// При неизвестном размере весов считаем консервативно: вычитаем только резерв,
// как раньше, НО без множителя «половина» — это уже не «половина на буферы», а
// конкретный резерв. Такой расчёт всё равно завышен для неизвестной модели, и
// вызывающий обязан это знать (Source=gguf без loaded_info).
func maxVRAMCtxFromMemory(freeMB, weightsMB, reserveMB uint64, kvPerToken, ggufMax int) int {
	if kvPerToken <= 0 {
		return 0
	}
	need := reserveMB + fragmentationReserveMB(weightsMB)
	if freeMB <= weightsMB+need {
		// Веса и буферы не влезают даже без KV: места нет вовсе.
		return 0
	}
	availMB := freeMB - weightsMB - need
	ctx := int(availMB*1024*1024) / kvPerToken
	if ctx < 0 {
		return 0
	}
	if ggufMax > 0 && ctx > ggufMax {
		ctx = ggufMax
	}
	return ctx
}

// weightsSizeMB — размер весов модели в МБ (R521).
//
// Источники по порядку: ModelInfo загруженной модели (SizeBytes), путь из
// ModelManager + stat файла. 0 = неизвестен (тогда формула вычитает только
// резерв и обязана считаться консервативной).
func (b *Backend) weightsSizeMB(modelName string) uint64 {
	if b == nil {
		return 0
	}
	if mi, err := b.GetModel(modelName); err == nil && mi != nil && mi.SizeBytes > 0 {
		return uint64(mi.SizeBytes) / (1024 * 1024)
	}
	if mm := b.ModelManager(); mm != nil {
		if path := mm.ResolveModelPath(modelName); path != "" {
			if st, err := os.Stat(path); err == nil && st.Size() > 0 {
				return uint64(st.Size()) / (1024 * 1024)
			}
		}
	}
	return 0
}

// unusedBridgeImportGuard — bridge импортируется для Reset-функции ниже; отдельный
// символ нужен, чтобы файл не потерял зависимость при рефакторинге.
var _ = bridge.RecentLogLines

// weightsPathBase — отделяет каталог от имени (используется тестами формулы).
func weightsPathBase(path string) string { return filepath.Base(path) }
