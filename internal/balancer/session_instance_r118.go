package balancer

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
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
