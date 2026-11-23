// aggregate.go — R715 (2026-11-24): схлопывание повторов в кольцевом буфере
// логов (дефект D3 части 3 плана docs/plans/2026-11-24-remaining-defects.md).
//
// ЖИВОЙ ФАКТ, РАДИ КОТОРОГО ЭТО НАПИСАНО. Кольцевой буфер балансера
// (LB_LOG_BUFFER_ENTRIES=2000) покрывал 13 минут работы стенда: 1565 строк из
// 2000 были ОДНИМ И ТЕМ ЖЕ предупреждением дискового гейта
// («backend disk space critical, blocking backend=A10-126 disk_free_mb=9480 …»).
// Всё, что было раньше 13 минут, утрачено — то есть архив для разбора снова не
// содержал нужного окна.
//
// ЧТО ЗДЕСЬ. Ключ нормализации повторяющегося события и счётчик занимаемого
// места. Само схлопывание делает брокер (broker.go, режим aggregate): повтор с
// тем же ключом НЕ занимает новую запись в кольце, а увеличивает Repeated у уже
// лежащей и обновляет LastTime. Поэтому растёт не только читаемость (одна
// строка «×N» вместо 1565), но и ОКНО: место в буфере тратится на РАЗНЫЕ
// события, а не на копии одного.
//
// КЛЮЧ: уровень + сообщение с вырезанными числами + «идентичность» события.
//
// Почему не только «уровень+сообщение» (как в первой редакции плана): в логе
// балансера есть сообщения, у которых текст постоянный, а событие — разное.
// Пример: «request trace» (одна строка на КАЖДЫЙ запрос, различие в полях
// request_id/path/status/duration_ms). Схлопывание по одному тексту собрало бы
// тысячу разных запросов в одну строку и уничтожило бы ровно тот след, ради
// которого лог и читают. Поэтому числа вырезаются только из ТЕКСТА сообщения, а
// из полей берутся лишь те, что задают идентичность события (список
// aggregateIdentityFieldsR715): request_id, путь, бэкенд, модель. Волатильные
// числа (disk_free_mb, duration_ms, статусы) в ключ НЕ входят — именно они
// меняются от повтора к повтору и мешали бы схлопыванию.
package logger

import (
	"sort"
	"strings"
	"time"
)

// aggregateIdentityFieldsR715 — поля, значение которых входит в ключ повтора
// БЕЗ нормализации (точное значение).
//
// Правило отбора: поле отвечает на вопрос «о каком событии/объекте строка».
// Если такое поле различается — это разные события, и схлопывать их нельзя.
var aggregateIdentityFieldsR715 = []string{
	"request_id",
	"session_id",
	"client_id",
	"trace_id",
	"job_id",
	"stream_id",
	"prompt_id",
	"run_id",
	"conversation_id",
	"agent_id",
	"user_id",
	"backend",
	"model",
	"path",
	"host",
	"url",
}

// AggregateIdentityFieldsR715 — копия списка для диагностики и тестов.
func AggregateIdentityFieldsR715() []string {
	out := make([]string, len(aggregateIdentityFieldsR715))
	copy(out, aggregateIdentityFieldsR715)
	return out
}

// AggregateKeyR715 — ключ повтора для записи. Пустая строка означает «не
// схлопывать» (запись без уровня/сообщения не считается повтором: пустых
// сообщений в логе нет, и склеивать их в одну строку было бы подменой данных).
func AggregateKeyR715(e LogEntry) string {
	level := strings.ToLower(strings.TrimSpace(e.Level))
	msg := NormalizeLogMessageR715(e.Message)
	if level == "" && msg == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(level) + len(msg) + 32)
	b.WriteString(level)
	b.WriteByte('|')
	b.WriteString(msg)
	if len(e.Fields) == 0 {
		return b.String()
	}
	// Идентичность — в стабильном порядке ключей: иначе одинаковые события
	// получали бы разные ключи из-за порядка обхода карты (он в Go случаен).
	idKeys := make([]string, 0, 4)
	for _, k := range aggregateIdentityFieldsR715 {
		if v, ok := e.Fields[k]; ok {
			if s := flatFieldValueR715(v); s != "" {
				idKeys = append(idKeys, k+"="+s)
			}
		}
	}
	if len(idKeys) == 0 {
		return b.String()
	}
	sort.Strings(idKeys)
	for _, kv := range idKeys {
		b.WriteByte('|')
		b.WriteString(kv)
	}
	return b.String()
}

// NormalizeLogMessageR715 — текст сообщения без чисел.
//
// Каждый непрерывный набор цифр заменяется на '#'. Так «диск 9480 МБ» и «диск
// 9471 МБ» становятся одним сообщением, а структура текста и слова остаются
// читаемыми (это же значение попадает в лог как representative записи).
//
// Пробелы по краям срезаются, внутренние подряд идущие пробелы сжимаются: в
// тексте они не различимы, а в ключе создавали бы разные записи.
func NormalizeLogMessageR715(msg string) string {
	if msg == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(msg))
	prevSpace := false
	prevDigit := false
	for _, r := range msg {
		switch {
		case r >= '0' && r <= '9':
			if !prevDigit {
				b.WriteByte('#')
			}
			prevDigit = true
			prevSpace = false
			continue
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
			prevDigit = false
			continue
		default:
			b.WriteRune(r)
			prevSpace = false
			prevDigit = false
		}
	}
	return strings.TrimSpace(b.String())
}

// flatFieldValueR715 — значение поля одной строкой (для ключа, без кавычек).
func flatFieldValueR715(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case time.Duration:
		return t.String()
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(FieldValueText(v))
	}
}

// logEntryBaseBytesR715 — постоянная часть записи в памяти: заголовок структуры
// (time.Time 24 Б, три строки по 16 Б, указатель на карту 8 Б, счётчик 8 Б,
// два указателя времени 16 Б), плюс неизбежный оверхеды аллокатора.
const logEntryBaseBytesR715 = 160

// logFieldPairBytesR715 — оверхед одной пары «ключ: значение» в map: бакет
// map на пару в среднем ~50 Б, плюс сами строки/интерфейс.
const logFieldPairBytesR715 = 96

// LogEntryApproxBytesR715 — оценка того, сколько памяти занимает запись в
// буфере. Нужна, чтобы обосновать размер буфера числом, а не обещанием:
// «5000 записей × ~1 КБ ≈ 5 МБ» (см. TestR715_LogEntryMemoryBudget).
//
// Оценка намеренно консервативная (сверху): строки Go не делят память с
// маленькими литералами при склейке, а карта полей требует бакетов.
func LogEntryApproxBytesR715(e LogEntry) int {
	total := logEntryBaseBytesR715 + len(e.Level) + len(e.Message) + len(e.Source)
	for k, v := range e.Fields {
		total += logFieldPairBytesR715 + len(k) + len(flatFieldValueR715(v))
	}
	return total
}

// LogEntriesApproxBytesR715 — сумма по срезу.
func LogEntriesApproxBytesR715(entries []LogEntry) int {
	total := 0
	for _, e := range entries {
		total += LogEntryApproxBytesR715(e)
	}
	return total
}

// FirstOccurrence — время первого появления события (для схлопнутой записи —
// первого из повторов; для обычной — собственное время).
func (e LogEntry) FirstOccurrence() time.Time {
	if e.FirstTime != nil && !e.FirstTime.IsZero() {
		return *e.FirstTime
	}
	return e.Time
}

// LastOccurrence — время последнего появления события.
func (e LogEntry) LastOccurrence() time.Time {
	if e.LastTime != nil && !e.LastTime.IsZero() {
		return *e.LastTime
	}
	return e.Time
}
