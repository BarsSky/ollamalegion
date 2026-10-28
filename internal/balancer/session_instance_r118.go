package balancer

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// session_instance_r118.go — R118.9 (2026-10-14): разделение экземпляров клиента
// с одной машины.
//
// ЖАЛОБА ОПЕРАТОРА (с самого начала сессии): «выдавать запрос от одной машины, но
// от двух экземпляров клиента Cline — балансер не разделяет».
//
// ПРИЧИНА. Когда клиент не называет ни пользователя, ни сессию, ни chat_id,
// идентификатор сессии собирался из отпечатка машины и User-Agent
// (`fp:<хеш IP+UA>::<clientName>::<model>::<endpoint>`) — у двух экземпляров Cline
// на одной машине он ОДИНАКОВЫЙ. Итог: общая привязка к бэкенду, общий «кошелёк»
// справедливости в admission-очереди, в панели — одна сессия на два клиента.
//
// РЕШЕНИЕ. Добавляем признак, который есть в каждом запросе и различает именно
// ЭКЗЕМПЛЯР (точнее — РАЗГОВОР): первый стабильный блок сообщений. Cline
// пересылает историю целиком в каждом запросе, поэтому первый блок (системная
// инструкция + первая задача) у одной задачи не меняется от запроса к запросу, а у
// двух экземпляров, работающих над разными задачами, — различается.
//
// Важно: ключ считается по НАЧАЛУ разговора, а не по последнему сообщению (оно
// меняется каждый ход) и не по числу сообщений (растёт с каждым ходом) — иначе
// сессия «переезжала» бы на каждом запросе и терялась бы привязка к бэкенду.

// conversationFingerprintMinChars — минимальная длина текста, чтобы отпечаток
// вообще считался различителем. Короткие «привет»/«2+2?» у разных клиентов
// совпадают и разделять по ним нельзя.
const conversationFingerprintMinChars = 24

// conversationFingerprintMaxChars — сколько символов начала разговора берём в хеш.
// Хватает, чтобы различить задачи, и не зависит от длины промпта.
const conversationFingerprintMaxChars = 2048

// conversationFingerprint — короткий хеш начала разговора ("" — различитель
// построить нельзя).
func conversationFingerprint(body []byte) string {
	if len(body) == 0 || len(body) > 1<<20 {
		return ""
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}

	var parts []string
	if msgs, ok := req["messages"].([]interface{}); ok {
		var firstRole, firstText, firstUserText string
		for i, raw := range msgs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			text := contentText(m["content"])
			if text == "" {
				continue
			}
			if i == 0 {
				firstRole, firstText = strings.ToLower(strings.TrimSpace(role)), text
			}
			if firstUserText == "" && strings.EqualFold(role, "user") {
				firstUserText = text
			}
		}
		if firstText != "" {
			parts = append(parts, firstRole+"|"+firstText)
		}
		// Если первым идёт системная инструкция (у Cline и подобных — да), задача
		// лежит в первом сообщении пользователя: добавляем и его.
		if firstUserText != "" && firstUserText != firstText {
			parts = append(parts, "user|"+firstUserText)
		}
	}
	// Ollama /api/generate: один prompt (+ опциональный system).
	if len(parts) == 0 {
		if p, _ := req["prompt"].(string); strings.TrimSpace(p) != "" {
			parts = append(parts, "prompt|"+p)
		}
	}
	if s, _ := req["system"].(string); len(parts) == 0 && strings.TrimSpace(s) != "" {
		parts = append(parts, "system|"+s)
	}
	if len(parts) == 0 {
		return ""
	}

	text := normalizeConversationText(strings.Join(parts, "\n"))
	if len(text) < conversationFingerprintMinChars {
		return ""
	}
	if len(text) > conversationFingerprintMaxChars {
		text = text[:conversationFingerprintMaxChars]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return fmt.Sprintf("%016x", h.Sum64())
}

// contentText — текст сообщения: строка либо массив частей (OpenAI multimodal).
func contentText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []interface{}:
		var b strings.Builder
		for _, part := range t {
			m, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
				if b.Len() > 0 {
					b.WriteString(" ")
				}
				b.WriteString(strings.TrimSpace(s))
			}
		}
		return strings.TrimSpace(b.String())
	default:
		return ""
	}
}

// normalizeConversationText — схлопываем пробелы/переводы строк: одна и та же
// задача, переформатированная клиентом, должна давать тот же отпечаток.
func normalizeConversationText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// conversationFingerprintFromRequest — отпечаток разговора из тела запроса.
// Тело читается и ВОССТАНАВЛИВАЕТСЯ (как requestBodyIdentity), чтобы вызывающий
// код мог прочитать его снова.
func conversationFingerprintFromRequest(r *http.Request) string {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if len(body) == 0 || len(body) > 1<<20 {
		return ""
	}
	return conversationFingerprint(body)
}

// ============================================================================
// R252.1 (2026-10-28): признаки «это продолжение того же разговора».
// ============================================================================
//
// ЖИВОЙ ДЕФЕКТ, который это лечит. Cline сжимает историю МЕЖДУ ходами: когда
// запрос приходит, стрима уже нет, HasActiveStream = false, а начало разговора
// переписано сводкой. Признак «активный стрим» как условие переиспользования
// сессии такой случай НЕ покрывает — сессия рождалась заново посреди задачи.
//
// Что переживает сжатие и потому служит признаком продолжения:
//   - первый SYSTEM-промпт (у Cline в нём workspace, режим, инструкции) —
//     остаётся прежним, сводка его не заменяет;
//   - ХВОСТ первого user-сообщения — Cline переносит содержимое прежней задачи в
//     сводку дословно, поэтому новый текст обычно начинается с этого хвоста или
//     содержит его.
//
// Тексты храним УСЕЧЁННЫМИ: полные простыни (файлы, логи) в карте сессий копить
// нельзя.

// conversationPartChars — сколько символов начала/хвоста сообщения запоминаем.
const conversationPartChars = 256

// conversationParts — усечённые признаки разговора.
type conversationParts struct {
	SystemHead string // начало первого system-сообщения
	UserHead   string // начало первого user-сообщения
	UserTail   string // конец первого user-сообщения
	// R252.2: дополнительные признаки продолжения — без них «тот же system»
	// сливал бы ДВЕ РАЗНЫЕ последовательные задачи в одном workspace.
	// UserHasSummaryMarker — в новом тексте есть маркер сводки (Cline пишет
	// «[Summary of earlier conversation]» / «по причине переполнения» и т.п.).
	UserHasSummaryMarker bool
	// TaskID — идентификатор задачи из environment_details, если Cline его положил
	// в текст («Current Task ID: …»). Стабилен внутри задачи и меняется между ними.
	TaskID string
}

// conversationOverlapMinRunes — минимальное ПЕРЕСЕЧЕНИЕ текстов (в рунах), которое
// считается признаком продолжения разговора (R252.2).
//
// 32 руны — это примерно фраза в 3-4 слова: достаточно, чтобы случайное совпадение
// («поправь импорты») не склеило две разные задачи, и достаточно мало, чтобы
// поймать перенесённый в сводку хвост прежнего запроса.
const conversationOverlapMinRunes = 32

// summaryMarkerSubstrings — маркеры сжатия истории (регистр не важен).
//
// Список намеренно короткий и предметный: это ровно те формулировки, которыми
// клиенты помечают «история урезана», а не любые слова про контекст.
var summaryMarkerSubstrings = []string{
	"summary of earlier conversation",
	"summary of the earlier conversation",
	"previous conversation",
	"context truncated",
	"context window",
	"conversation was summarized",
	"summarized to save",
	"сводка предыдущ",
	"сводка предыд",
	"по причине переполнения",
	"из-за переполнения",
	"переполнения контекста",
	"история сокращена",
	"предыдущего разговора",
}

// hasSummaryMarker — есть ли в тексте маркер сжатия истории (R252.2).
func hasSummaryMarker(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, m := range summaryMarkerSubstrings {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// overlapRunes — есть ли у двух текстов общий кусок длиной не меньше n рун.
//
// Окна скользят по тексту a с шагом n/4: полный перебор по каждой руне не нужен
// (тексты усечены до conversationPartChars), а шаг даёт перекрытие окон в 4 раза,
// поэтому реальное совпадение короче n не «просочится».
func overlapRunes(a, b string, n int) bool {
	if n <= 0 || a == "" || b == "" {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < n || len(rb) < n {
		return false
	}
	step := n / 4
	if step < 1 {
		step = 1
	}
	for i := 0; i+n <= len(ra); i += step {
		if strings.Contains(b, string(ra[i:i+n])) {
			return true
		}
	}
	return false
}

// conversationOverlap — признак продолжения по пересечению сохранённых частей
// разговора (R252.2): сравниваем начало/хвост нового первого user-сообщения с
// началом/хвостом прежнего.
func conversationOverlap(now, saved conversationParts, n int) bool {
	if saved.UserHead == "" && saved.UserTail == "" {
		return false
	}
	pairs := [][2]string{
		{now.UserHead, saved.UserHead},
		{now.UserHead, saved.UserTail},
		{now.UserTail, saved.UserHead},
		{now.UserTail, saved.UserTail},
	}
	for _, pr := range pairs {
		if overlapRunes(pr[0], pr[1], n) {
			return true
		}
	}
	return false
}

// clipHead — нормализованное начало строки (не более conversationPartChars рун).
func clipHead(s string) string {
	r := []rune(normalizeConversationText(s))
	if len(r) > conversationPartChars {
		r = r[:conversationPartChars]
	}
	return string(r)
}

// clipTail — нормализованный конец строки (не более conversationPartChars рун).
func clipTail(s string) string {
	r := []rune(normalizeConversationText(s))
	if len(r) > conversationPartChars {
		r = r[len(r)-conversationPartChars:]
	}
	return string(r)
}

// conversationPartsFromBody — признаки из уже прочитанного тела запроса.
func conversationPartsFromBody(body []byte) conversationParts {
	if len(body) == 0 || len(body) > 1<<20 {
		return conversationParts{}
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return conversationParts{}
	}
	var out conversationParts
	firstSystem := ""
	firstUser := ""
	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			text := contentText(m["content"])
			if text == "" {
				continue
			}
			switch {
			case firstSystem == "" && strings.EqualFold(role, "system"):
				firstSystem = text
			case firstUser == "" && strings.EqualFold(role, "user"):
				firstUser = text
			}
			if firstSystem != "" && firstUser != "" {
				break
			}
		}
	}
	// Ollama-поверхность: system/prompt на верхнем уровне.
	if firstSystem == "" {
		if s, _ := req["system"].(string); strings.TrimSpace(s) != "" {
			firstSystem = s
		}
	}
	if firstUser == "" {
		if p, _ := req["prompt"].(string); strings.TrimSpace(p) != "" {
			firstUser = p
		}
	}
	out.SystemHead = clipHead(firstSystem)
	if firstUser != "" {
		out.UserHead = clipHead(firstUser)
		out.UserTail = clipTail(firstUser)
		out.UserHasSummaryMarker = hasSummaryMarker(firstUser)
		out.TaskID = extractTaskID(firstUser)
	}
	if out.TaskID == "" {
		out.TaskID = extractTaskID(firstSystem)
	}
	return out
}

// taskIDRe — идентификатор задачи из environment_details, который Cline кладёт в
// текст сообщения («Current Task ID: 175934…», «task_id: abc-123»).
var taskIDRe = regexp.MustCompile(`(?i)(?:current\s+)?task[\s_-]*id\s*[:=]\s*([A-Za-z0-9][A-Za-z0-9._-]{3,63})`)

// extractTaskID — идентификатор задачи из текста ("" — нет).
func extractTaskID(text string) string {
	if text == "" {
		return ""
	}
	m := taskIDRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// conversationPartsFromRequest — признаки из запроса (тело читается и
// ВОССТАНАВЛИВАЕТСЯ, как conversationFingerprintFromRequest).
func conversationPartsFromRequest(r *http.Request) conversationParts {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return conversationParts{}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return conversationParts{}
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	return conversationPartsFromBody(body)
}
