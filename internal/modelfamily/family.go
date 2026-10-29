// Package modelfamily — R268 (2026-10-28): СЕМЕЙСТВА МОДЕЛЕЙ как данные, а не как
// условия в коде.
//
// ЖИВАЯ ПРИЧИНА ПОЯВЛЕНИЯ ПАКЕТА. Поддержка семейств в llama.cpp-тракте была
// размазана по десяткам условий в разных файлах: `isGemmaModel`,
// `defaultAntipromptsForModel`, ветки `strings.Contains(name, "qwen3")` в
// стоп-строках, пять списков триггеров текстовых tool-call'ов в детекторе,
// отдельные списки служебных меток в фильтре, отдельные проверки версий в
// n_ctx-коде. Обновление одного семейства означало правку в нескольких местах,
// и любая забытая точка возвращала дефект («ответ вперемешку с метками»,
// неверные стоп-строки, потерянный вызов инструмента).
//
// ПРАВИЛО, КОТОРОЕ ЭТОТ ПАКЕТ ЗАКРЕПЛЯЕТ:
//   - всё, что мы знаем о семействе (как опознать, какие версии поддержаны,
//     какой у него формат tool-call, какие служебные метки и стоп-строки),
//     живёт в ОДНОМ файле `family_<id>.go`;
//   - добавление семейства = новый файл + `func init() { Register(...) }`,
//     БЕЗ правки switch/if в тракте;
//   - порядок применения детерминирован (приоритет, затем id), а не зависит от
//     порядка инициализации файлов;
//   - неизвестное семейство — НЕ ошибка: работает `generic`, то есть прежнее
//     поведение (обратная совместимость важнее строгости).
//
// ЧЕГО ПАКЕТ НЕ ДЕЛАЕТ. Он не парсит и не конвертирует ответы: детекторы и
// фильтры остаются на месте, но берут из семейства СПИСКИ (метки, стоп-строки,
// формат вызова) вместо собственных хардкодов. Так одна правка семейства
// доезжает до всех точек сразу.
package modelfamily

import (
	"fmt"
	"sort"
	"strings"
)

// Status — насколько семейство/версия пригодны для КОНВЕРТАЦИИ в Ollama API.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ТИП, А НЕ bool. Отказ должен быть объяснимым: оператору
// бесполезно «не поддерживается» без указания семейства, версии, диапазона и
// того, каким портом пользоваться. Status — часть этого объяснения.
type Status int

const (
	// Supported — семейство известно и версия в поддержанном диапазоне.
	Supported Status = iota
	// UnknownFamily — семейство не опознано; работает generic (прежнее поведение).
	UnknownFamily
	// UnsupportedVersion — семейство известно, но версия выше поддержанной.
	UnsupportedVersion
)

func (s Status) String() string {
	switch s {
	case Supported:
		return "supported"
	case UnknownFamily:
		return "unknown_family"
	case UnsupportedVersion:
		return "unsupported_version"
	default:
		return "unknown"
	}
}

// Meta — контекст запроса, влияющий на выбор семейства.
//
// ЗАЧЕМ. Совпадения по имени модели недостаточно: у одного и того же файла
// поведение зависит от того, объявлены ли инструменты и в какой протокол
// конвертируем. Поле необязательное — пустой Meta означает «решает только имя»,
// что и нужно большинству вызовов.
type Meta struct {
	// HasTools — в запросе объявлены tools[] (клиент ждёт tool_calls).
	HasTools bool
	// Endpoint — путь запроса ("/api/chat", "/v1/chat/completions").
	Endpoint string
}

// Capabilities — всё, что тракт должен знать о семействе, чтобы правильно
// собрать запрос и отфильтровать ответ. Ноль-значение = «ничего особенного».
type Capabilities struct {
	// NativeTools — модель/шаблон умеет нативные tool_calls (иначе вызов
	// приходит текстом и его надо распознавать детектором).
	NativeTools bool
	// ReasoningChannel — модель эмитит размышления отдельным каналом/токенами
	// (qwen3.8 `<|channel|>`, gemma `</think>`, deepseek `[THINK]`).
	ReasoningChannel bool
	// ToolCallTextForm — диалект текстового вызова для детектора:
	// "json_array" | "hermes" | "python_tag" | "tool_calls_marker" |
	// "tool_code" | "gemma_turn" | "" (нет особого диалекта).
	ToolCallTextForm string
	// StopStrings — стоп-строки (antiprompts), которые cppworker обязан
	// применить, чтобы модель не уезжала в следующий ход.
	StopStrings []string
	// ServiceMarkers — служебные метки шаблона, которые НЕ должны попадать в
	// видимый текст (вычищаются фильтром).
	ServiceMarkers []string
}

// VersionRange — какие версии семейства мы умеем конвертировать.
//
// MaxMajor == 0 означает «верхней границы нет»: так живут семейства, у которых
// мы не знаем линейку версий (hermes/mistral/deepseek), и generic. Это осознанно
// мягкое поведение: придумывать отказ там, где мы не знаем линейку, значило бы
// ломать работу на моделях, которые сегодня прекрасно обслуживаются.
type VersionRange struct {
	MinMajor int
	MaxMajor int
}

func (v VersionRange) String() string {
	switch {
	case v.MaxMajor == 0 && v.MinMajor == 0:
		return "любая"
	case v.MaxMajor == 0:
		return fmt.Sprintf(">= %d", v.MinMajor)
	case v.MinMajor == 0:
		return fmt.Sprintf("<= %d", v.MaxMajor)
	default:
		return fmt.Sprintf("%d..%d", v.MinMajor, v.MaxMajor)
	}
}

// Supports — входит ли major-версия в диапазон. major == 0 («версия не
// определена») считается поддержанной: отказывать на основании незнания нельзя.
func (v VersionRange) Supports(major int) bool {
	if major <= 0 {
		return true
	}
	if v.MinMajor > 0 && major < v.MinMajor {
		return false
	}
	if v.MaxMajor > 0 && major > v.MaxMajor {
		return false
	}
	return true
}

// Family — одно семейство моделей. Всё, что о нём нужно знать, — здесь.
type Family interface {
	// ID — стабильный идентификатор ("qwen", "gemma", ...). Используется в
	// логах и в API; менять нельзя, на него смотрят тесты и оператор.
	ID() string
	// Priority — порядок применения: больше = раньше. Нужен, потому что имена
	// пересекаются ("qwen" внутри "qwq"? "gemma" внутри "gemma-4"): конкретные
	// семейства обязаны выигрывать у generic независимо от порядка init.
	Priority() int
	// Match — относится ли модель к семейству (по имени + контексту).
	Match(modelName string, meta Meta) bool
	// Versions — поддержанные версии для КОНВЕРТАЦИИ.
	Versions() VersionRange
	// CapabilitiesFor — возможности с учётом версии (у gemma-4 и gemma-2
	// разные стоп-строки, поэтому версия — параметр, а не поле).
	CapabilitiesFor(major int) Capabilities
	// DetectVersion — версия из имени модели (0, "" = не определена).
	//
	// В ИНТЕРФЕЙСЕ, А НЕ ЧЕРЕЗ TYPE-ASSERT: семейства регистрируются чужими
	// файлами и в тестах, и паника «интерфейс не реализован» вместо понятного
	// «версия не определена» здесь недопустима.
	DetectVersion(modelName string) (major int, version string)
	// Notes — человекочитаемая заметка для документации и логов.
	Notes() string
}

// Resolution — результат опознания: семейство, статус и объяснение.
type Resolution struct {
	Family  Family
	Status  Status
	Major   int    // определённая major-версия (0 = не определена)
	Version string // версия как в имени модели ("3.8"), "" = не определена
	Reason  string // почему такой статус (для лога и ответа)
}

// GenericID — id семейства-фолбэка. Вынесен, чтобы вызовы не сравнивали строку
// с литералом в нескольких местах.
const GenericID = "generic"

// registry — упорядоченный список семейств.
//
// ПОЧЕМУ slice, а не map: порядок применения обязан быть детерминированным.
// Итерация по map в Go случайна, и «какое семейство выиграло» зависело бы от
// запуска — это уже ловили в этом репозитории на других реестрах.
var registry []Family

// Register — добавить семейство в реестр. Вызывается из init() файла семейства.
func Register(f Family) {
	if f == nil {
		return
	}
	// Идемпотентность: повторная регистрация того же id (например после
	// hot-reload в тестах) не должна плодить дубликаты.
	for _, existing := range registry {
		if existing.ID() == f.ID() {
			return
		}
	}
	registry = append(registry, f)
}

// Families — копия реестра в порядке применения (priority desc, затем id).
func Families() []Family {
	out := make([]Family, len(registry))
	copy(out, registry)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority() != out[j].Priority() {
			return out[i].Priority() > out[j].Priority()
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// Resolve — опознать семейство и версию модели.
//
// ЛОГИКА ОТКАЗА. Отказ (UnsupportedVersion) выдаётся ТОЛЬКО когда семейство
// опознано и его версия выше поддержанной. Неизвестное имя не отказ: работает
// generic, то есть ровно то поведение, которое было до появления пакета.
func Resolve(modelName string, meta Meta) Resolution {
	for _, f := range Families() {
		if !f.Match(modelName, meta) {
			continue
		}
		major, version := f.DetectVersion(modelName)
		res := Resolution{Family: f, Major: major, Version: version}
		v := f.Versions()
		if v.Supports(major) {
			res.Status = Supported
			res.Reason = fmt.Sprintf("семейство %s, версия %s в поддержанном диапазоне %s",
				f.ID(), versionOrUnknown(version), v)
			return res
		}
		res.Status = UnsupportedVersion
		res.Reason = fmt.Sprintf("семейство %s, версия %s выше поддержанной (%s)",
			f.ID(), versionOrUnknown(version), v)
		return res
	}
	// Сюда попадаем только если generic не зарегистрирован (например пакет
	// урезали в тесте) — честно сообщаем «неизвестно», а не притворяемся.
	return Resolution{Status: UnknownFamily, Reason: "семейство не опознано"}
}

// Of — семейство модели без учёта поддержки версии (для стоп-строк и меток).
func Of(modelName string) Family {
	res := Resolve(modelName, Meta{})
	if res.Family == nil {
		return nil
	}
	return res.Family
}

// StopStringsFor — стоп-строки модели по её семейству и версии.
//
// Это ЕДИНСТВЕННАЯ точка, откуда тракт берёт antiprompts: раньше их собирала
// функция с четырьмя ветками в cmd/cppworker, и обновление семейства требовало
// помнить про неё.
func StopStringsFor(modelName string) []string {
	res := Resolve(modelName, Meta{})
	if res.Family == nil {
		return nil
	}
	return res.Family.CapabilitiesFor(res.Major).StopStrings
}

// ServiceMarkersFor — служебные метки шаблона, которые нельзя показывать
// клиенту в видимом тексте (объединение меток семейства и generic-набора
// вызывающего кода делает сам вызывающий, если ему нужен более широкий список).
func ServiceMarkersFor(modelName string) []string {
	res := Resolve(modelName, Meta{})
	if res.Family == nil {
		return nil
	}
	return res.Family.CapabilitiesFor(res.Major).ServiceMarkers
}

// SupportsConversion — можно ли конвертировать ответ этой модели в Ollama API.
func SupportsConversion(modelName string, meta Meta) bool {
	return Resolve(modelName, meta).Status != UnsupportedVersion
}

// RefusalText — текст отказа для оператора и клиента.
//
// ПОЧЕМУ ТАК ПОДРОБНО. Живая жалоба оператора была не «ошибка», а «непонятно,
// почему не работает». Поэтому в тексте: что именно не поддержано (модель,
// семейство, версия), какой диапазон поддержан и КУДА идти — на порт
// OpenAI-passthrough, где конвертации нет и ответ отдаётся как есть.
func RefusalText(modelName string, res Resolution, openAIPort int) string {
	familyID := "generic"
	rangeStr := "любая"
	if res.Family != nil {
		familyID = res.Family.ID()
		rangeStr = res.Family.Versions().String()
	}
	return fmt.Sprintf(
		"модель %q (семейство %s, версия %s) не поддерживается для конвертации в Ollama API — "+
			"поддержаны версии %s. Используйте порт %d: там OpenAI-совместимый эндпоинт без конвертации, "+
			"ответ отдаётся как есть",
		modelName, familyID, versionOrUnknown(res.Version), rangeStr, openAIPort)
}

func versionOrUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "не определена"
	}
	return v
}

// ─── Помощники опознания версии ─────────────────────────────────────────────

// digitsAfterToken — вытащить версию, стоящую сразу после токена семейства.
//
// Поддерживает самые распространённые записи имён: "gemma-4-E4B", "gemma4",
// "qwen3.8-27b", "Qwen3.5", "llama-3.1-8b", "llama3". Возвращает (major, "3.8").
// Разделители между токеном и цифрой (- _ . пробел) допускаются: имена моделей
// пишут и "qwen3.8", и "qwen-3.8", и "Qwen 3.8".
func digitsAfterToken(lower, token string) (major int, version string) {
	idx := strings.Index(lower, token)
	if idx < 0 {
		return 0, ""
	}
	i := idx + len(token)
	for i < len(lower) && (lower[i] == '-' || lower[i] == '_' || lower[i] == ' ' || lower[i] == '.') {
		i++
	}
	start := i
	for i < len(lower) && lower[i] >= '0' && lower[i] <= '9' {
		i++
	}
	if start == i {
		return 0, ""
	}
	for j := start; j < i; j++ {
		major = major*10 + int(lower[j]-'0')
	}
	ver := lower[start:i]
	// Суффикс размера модели: "qwen-7b" — это 7 МИЛЛИАРДОВ параметров, а не
	// версия 7. Без этой проверки модель "qwen-7b" получала major=7 и попадала
	// под отказ «версия выше поддержанной» — то есть ломалась бы исправная
	// работа. Касается только латинских b/m/k сразу после цифр.
	if i < len(lower) && (lower[i] == 'b' || lower[i] == 'm' || lower[i] == 'k') {
		return 0, ""
	}
	// Дробная часть: "3.8" — иначе qwen3.8 выглядел бы как "3".
	if i < len(lower) && lower[i] == '.' {
		j := i + 1
		st := j
		for j < len(lower) && lower[j] >= '0' && lower[j] <= '9' {
			j++
		}
		if st != j {
			ver = lower[start:j]
		}
	}
	return major, strings.TrimSuffix(ver, ".")
}

// containsAny — есть ли в строке (lowercase) любой из токенов.
func containsAny(lower string, tokens ...string) bool {
	for _, t := range tokens {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// firstDigitsAfterAny — версия после ПЕРВОГО найденного токена семейства.
func firstDigitsAfterAny(lower string, tokens ...string) (major int, version string) {
	for _, t := range tokens {
		if m, v := digitsAfterToken(lower, t); v != "" {
			return m, v
		}
	}
	return 0, ""
}

// ─── Общая основа семейства ─────────────────────────────────────────────────

// base — общая часть любого семейства: id, приоритет, диапазон, токены имени.
//
// ЧТО ЗДЕСЬ, А ЧТО В ФАЙЛЕ СЕМЕЙСТВА. В base — механика (id/приоритет/версии/
// опознание версии), в файле семейства — только Match и CapabilitiesFor, то есть
// ровно те знания, которые меняются при обновлении семейства. Так «добавить
// семейство» = скопировать шаблон и заполнить две функции, без правок тракта.
type base struct {
	id       string
	priority int
	versions VersionRange
	// tokens — по ним ищется версия в имени модели ("gemma", "qwen", ...).
	tokens []string
	// caps — возможности как функция версии (nil = пустые возможности).
	caps func(major int) Capabilities
	// notes — короткая заметка (уходит в логи/документацию).
	notes string
}

func (b base) ID() string { return b.id }

func (b base) Priority() int { return b.priority }

func (b base) Versions() VersionRange { return b.versions }

func (b base) Notes() string { return b.notes }

func (b base) CapabilitiesFor(major int) Capabilities {
	if b.caps == nil {
		return Capabilities{}
	}
	return b.caps(major)
}

func (b base) DetectVersion(modelName string) (int, string) {
	// Семейство без линейки версий (hermes/mistral/deepseek) не должно
	// «находить» версию в размере модели: "mistral-nemo-12b" — это 12B
	// параметров, а не версия 12. Иначе такие модели получали бы неверный
	// диапазон и ложный отказ на исправной работе.
	if b.versions.MinMajor == 0 && b.versions.MaxMajor == 0 {
		return 0, ""
	}
	return firstDigitsAfterAny(strings.ToLower(modelName), b.tokens...)
}

// defaultStopStrings — стоп-строки «по умолчанию» для семейств, у которых нет
// собственного набора.
//
// ИСТОРИЯ (R248, 2026-10-27): из наборов убраны "<|im_end|>"/"<|end|>" — модель
// цитирует их в тексте, а проверка суффиксная и обрывала ответ посреди фразы у
// Cline. Эти строки — ровно то, что было в ветке default старой функции, поэтому
// поведение не меняется; собственные наборы семейств тоже перенесены дословно.
func defaultStopStrings() []string {
	return []string{"<|end|>", "<|user|>", "<|assistant|>"}
}

// gemmaTurnStops — стоп-строки gemma-4 (без "<end_of_turn>": модель цитирует его
// в рассуждениях, и генерация обрывалась на ~85 токенах; см. Round 32 #27).
func gemma4StopStrings() []string {
	return []string{"<start_of_turn>user", "<start_of_turn>model"}
}

// gemmaOldStopStrings — stоп-строки gemma-2 и старше: тут "<end_of_turn>" —
// настоящий маркер конца хода, а не цитата.
func gemmaOldStopStrings() []string {
	return []string{"<end_of_turn>", "<start_of_turn>user", "<start_of_turn>model"}
}
