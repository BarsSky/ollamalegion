// seed.go — ОДНОКРАТНЫЙ сид записями из
// docs/operations/model-tuning-reference.ru.md.
//
// РЕШЕНИЕ ЧЕЛОВЕКА №4 (docs/plans/2026-11-17-reference-templates-design.md §6.1):
// при первом старте (файла хранилища нет) записи-рекомендации создаются из
// готовых профилей и таблицы ориентиров этого документа. Сид выполняется
// РОВНО ОДИН РАЗ и только когда файла нет: существующий файл сид не
// перезаписывает никогда (см. store.go, Load).
//
// ПОЧЕМУ КОПИЯ ЧИСЕЛ, А НЕ ЧТЕНИЕ MARKDOWN НА СТАРТЕ: автоматическое
// перечитывание доков на каждом старте сделало бы доки вторым источником истины
// — правка в markdown молча меняла бы поведение (§2.5, §5.1 п.7). Здесь числа
// зафиксированы один раз и дальше живут в файле, который правит оператор.
//
// author:"docs-import" и sources[].doc/section у каждой записи — чтобы в UI было
// видно происхождение и его можно было проверить по документу.
package reference

import (
	"encoding/json"
	"strconv"
)

// docsTuningReference — путь документа-первоисточника от корня репозитория.
const docsTuningReference = "docs/operations/model-tuning-reference.ru.md"

// SeedDoc — документ, который записывается при первом старте.
//
// notes профилей моделей сюда НЕ переносятся и в профилях не трогаются
// (решение №4): notes — отдельная строка профиля (§7 п.4 документа).
func SeedDoc(now string) Doc {
	recs := seedRecommendations()
	for i := range recs {
		recs[i].CreatedAt = now
		recs[i].UpdatedAt = now
		recs[i].Revision = 1
		if recs[i].Author == "" {
			recs[i].Author = "docs-import"
		}
	}
	return Doc{
		Version:         SchemaVersion,
		Recommendations: recs,
		Templates:       seedTemplates(now),
	}
}

// seedTemplates — наборы параметров, которые оператор применяет на бэкенд
// (страница «Справка и шаблоны», вкладка «Шаблоны»).
//
// ЗАЧЕМ ОНИ ЕСТЬ В СИДЕ. R493f (2026-11-17): живой факт — оператор открыл
// страницу с шаблонами и увидел ПУСТОЙ список: сид создавал только записи-
// рекомендации, а `Templates: []Template{}` был пуст по построению. Страница при
// этом работает (проверено headless-браузером: вкладка, «Создать шаблон»,
// применимость к модели на месте), но применять было нечего.
//
// ПАРАМЕТРЫ НЕ ДУБЛИРУЮТСЯ ЧИСЛАМИ: набор собирается ИЗ той же рекомендации,
// которую описывает (`BasedOn`), поэтому правка чисел в одном месте не
// расходится со вторым. Документ-первоисточник тот же.
func seedTemplates(now string) []Template {
	recs := seedRecommendations()
	byID := make(map[string]Recommendation, len(recs))
	for _, r := range recs {
		byID[r.ID] = r
	}

	specs := []struct {
		id      string
		name    string
		basedOn string
		comment string
	}{
		{
			id:      "tpl-agent-loop-4b",
			name:    "Агентский цикл на 4B/8 ГБ: окно 32K, без размышлений",
			basedOn: "qwen3-4b-8gb-agent",
			comment: "Для агентских циклов (Cline, OpenWebUI tools): ход должен быть коротким. " +
				"Размышления выключены — на 4B они съедают ответ, и ход завершается пустым. " +
				"Окно 32K оставляет запас под преамбулу инструментов.",
		},
		{
			id:      "tpl-27b-a10-131k",
			name:    "27B Q4 на A10 24 ГБ: окно 131K, KV q4_0",
			basedOn: "qwen3-8-27b-ud-q4-a10-24gb",
			comment: "Большая модель на одной A10: KV в q4_0 (иначе не влезает вместе с весами), " +
				"primary=true — клиентский num_ctx окно не поднимает.",
		},
		{
			id:      "tpl-reasoning-32k",
			name:    "Рассуждающая модель: окно 32K, размышления ВКЛ, потолок вывода 8192",
			basedOn: "deepseek-r1-distill",
			comment: "Для R1-дистиллятов и подобных. В агентских циклах всё равно лучше " +
				"enableReasoning=false и меньше numPredict: скорость хода важнее глубины.",
		},
	}

	out := make([]Template, 0, len(specs))
	for _, spec := range specs {
		rec, ok := byID[spec.basedOn]
		if !ok {
			continue // сид без рекомендации не создаём: шаблон обязан иметь источник
		}
		params, err := json.Marshal(rec.Params)
		if err != nil {
			continue
		}
		out = append(out, Template{
			ID:        spec.id,
			Name:      spec.name,
			Version:   1,
			Params:    json.RawMessage(params),
			Comment:   spec.comment,
			BasedOn:   spec.basedOn,
			Author:    "docs-import",
			CreatedAt: now,
			UpdatedAt: now,
			Revision:  1,
		})
	}
	return out
}

// seedRecommendations — записи из §1 и §3.1–§3.6 документа.
func seedRecommendations() []Recommendation {
	return []Recommendation{
		seedBaselineGeneral(),
		seedQwen27BA10(),
		seedQwen4B8GB(),
		seedQwen30BClass(),
		seedGemma4(),
		seedDeepSeekR1(),
		seedSmallModels(),
	}
}

// seedBaselineGeneral — «таблица ориентиров» (§1, строки 28-45): запись БЕЗ
// appliesTo — общая справка (решение №1).
func seedBaselineGeneral() Recommendation {
	return Recommendation{
		ID:    "tuning-baseline-32k",
		Title: "База: окно 32K, KV q8_0, батч 512, все слои на GPU",
		// appliesTo намеренно отсутствует: это справка «к чему угодно».
		Params: []Param{
			{Name: "contextLength", Value: rawInt(32768), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(32768), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("q8_0")},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-1), Unit: "layers",
				Comment: "-1 = все слои в VRAM; -2 = «сколько влезет по memfit»"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "useMmap", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false)},
		},
		Comment: "Отправная точка из таблицы «поле → как выбирать». Дальше двигать по одному " +
			"параметру: окно — до физического предела KV+веса; kvCacheType q8_0 вместо f16 — " +
			"когда VRAM в обрез; q4_0 — крайний случай (качество деградирует мягче, чем оффлоад слоёв). " +
			"numGpuLayers вручную не ограничивать: частичный оффлоад даёт −30…50 % скорости на каждом токене.",
		Sources: []Source{{Doc: docsTuningReference, Section: "1"}},
		Author:  "docs-import",
	}
}

// seedQwen27BA10 — §3.1 (docs/operations/model-tuning-reference.ru.md:66-85).
func seedQwen27BA10() Recommendation {
	return Recommendation{
		ID:    "qwen3-8-27b-ud-q4-a10-24gb",
		Title: "27B Q4 на A10 24 ГБ: окно 131K, KV q4_0, батч 1024",
		AppliesTo: &AppliesTo{
			ModelPattern: "Qwen3.8-27B*",
			BackendType:  "llama_cpp",
			Quantization: "Q4_K_M",
			VRAMMinMB:    22528,
		},
		Params: []Param{
			{Name: "contextLength", Value: rawInt(131072), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(131072), Unit: "tokens"},
			{Name: "contextLengthMax", Value: rawInt(131072), Unit: "tokens"},
			{Name: "contextLengthAuto", Value: rawBool(true)},
			{Name: "primary", Value: rawBool(true)},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("q4_0"),
				Comment: "100k+ контекст с q8_0 не влезает вместе с весами (15.3 GiB) в 22.5 ГБ"},
			{Name: "batchSize", Value: rawInt(1024), Comment: "префилл — ~200 с из 213 с хода"},
			{Name: "numGpuLayers", Value: rawInt(-2), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "useMmap", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false),
				Comment: "агент шлёт 80–90k промпта на ход; 2169 токенов ушли в <think> → пустой ответ"},
		},
		Comment: "Для 27B Q4 на 24 ГБ. primary=true: клиентский num_ctx не поднимает окно. " +
			"Клиенту выставить Model Context Window 96k при серверных 131k и Reasoning Effort minimal — " +
			"тогда есть запас на ответ и нет 503+reload. Проверено на стенде: окно 180224/188416 грузится.",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.1"}},
		Author:  "docs-import",
	}
}

// seedQwen4B8GB — §3.2 (docs/operations/model-tuning-reference.ru.md:87-99).
func seedQwen4B8GB() Recommendation {
	return Recommendation{
		ID:    "qwen3-4b-8gb-agent",
		Title: "4B Q4 на 8 ГБ VRAM: окно 32K, KV f16, один слот",
		AppliesTo: &AppliesTo{
			ModelPattern: "Qwen3-4B*",
			BackendType:  "llama_cpp",
			Quantization: "Q4_K_M",
			VRAMMinMB:    8192,
		},
		Params: []Param{
			{Name: "contextLength", Value: rawInt(32768), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(32768), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1), Comment: "1 слот: предсказуемая скорость, окно не делится"},
			{Name: "kvCacheType", Value: rawStr("f16"),
				Comment: "при 32k и 4B весах память позволяет — качество лучше"},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-1), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false),
				Comment: "на 4B размышления чаще портят tool-call, чем помогают"},
		},
		Comment: "Агентский цикл на 8 ГБ: окно 32K держит сессию без reload, размышления выключены " +
			"(они едят бюджет хода). Для 3B можно поднять окно до 64K.",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.2"}},
		Author:  "docs-import",
	}
}

// seedQwen30BClass — §3.3 (docs/operations/model-tuning-reference.ru.md:101-113).
// appliesTo нет: класс покрывает разные имена (Qwen3-32B, Qwen3.6-35B-A3B),
// одного glob'а у него нет — это общая справка по классу.
func seedQwen30BClass() Recommendation {
	return Recommendation{
		ID:    "qwen3-30b-class",
		Title: "30B+ класс на 24 ГБ: скромное окно 16K и KV q8_0",
		Params: []Param{
			{Name: "contextLength", Value: rawInt(16384), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(16384), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("q8_0")},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-2), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "useMmap", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false)},
		},
		Comment: "На 24 ГБ 30B-класс держит ~16k с q8_0; больше — оффлоад слоёв и падение скорости " +
			"в 2–3 раза. MoE (35B-A3B) переносит окно легче, но KV считается по полному числу слоёв.",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.3"}},
		Author:  "docs-import",
	}
}

// seedGemma4 — §3.4 (docs/operations/model-tuning-reference.ru.md:115-127).
func seedGemma4() Recommendation {
	return Recommendation{
		ID:    "gemma-4-reasoning-tags",
		Title: "gemma-4: окно 32K, KV q8_0, размышления выключены",
		AppliesTo: &AppliesTo{
			ModelPattern: "gemma-4*",
			BackendType:  "llama_cpp",
			Quantization: "Q4_K_M",
		},
		Params: []Param{
			{Name: "contextLength", Value: rawInt(32768), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(32768), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("q8_0")},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-1), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false),
				Comment: "у gemma-4 размышления приходят тегами <|channel>thought и попадают в content (R216/R219)"},
		},
		Comment: "Профиль по умолчанию для gemma-4: размышления выключены, включать только вручную.",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.4"}},
		Author:  "docs-import",
	}
}

// seedDeepSeekR1 — §3.5 (docs/operations/model-tuning-reference.ru.md:129-144).
func seedDeepSeekR1() Recommendation {
	return Recommendation{
		ID:    "deepseek-r1-distill",
		Title: "DeepSeek-R1-Distill: окно 32K, размышления ВКЛ и потолок вывода 8192",
		AppliesTo: &AppliesTo{
			ModelPattern: "DeepSeek-R1-Distill-*",
			BackendType:  "llama_cpp",
		},
		Params: []Param{
			{Name: "contextLength", Value: rawInt(32768), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(32768), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("q8_0")},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-2), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(true),
				Comment: "модель рассуждающая по природе"},
			{Name: "maxTokens", Value: rawInt(8192), Comment: "обязателен: иначе done_reason=length и пустой ответ"},
		},
		Comment: "Для рассуждающей модели. На агентских циклах всё равно рекомендуется " +
			"enableReasoning=false и maxTokens поменьше: скорость хода важнее глубины.",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.5"}},
		Author:  "docs-import",
	}
}

// seedSmallModels — §3.6 (docs/operations/model-tuning-reference.ru.md:146-158).
// appliesTo нет: правило покрывает несколько разных семейств (Qwen3-8B,
// Llama-3.1-8B, Mistral-Nemo, Phi-4-14B).
func seedSmallModels() Recommendation {
	return Recommendation{
		ID:    "small-models-32k-f16",
		Title: "Малые модели (7–14B): окно 32K, KV f16, все слои на GPU",
		Params: []Param{
			{Name: "contextLength", Value: rawInt(32768), Unit: "tokens"},
			{Name: "contextPerSeq", Value: rawInt(32768), Unit: "tokens"},
			{Name: "parallel", Value: rawInt(1)},
			{Name: "kvCacheType", Value: rawStr("f16")},
			{Name: "batchSize", Value: rawInt(512)},
			{Name: "numGpuLayers", Value: rawInt(-1), Unit: "layers"},
			{Name: "flashAttn", Value: rawBool(true)},
			{Name: "enableReasoning", Value: rawBool(false)},
		},
		Comment: "Правило: если веса + KV при 32k влезают в VRAM с запасом ~15 % — f16 и все слои " +
			"на GPU; как только перестают — q8_0, затем q4_0 (в этом порядке: качество деградирует " +
			"мягче, чем при оффлоаде слоёв).",
		Sources: []Source{{Doc: docsTuningReference, Section: "3.6"}},
		Author:  "docs-import",
	}
}

// --- кодирование значений сида ------------------------------------------

// rawInt — целое как элемент JSON.
func rawInt(v int) json.RawMessage {
	return json.RawMessage(strconv.Itoa(v))
}

// rawStr — строка как элемент JSON.
func rawStr(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(b)
}

// rawBool — булево как элемент JSON.
func rawBool(v bool) json.RawMessage {
	if v {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}
