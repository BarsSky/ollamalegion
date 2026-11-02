// session_resolver_r378.go — R378 (2026-11-01): ОДИН резолвер ключей
// сессии/очереди (P2-3 из аудита регрессий).
//
// ЗАЧЕМ. Для одного и того же запроса балансер считал ДВА идентификатора:
//
//   - ключ очереди (admission, кошелёк справедливости) — RequestSessionKey;
//   - ключ маршрутизации/привязки к бэкенду — getSessionIDWithModel.
//
// Оба читали одни и те же источники (заголовки пользователя/сессии/чата, поля
// тела, metadata, начало разговора), но каждый — СВОИМ списком, в client.go и
// autoload_wait.go. Пока списки совпадают, это невидимо; расхождение началось
// сразу: ключ очереди знал X-Chat-Id, а extractChatIDFromBody — ещё и
// X-Conversation-Id; ключ очереди не смотрел cookie session_id, а ключ
// маршрутизации смотрел. Любая новая голова идентичности добавлялась в два
// места из трёх — и клиент получал два разных ответа на вопрос «одна ли это
// сессия».
//
// РЕШЕНИЕ. Источники (какие заголовки, cookie и поля тела вообще считаются
// идентичностью) описаны ОДИН раз — ниже, в списках identity*R378. Оба ключа
// строятся из них; порядок предпочтения у каждого ключа свой и осознанный
// (очередь ставит пользователя выше чата, маршрутизация — X-Client-ID выше
// пользователя), поэтому порядок НЕ унифицируется: унифицированы источники.
// Новая голова идентичности добавляется здесь и попадает в оба ключа.
//
// Точка входа для инференс-путей — resolveInferenceKeysR378: оба ключа
// считаются вместе, чтобы хендлер не мог взять один из них из другого места.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Списки источников идентичности. Менять только здесь: на них смотрит стюард
// TestR378_Steward_IdentitySourcesLiveHere.
var (
	// identityUserHeadersR378 — явный пользователь (OpenWebUI, LiteLLM).
	identityUserHeadersR378 = []string{"X-User-Id", "X-OpenWebUI-User-Id", "X-User-Email"}
	// identitySessionHeadersR378 — идентификатор сессии/чата в заголовке; в том
	// числе то, что OpenWebUI уже назначил сам.
	identitySessionHeadersR378 = []string{"X-Session-Id", "X-Chat-Id", "X-Conversation-Id"}
	// identityChatHeadersR378 — только чат (используется маршрутизацией, где
	// сессия и чат различаются).
	identityChatHeadersR378 = []string{"X-Chat-Id", "X-Chat-ID", "X-Conversation-Id", "X-Conversation-ID"}
	// identityRequestIDHeadersR378 — идентификатор ЗАПРОСА: последний рубеж для
	// ключа очереди, не сессия (R118).
	identityRequestIDHeadersR378 = []string{"X-Request-Id", "X-Request-ID"}
	// identityCookieNameR378 — cookie с уже назначенной сессией.
	identityCookieNameR378 = "session_id"
)

// requestUserIdentity — явный пользователь из заголовков (без префикса).
func requestUserIdentity(r *http.Request) string {
	if r == nil {
		return ""
	}
	if _, value := firstIdentityHeaderR378(r, identityUserHeadersR378); value != "" {
		return value
	}
	return ""
}

// firstIdentityHeaderR378 — первый непустой заголовок из списка: (имя, значение).
// Имя нужно ключу очереди, чтобы префикс показал оператору источник.
func firstIdentityHeaderR378(r *http.Request, headers []string) (string, string) {
	if r == nil {
		return "", ""
	}
	for _, h := range headers {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return h, v
		}
	}
	return "", ""
}

// firstStringField — первое непустое строковое поле из набора ключей.
func firstStringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// requestBodyIdentity — R67b: user/chat идентификаторы из тела запроса.
//
// Источники (по убыванию приоритета):
//   - top-level: user / user_id, chat_id / conversation_id / session_id;
//   - nested metadata: metadata.user_id, metadata.chat_id, metadata.session_id —
//     именно так OpenWebUI (OpenAI-совместимый режим) помечает запрос.
//
// Тело читается один раз (до 1 MiB) и ВОССТАНАВЛИВАЕТСЯ через io.NopCloser,
// так что вызывающий код может читать его снова.
func requestBodyIdentity(r *http.Request) (user, chat string) {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return "", ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if len(body) == 0 || len(body) > 1<<20 {
		return "", ""
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	user = firstStringField(req, "user", "user_id", "userId", "user_email")
	chat = firstStringField(req, "chat_id", "chatId", "conversation_id", "conversationId", "session_id")
	if md, ok := req["metadata"].(map[string]interface{}); ok {
		if user == "" {
			user = firstStringField(md, "user_id", "userId", "user", "user_email")
		}
		if chat == "" {
			chat = firstStringField(md, "chat_id", "chatId", "conversation_id", "conversationId", "session_id")
		}
	}
	return user, chat
}

// extractChatIDFromBody — идентификатор чата: заголовок (быстрее, без чтения
// тела) или чат из тела. Используется маршрутизацией: OpenWebUI различает чаты
// одного пользователя именно так (chat_id / conversation_id / session_id,
// в т.ч. внутри metadata).
func extractChatIDFromBody(r *http.Request) string {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return ""
	}
	if _, v := firstIdentityHeaderR378(r, identityChatHeadersR378); v != "" {
		return v
	}
	_, chat := requestBodyIdentity(r)
	return chat
}

// RequestSessionKey — R67a: идентификатор пользователя/сессии клиента (ключ
// admission-очереди и группировки в логах/метриках). Порядок источников:
//  1. X-User-Id / X-OpenWebUI-User-Id / X-User-Email — явный пользователь;
//  2. X-Session-Id / X-Chat-Id / X-Conversation-Id — идентификатор сессии/чата;
//  3. поле "user"/"user_id" в теле запроса (стандарт OpenAI) или "chat_id"/
//     "session_id" (Ollama), в том числе внутри metadata (OpenWebUI);
//  4. начало разговора (R118.9) — различает экземпляры клиента с одной машины;
//  5. X-Request-Id — идентификатор запроса, последний рубеж (R118);
//  6. "" — анонимный клиент (Cline и подобные не передают ни того, ни другого).
//
// Значение не аутентифицирует запрос — это только ключ группировки/справедливости.
func RequestSessionKey(r *http.Request, body []byte) string {
	if name, v := firstIdentityHeaderR378(r, identityUserHeadersR378); v != "" {
		return name + ":" + v
	}
	if name, v := firstIdentityHeaderR378(r, identitySessionHeadersR378); v != "" {
		return name + ":" + v
	}
	if len(body) > 0 {
		var parsed struct {
			User      string `json:"user"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
			ChatID    string `json:"chat_id"`
			Metadata  struct {
				UserID    string `json:"user_id"`
				ChatID    string `json:"chat_id"`
				SessionID string `json:"session_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			for _, v := range []string{parsed.User, parsed.UserID, parsed.Metadata.UserID} {
				if v = strings.TrimSpace(v); v != "" {
					return "user:" + v
				}
			}
			for _, v := range []string{parsed.SessionID, parsed.Metadata.SessionID} {
				if v = strings.TrimSpace(v); v != "" {
					return "session:" + v
				}
			}
			for _, v := range []string{parsed.ChatID, parsed.Metadata.ChatID} {
				if v = strings.TrimSpace(v); v != "" {
					return "chat:" + v
				}
			}
		}
	}
	// R118.9 (2026-10-14): различитель экземпляра клиента — начало разговора.
	// Ставим его ПОСЛЕ явных идентификаторов (пользователь/сессия/chat) и ПЕРЕД
	// X-Request-Id: в отличие от id запроса он стабилен внутри задачи, поэтому
	// admission-очередь видит два экземпляра Cline с одной машины как две разные
	// сессии, не теряя справедливость между ходами одной задачи. С id запроса
	// каждый ход получал бы НОВЫЙ ключ, то есть кошелёк справедливости не работал
	// бы вовсе (живой случай: одна задача, десять ходов, десять «сессий»).
	if conv := conversationFingerprint(body); conv != "" {
		return "conv:" + conv
	}
	// R118 (2026-10-14): X-Request-Id — идентификатор ЗАПРОСА, а не сессии, и
	// поэтому берётся ПОСЛЕДНИМ рубежом (после пользователя, сессии, chat_id и
	// начала разговора): устойчивый идентификатор всегда важнее.
	//
	// Зачем вообще. Клиент, который не называет ни пользователя, ни сессию, ни
	// разговор, но даёт уникальный id запроса, получает различимые строки в
	// очереди вместо одной слипшейся. Префикс `request:` виден в UI/статистике, так
	// что это не путается с настоящей сессией.
	//
	// На липкость к бэкенду (sessionMgr) это не влияет: там ключ считает
	// getSessionIDWithModel (client.go), а RequestSessionKey вызывается только как
	// fallback (proxy.go: sessionKey = sessionID, иначе RequestSessionKey) и в
	// llama.cpp admission-пути.
	if _, v := firstIdentityHeaderR378(r, identityRequestIDHeadersR378); v != "" {
		return "request:" + v
	}
	return ""
}

// mergeUniqueR378 — объединяет списки заголовков без повторов, сохраняя
// порядок. Нужен там, где список заголовков выводится из источников
// идентичности (проброс self-proxy): новый источник автоматически попадает и
// туда, и в ключи.
func mergeUniqueR378(lists ...[]string) []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, list := range lists {
		for _, h := range list {
			key := strings.ToLower(h)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, h)
		}
	}
	return out
}

// inferenceKeysR378 — оба ключа одного запроса, посчитанные вместе.
type inferenceKeysR378 struct {
	// SessionID — ключ маршрутизации и привязки к бэкенду (getSessionIDWithModel).
	SessionID string
	// QueueKey — ключ admission-очереди и кошелька справедливости.
	QueueKey string
	// QueueSource — чем является QueueKey для монитора (client/derived/none).
	QueueSource string
}

// resolveInferenceKeysR378 — R378: единственная точка, где для инференс-запроса
// считаются оба ключа. Хендлеры берут их отсюда, а не из двух разных функций:
// пока вызовы были раздельными, один путь мог передать в очередь ключ из одного
// набора источников, а в сессию — из другого.
func (p *Proxy) resolveInferenceKeysR378(r *http.Request, clientName, model string, body []byte) inferenceKeysR378 {
	queueKey := RequestSessionKey(r, body)
	out := inferenceKeysR378{
		QueueKey:    queueKey,
		QueueSource: SessionIdentitySource(queueKey),
	}
	if p != nil {
		out.SessionID = p.getSessionIDWithModel(r, clientName, model)
	}
	return out
}
