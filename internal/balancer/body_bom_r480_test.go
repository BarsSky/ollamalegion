// body_bom_r480_test.go — R480: BOM в начале тела запроса.
//
// Живой дефект, найденный при проверке стадий на стенде: тело, записанное
// Windows-инструментом (PowerShell 5.1 Set-Content -Encoding utf8 пишет BOM),
// балансер принимал, но:
//   - JSON не разбирался (model=""),
//   - stream-флаг терялся → запрос на /api/chat с "stream":true уходил в
//     НЕ-stream транспорт, и клиент получал ответ одним куском (при том что
//     воркер стримил) — ровно класс жалоб «токены шли, а стрима нет»;
//   - воркеру уезжало тело с BOM.
//
// Тесты держат три вещи: помощник снимает ровно три байта, оба пути чтения тела
// (generic peekBodyR479 и handler readRequestBodyLimited) отдают чистое тело, и
// разбор тела видит model/stream.
package balancer

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"
)

const bomJSONR480 = "\xef\xbb\xbf" + `{"model":"qwen3:0.6b","stream":true}`

func TestStripUTF8BOMR480(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"с BOM", bomJSONR480, bomJSONR480[3:]},
		{"без BOM", `{"model":"m"}`, `{"model":"m"}`},
		{"только BOM", "\xef\xbb\xbf", ""},
		{"короткое тело", "\xef", "\xef"},
		{"не BOM, похоже", "\xef\xbb\x00rest", "\xef\xbb\x00rest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripUTF8BOMR480([]byte(tc.in))); got != tc.want {
				t.Fatalf("stripUTF8BOMR480(%q) = %q, ожидалось %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadRequestBodyLimited_StripsBOM_R480 — handler-путь: тело чистое, и
// isStreamingFromBody видит stream:true (иначе /api/chat уходил в non-stream).
func TestReadRequestBodyLimited_StripsBOM_R480(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(bomJSONR480)))
	body, err := readRequestBodyLimited(req)
	if err != nil {
		t.Fatalf("readRequestBodyLimited: %v", err)
	}
	if bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("BOM остался в теле: %q", body)
	}
	if !isStreamingFromBody("/api/chat", body) {
		t.Fatalf("isStreamingFromBody не увидел stream:true в %q", body)
	}
}

// TestParseRequestBody_StripsBOM_R480 — generic-путь: model и stream разобраны,
// тело для проксирования тоже без BOM.
func TestParseRequestBody_StripsBOM_R480(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(bomJSONR480)))
	p := &Proxy{}
	parsed := p.parseRequestBody(req)
	if parsed.Model != "qwen3:0.6b" {
		t.Fatalf("parsed.Model = %q, ожидалось qwen3:0.6b (BOM сломал разбор)", parsed.Model)
	}
	if !parsed.Stream {
		t.Fatalf("parsed.Stream = false, ожидалось true")
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("чтение восстановленного тела: %v", err)
	}
	if bytes.HasPrefix(restored, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("в восстановленном теле остался BOM: %q", restored)
	}
}

// TestPeekBodyR479_StripsBOM_R480 — монитор «последние клиенты» и любое чтение
// начала тела видят модель, а не мусор.
func TestPeekBodyR479_StripsBOM_R480(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(bomJSONR480)))
	head, ok := peekBodyR479(req, 64<<10)
	if !ok {
		t.Fatal("peekBodyR479 вернул ok=false")
	}
	if bytes.HasPrefix(head, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("BOM остался в начале тела: %q", head)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("чтение восстановленного тела: %v", err)
	}
	// req.Body после peek — это ВСЁ тело (начало + остаток), а не только остаток.
	if string(rest) != bomJSONR480[3:] {
		t.Fatalf("тело после peek изменилось: head=%q body=%q", head, rest)
	}
}
