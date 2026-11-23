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

// maxVRAMCtxFromMemory — чистая формула потолка окна (R521) для ПЛОСКОГО KV.
//
// R707: производственный путь послойного расчёта — maxVRAMCtxForPlanR707; эта
// функция осталась для вырожденного случая «о слоях нет ничего» (тогда KV на
// токен — defensive-константа, и считается ровно как до R707) и как эталон
// формулы в тестах R521.
//
// freeMB — свободная VRAM СЕЙЧАС, weightsMB — размер весов модели (0 = неизвестен),
// reserveMB — compute buffer, kvPerToken — байт KV на токен, ggufMax — потолок
// модели (0 = неизвестен).
func maxVRAMCtxFromMemory(freeMB, weightsMB, reserveMB uint64, kvPerToken, ggufMax int) int {
	return maxVRAMCtxFromFlatKVPerTokenR707(freeMB, weightsMB, reserveMB, kvPerToken, ggufMax)
}

// maxVRAMCtxForPlanR707 — потолок окна по свободной VRAM для ПОСЛОЙНОЙ раскладки.
//
// Отличие от maxVRAMCtxFromMemory: бюджет KV делится не на «KV на токен всех
// слоёв», а на прирост с учётом окна (KVShape.MaxCtxForBudget): после окна растут
// только глобальные слои. Для gemma-4-E4B это разница между ~7k и ~80k окна на
// 8-гигабайтной карте при одинаковом бюджете.
func maxVRAMCtxForPlanR707(freeMB, weightsMB, reserveMB uint64, plan KVPlan, kvType string, ggufMax int) int {
	availMB, ok := kvBudgetAfterWeightsMB(freeMB, weightsMB, reserveMB)
	if !ok {
		return 0
	}
	return plan.MaxCtxForBudget(kvType, int64(availMB)*1024*1024, ggufMax)
}

// maxVRAMCtxFromFlatKVPerTokenR707 — прежняя формула (плоский KV) для путей, у
// которых есть только число «байт на токен».
func maxVRAMCtxFromFlatKVPerTokenR707(freeMB, weightsMB, reserveMB uint64, kvPerToken, ggufMax int) int {
	if kvPerToken <= 0 {
		return 0
	}
	availMB, ok := kvBudgetAfterWeightsMB(freeMB, weightsMB, reserveMB)
	if !ok {
		return 0
	}
	ctx := int(availMB*1024*1024) / kvPerToken
	if ggufMax > 0 && ctx > ggufMax {
		ctx = ggufMax
	}
	return ctx
}

// kvBudgetAfterWeightsMB — сколько МБ остаётся под KV после весов, compute-буфера
// и запаса на фрагментацию. ok=false — места нет вовсе (веса и буферы не влезают).
func kvBudgetAfterWeightsMB(freeMB, weightsMB, reserveMB uint64) (uint64, bool) {
	need := reserveMB + fragmentationReserveMB(weightsMB)
	if freeMB <= weightsMB+need {
		// Веса и буферы не влезают даже без KV: места нет вовсе.
		return 0, false
	}
	return freeMB - weightsMB - need, true
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
