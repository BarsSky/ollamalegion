//go:build llama_stub

// auto_continue_duplicate_api_r66d_test.go — R66d (2026-09-23).
//
// ВОСПРОИЗВЕДЕНИЕ НА API (жалоба): «при запуске qwen3.8 на NVIDIA A10 без
// reasoning ответ клиенту OpenWebUI пришёл продублированным слово-в-слово».
//
// Сценарий целиком проходит через HTTP-транспорт балансера
// (proxyRequestLlamaCpp), как реальный запрос OpenWebUI/Cline:
//  1. Клиент шлёт streaming /api/chat.
//  2. Мок cppworker отдаёт ответ, обрезанный на середине (незакрытый ```-блок),
//     но с корректным завершением стрима ([DONE]) — то есть обрыв на стороне
//     МОДЕЛИ, а не транспорта.
//  3. Балансер с LB_AUTO_CONTINUE_ON_TRUNCATION=1 детектит обрыв
//     (TruncateReason → unclosed_code_block) и делает continue-запрос.
//  4. Мок на continue отвечает ПЕРЕГЕНЕРАЦИЕЙ: тем же ответом с начала плюс
//     новый текст (типичный chat-антипаттерн: модель перезапускает ответ).
//
// ОЖИДАНИЕ ПОСЛЕ ФИКСА: continuation подавлен (политика smart + similarity),
// клиент получает ровно исходный (обрезанный) текст — БЕЗ дубля. При этом
// continue-запрос РЕАЛЬНО уходил (2 запроса), иначе тест был бы no-op.
//
// До фикса (проверено мутацией — временное отключение
// isRegenerationBySimilarity) клиент получал исходный текст + перегенерацию,
// то есть тот самый «дубликат слово-в-слово».
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// origTruncatedR66d — ответ, обрезанный на середине: одна ``` (непарная) →
	// TruncateReason вернёт unclosed_code_block. Длина > 120 рун, чтобы
	// similarity-проверка вообще применялась.
	origTruncatedR66d = "Столица Франции — Париж. Город расположен на реке Сена, " +
		"в северной части страны. Население агломерации превышает 12 миллионов человек.\n" +
		"```go\nfunc main() {\n\tfmt.Println(\"start\")"

	// continuationMarkerR66d — уникальная строка, которой НЕТ в оригинале.
	continuationMarkerR66d = "И ДАЛЕЕ МОДЕЛЬ ПРОДОЛЖАЕТ УЖЕ НОВЫМ ТЕКСТОМ"

	// regeneratedR66d — перегенерация: тот же ответ с начала + новый текст.
	regeneratedR66d = origTruncatedR66d + "\n\t}\n}\n```\n" + continuationMarkerR66d
)

// makeRegeneratingUpstreamR66d — мок cppworker:
//   - streaming-запрос (stream=true)  → SSE с origTruncatedR66d и завершением [DONE];
//   - continue-запрос (stream=false)  → нестриминговый ответ с regeneratedR66d.
//
// Возвращает сервер и счётчик запросов к inference-эндпоинту.
func makeRegeneratingUpstreamR66d(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var inferReqs int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/chat") && !strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		atomic.AddInt32(&inferReqs, 1)

		var parsed struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &parsed)

		if !parsed.Stream {
			// Continue-запрос (PerformAutoContinue форсит stream=false и ждёт
			// OpenAI-подобный ответ с choices[0].message.content).
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"message": map[string]interface{}{"content": regeneratedR66d},
					},
				},
				"usage": map[string]interface{}{"completion_tokens": 64},
			})
			return
		}

		// Основной streaming-ответ: SSE-чанки cppworker (балансер транслирует их
		// в NDJSON для native /api/chat клиента).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		half := len(origTruncatedR66d) / 2
		for _, part := range []string{origTruncatedR66d[:half], origTruncatedR66d[half:]} {
			chunk := map[string]interface{}{
				"id":      "chatcmpl-r66d",
				"object":  "chat.completion.chunk",
				"created": 1700000000,
				"model":   "Qwen3.8-27B",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{"content": part},
						"finish_reason": nil,
					},
				},
			}
			payload, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(payload)
			_, _ = w.Write([]byte("\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Корректное завершение стрима: обрыв не транспортный, а модельный.
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &inferReqs
}

// TestAutoContinue_RegenerationNotDuplicated_APIR66d — воспроизведение и
// проверка фикса дублирования ответа на уровне HTTP-транспорта балансера.
func TestAutoContinue_RegenerationNotDuplicated_APIR66d(t *testing.T) {
	// Авто-продолжение включено, политика — default "smart" (LB_AUTO_CONTINUE_CHAT_POLICY не задаём).
	t.Setenv("LB_AUTO_CONTINUE_ON_TRUNCATION", "1")
	t.Setenv("LB_AUTO_CONTINUE_CHAT_POLICY", "")

	upstream, inferReqs := makeRegeneratingUpstreamR66d(t)
	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model": "Qwen3.8-27B",
		"messages": []interface{}{
			map[string]string{"role": "user", "content": "Расскажи про Париж и покажи пример кода"},
		},
		"stream": true,
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp вернул ошибку: %v", err)
	}

	// Собираем весь контент, который получил клиент.
	var clientContent strings.Builder
	for _, chunk := range parseNDJSONResponse(t, bytes.NewReader(rec.Body.Bytes())) {
		if msg, ok := chunk["message"].(map[string]interface{}); ok {
			if c, ok := msg["content"].(string); ok {
				clientContent.WriteString(c)
			}
		}
	}
	got := clientContent.String()

	// 1. Путь авто-продолжения действительно отработал: было 2 запроса
	//    (основной streaming + continue). Иначе тест ничего не проверяет.
	if n := atomic.LoadInt32(inferReqs); n != 2 {
		t.Fatalf("к cppworker ушло %d inference-запросов, ожидалось 2 "+
			"(основной + continue) — сценарий авто-продолжения не воспроизведён", n)
	}

	// 2. Клиент получил исходный (обрезанный) текст.
	if !strings.Contains(got, "Столица Франции") {
		t.Fatalf("клиент не получил исходный текст; content=%q", got)
	}

	// 3. ДУБЛИКАТА БЫТЬ НЕ ДОЛЖНО: перегенерация (её уникальный маркер)
	//    не должна попасть в ответ клиенту.
	if strings.Contains(got, continuationMarkerR66d) {
		t.Errorf("ДУБЛИКАТ: клиент получил перегенерацию после обрыва (continuation-маркер присутствует).\n"+
			"content=%q", got)
	}

	// 4. Исходный текст не должен быть повторён дважды.
	if n := strings.Count(got, "Столица Франции — Париж"); n != 1 {
		t.Errorf("исходный текст встречается %d раз (ожидалось 1) — ответ продублирован.\ncontent=%q", n, got)
	}
}
