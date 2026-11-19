// kv_budget.h — R590 (2026-11-19): размер KV-cache на токен и потолок n_ctx по
// свободной VRAM — чистая арифметика, вынесенная из bridge.c.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. В bridge.c этот расчёт жил внутри ветки
// `#ifdef GGML_USE_CUDA` рядом с cuMemGetInfo/llama_model_*, поэтому проверялся
// ТОЛЬКО на живой карте. Проверять его надо на числах: ровно здесь поселилась
// регрессия (см. ниже), которую заметили по живому логу, а не по тесту — потому
// что теста на формулу не было вообще.
//
// ЧТО БЫЛО СЛОМАНО (R590). Для f16 (дефолтный тип KV, когда оператор не задал
// kvCacheType) формула делилабайты на блок из 32 ЭЛЕМЕНТОВ, но считала, что
// «32» — это байты НА ЭЛЕМЕНТ:
//
//     unsigned long long type_size = 2;            // f16 default — это значение
//     switch (ctx_params.type_k) {                // НИКОГДА не использовалось
//         case GGML_TYPE_Q8_0: type_size = 34; break;
//         case GGML_TYPE_Q4_0: type_size = 18; break;
//         default:             type_size = 32; break;  // «f16: 32 * 2B»
//     }
//     unsigned long long elems_per_block = (type_size == 32) ? 1 : 32;
//     row_bytes = n_kv_heads * kv_head_dim * type_size / elems_per_block;
//
// При f16 получалось row_bytes = heads * head_dim * 32 вместо * 2, то есть
// ровно в 16 раз больше. Живое подтверждение (2026-11-19, RTX 3070, Qwen3-4B,
// KV f16): мост печатал `kv_per_token=2359296 bytes`, а llama.cpp на той же
// загрузке выделял `CUDA0 KV buffer size = 4096.00 MiB` + `CPU KV buffer = 512`
// на 32768 ячеек и 36 слоёв, то есть 4608 MiB / 32768 = 147 456 B/токен —
// в 16.0 раза меньше. Следствие: max_vram_n_ctx для f16-моделей занижался в
// 16 раз, и балансер видел «физический потолок» в 16 раз меньше настоящего.
//
// КАК СЧИТАЕМ ТЕПЕРЬ. Тип KV задаётся парой (байт в блоке, элементов в блоке):
//
//     f16  — 2 байта на 1 элемент          (2/1)
//     q8_0 — 34 байта на 32 элемента       (34/32)
//     q4_0 — 18 байт на 32 элемента        (18/32)
//
// Это те же дроби, что в Go (`internal/cppbackend/kv_cache_type.go`,
// kvCacheBytesPerElem) — одно число, один источник, по обе стороны моста.
//
// K и V считаются раздельно: у llama.cpp это два независимых тензора и типы у
// них могут отличаться (type_k != type_v), а прежний код молча применял тип K к
// обоим.
#ifndef BRIDGE_KV_BUDGET_H
#define BRIDGE_KV_BUDGET_H

#ifdef __cplusplus
extern "C" {
#endif

// kv_budget_type — тип квантования KV-cache. Неизвестный (в т.ч. f32) считается
// как f16: занижать требования к памяти опаснее, чем завысить.
typedef enum {
    KV_BUDGET_TYPE_F16  = 0,
    KV_BUDGET_TYPE_Q8_0 = 1,
    KV_BUDGET_TYPE_Q4_0 = 2
} kv_budget_type;

// kv_budget_shape — измеренные у модели размерности, от которых зависит KV.
typedef struct {
    int n_kv_heads;  // llama_model_n_head_kv
    int kv_head_dim; // attention.key_length (НЕ n_embd/n_heads, если planner дал точнее)
    int kv_layers;   // число слоёв, реально хранящих KV ( рекуррентные/SSM не хранят)
} kv_budget_shape;

// kv_budget_row_bytes — байты на один токен в ОДНОМ тензоре (K или V) для одного
// слоя: n_kv_heads * kv_head_dim элементов, упакованных по типу.
// Возвращает 0 при вырожденных входах (вызывающая сторона трактует как
// «оценить нельзя», а не как «нужно 0 байт»).
unsigned long long kv_budget_row_bytes(int n_kv_heads, int kv_head_dim, kv_budget_type type);

// kv_budget_per_token — байты KV на один токен для ВСЕХ слоёв, K + V.
// 0 = посчитать нельзя (нет размерностей).
long long kv_budget_per_token(const kv_budget_shape *shape,
                              kv_budget_type type_k,
                              kv_budget_type type_v);

// kv_budget_max_n_ctx — сколько токенов контекста помещается в free_bytes при
// заданном kv_per_token. 0 = не помещается ни один токен (или данных нет).
long long kv_budget_max_n_ctx(long long free_bytes, long long kv_per_token);

// kv_budget_type_name — имя типа для лога моста («f16»/«q8_0»/«q4_0»).
// Нужно, чтобы в логе загрузки было видно, КАКОЙ тип применился: именно этой
// пары полей не хватало, когда расчёт разошёлся с llama.cpp в 16 раз.
const char *kv_budget_type_name(kv_budget_type type);

#ifdef __cplusplus
}
#endif

#endif // BRIDGE_KV_BUDGET_H
