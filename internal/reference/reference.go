// Package reference — серверное хранилище «справки/рекомендаций» и шаблонов
// параметров загрузки моделей (страница WebUI «Справка/рекомендации + шаблоны»).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ. Хранилище не знает ни про HTTP, ни про профили моделей
// в рантайме: оно умеет ровно две вещи — держать на диске документ
// `model-reference.json` и отдавать/принимать его записи под замком. Всё, что
// касается «применить к профилю», живёт в `internal/api` и вызывает
// существующие функции профилей. Из-за этого пакет не может импортировать
// `internal/api` и не заводит второго пути применения.
//
// Контракт описан в docs/plans/2026-11-17-reference-templates-design.md
// (§2 «Контракт хранения», §3 «Контракт API»). Решения человека, уточняющие
// документ, перечислены там же в §6.
//
// ГЛАВНОЕ ПРАВИЛО: файл — справочник и заготовки. Действующие значения живут в
// профилях моделей (`config.LlamaCppModelProfiles`). Значения из файла НИКОГДА
// не подставляются в профиль автоматически (§7 п.1 документа).
package reference

import "encoding/json"

// SchemaVersion — версия формата файла. Неизвестная версия → warn и работаем
// без данных (как profiles_persistence.go:86-90), а не падаем.
const SchemaVersion = 1

// FileName — имя файла рядом со state.json (тот же записываемый том /app/data).
const FileName = "model-reference.json"

// EnvPathOverride — env-переменная, переопределяющая путь целиком
// (по образцу LB_IMAGE_MODEL_PROFILES_PATH, internal/config/image_model_profiles.go:37-41).
const EnvPathOverride = "LB_MODEL_REFERENCE_PATH"

// Doc — файл целиком: `{version, recommendations, templates}` (§2.2).
type Doc struct {
	Version int `json:"version"`
	// Recommendations и Templates всегда непустые срезы после инициализации:
	// иначе GET отдал бы `null` вместо `[]`, и UI выглядел бы сломанным до
	// первой записи (§2.5).
	Recommendations []Recommendation `json:"recommendations"`
	Templates       []Template       `json:"templates"`
}

// AppliesTo — «к чему применима запись» (§2.3). Только для подбора/подсветки в
// UI, НИКОГДА не используется как разрешение на автоприменение.
//
// Решение человека №1 (docs §6.1): поле НЕОБЯЗАТЕЛЬНО.
//   - запись БЕЗ appliesTo — «общая справка» (значения с обоснованием, к чему угодно);
//   - запись С appliesTo — «для класса моделей/пер-модели»: glob по имени,
//     тип бэкенда, числовые границы VRAM.
//
// Одна схема, один вид записи — без флага «вид записи».
type AppliesTo struct {
	// ModelPattern — glob по имени модели (не regex, §7 п.7).
	ModelPattern string `json:"modelPattern,omitempty"`
	// BackendType — тип бэкенда ("llama_cpp" и т.п.).
	BackendType string `json:"backendType,omitempty"`
	// Quantization — метка квантизации (Q4_K_M, …), только для подбора.
	Quantization string `json:"quantization,omitempty"`
	// VRAMMinMB/VRAMMaxMB — числовые границы VRAM; 0 = граница не задана.
	VRAMMinMB int `json:"vramMinMB,omitempty"`
	VRAMMaxMB int `json:"vramMaxMB,omitempty"`
}

// Source — ссылка на документ-первоисточник. Это ССЫЛКА, а не копия дока.
type Source struct {
	// Doc — путь от корня репозитория.
	Doc string `json:"doc"`
	// Section — раздел внутри документа (§3.2, §1 и т.п.).
	Section string `json:"section,omitempty"`
}

// Param — одна пара «имя поля профиля → значение» с человеческими подписями.
//
// Решение человека №5 (docs §6.5): Value ВСЕГДА в канонических единицах поля
// профиля (токены для contextLength/contextPerSeq, слои для numGpuLayers,
// байты для sizeBytes). Unit — НЕОБЯЗАТЕЛЬНАЯ подпись для человека; в
// вычислениях, сравнении и diff она НЕ участвует (см. preview: diff строится
// только по Name/Value).
type Param struct {
	// Name — ТОЛЬКО имя поля types.LlamaCppModelProfile (проверяется
	// reflection'ом по json-тегам типа, второго списка имён нет).
	Name string `json:"name"`
	// Value — JSON-примитив. Тип выводится из типа поля профиля при декодировании.
	Value json.RawMessage `json:"value"`
	// Unit — подпись для человека ("tokens", "layers"). Не участвует в расчётах.
	Unit string `json:"unit,omitempty"`
	// Comment — пояснение к одному параметру (опционально).
	Comment string `json:"comment,omitempty"`
}

// Recommendation — запись-рекомендация (§2.3).
type Recommendation struct {
	// ID — slug ^[a-z0-9][a-z0-9._-]{2,63}$, задаётся при создании и НЕ меняется.
	ID string `json:"id"`
	// Title — человеческий заголовок записи.
	Title string `json:"title"`
	// AppliesTo — опционально (решение человека №1).
	AppliesTo *AppliesTo `json:"appliesTo,omitempty"`
	// Params — значения параметров; имена проверяются по полям профиля.
	Params []Param `json:"params"`
	// Comment — обоснование записи целиком.
	Comment string `json:"comment,omitempty"`
	// Sources — откуда взято (ссылки на доки).
	Sources []Source `json:"sources,omitempty"`
	// Author — кто создал ("operator", "docs-import").
	Author string `json:"author,omitempty"`
	// CreatedAt/UpdatedAt — RFC3339 UTC. Ставит сервер.
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Revision — оптимистичная блокировка: сервер растит её при каждом
	// успешном PUT; в теле PUT несовпавшая ревизия → 409 revision_conflict.
	Revision int `json:"revision"`
}

// Template — именованный набор параметров (§2.4).
//
// Отличие от Recommendation намеренное: Params — ПЛОСКИЙ объект в форме тела
// `PUT /api/v1/cppworker/model-profiles/{name}`, потому что применяется он
// именно этим телом (мерж + validateModelProfile + DisallowUnknownFields).
// Второй структуры «набор параметров» не заводим.
type Template struct {
	ID string `json:"id"`
	// Name — человеческое имя набора.
	Name string `json:"name"`
	// Version — версия содержимого для человека (идёт в лог/ответ применения).
	Version int `json:"version"`
	// Params — `{"contextLength":32768,"kvCacheType":"q8_0",...}`.
	Params json.RawMessage `json:"params"`
	// Comment — когда набор годится и чем ограничен.
	Comment string `json:"comment,omitempty"`
	// BasedOn — id рекомендации, из которой шаблон вырос (может отсутствовать).
	BasedOn string `json:"basedOn,omitempty"`
	// Author/CreatedAt/UpdatedAt/Revision — как у Recommendation.
	Author    string `json:"author,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Revision  int    `json:"revision"`
}

// TemplateVersionOrDefault — версия содержимого шаблона для ответа/лога:
// 0 трактуется как 1 (шаблон без явной версии).
func (t Template) TemplateVersionOrDefault() int {
	if t.Version <= 0 {
		return 1
	}
	return t.Version
}
