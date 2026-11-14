// stream_hijack_golden_t2_r493_test.go — T2 для HIJACK-цикла (этап 6 плана «два
// мозга»): побайтовый контракт того, что видит клиент на /v1/chat/completions,
// когда балансер отдаёт ответ через Hijack.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ GOLDEN. T2 (stream_golden_t2_test.go) гоняет запросы через
// `httptest.NewRecorder()` — такой writer НЕ реализует http.Hijacker, поэтому все
// шесть его комбинаций идут по legacy-циклу (`proxyRequestOpenAIStreamingLegacy`).
// Основной же путь /v1 в живом стенде — hijack (`proxyRequestOpenAIStreamingHijacked`):
// именно там жили R252 (второй `[DONE]`), R277 (граница SSE-события), R294
// (кадр-ошибка воркера закрывает поток), R298 (сторож тишины). До этого файла
// hijack-цикл проверялся только ФОРМОЙ кадра (`TestR493r_HijackFrameMatchesLegacyChunkWriter`)
// и стражем записей — то есть «пишет через помощник», а не «пишет то же, что надо
// клиенту». Любая правка этого цикла (в т.ч. сведение циклов на этапе 6) не имела
// сети, которая поймала бы изменение байтов.
//
// КАК СНЯТЫ БАЙТЫ. Не через net/http-клиент: он де-чанкует и скрывает framing,
// который как раз и ломает strict-клиентов (OpenWebUI aiohttp). Здесь — сырой TCP:
// пишем запрос руками, читаем ответ, разбираем chunked сами и сверяем И кадры
// чанков, И их содержимое, И ровно один терминатор потока.
package balancer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hijackRawResultR493 — что клиент реально получил по сырому сокету.
//
// Порядок полей — под govet fieldalignment (govet enable-all в .golangci.yml):
// строки первыми, срез последним из указателесодержащих (у среза указатель —
// ПЕРВОЕ слово, поэтому префикс под сканирование заканчивается на нём), bool —
// в конце. Другой порядок даёт 56 pointer-байт вместо 40, и CI-линт это ловит.
type hijackRawResultR493 struct {
	Headers        string   // заголовки ответа как есть (без тела)
	Raw            string   // всё тело как есть (для диагностики)
	ChunkData      []string // содержимое кадров chunked по порядку
	HasZero        bool     // был ли финальный кадр "0\r\n\r\n"
	ClosedByServer bool     // закрыл ли сервер соединение сразу после терминатора
}

// Body — склеенное содержимое кадров (то, что увидит строгий клиент).
func (r hijackRawResultR493) Body() string { return strings.Join(r.ChunkData, "") }

// rawHijackRequestR493 — выполнить запрос по сырому TCP и разобрать chunked-ответ.
//
// ЗАПРОС БЕЗ `Connection: close` НАМЕРЕННО. Это и есть доказательство, что тест
// идёт по HIJACK-ветке, а не по legacy: hijack пишет ответ руками и закрывает
// соединение сам (`defer clientConn.Close()`), поэтому после `0\r\n\r\n` приходит
// EOF. Legacy-ветка отдала бы то же тело, но net/http оставил бы соединение
// живым (keep-alive) — и проверка `ClosedByServer` упала бы. Без этого отличия
// тест «зеленел» бы и на legacy, то есть не проверял бы тот цикл, ради которого
// написан.
func rawHijackRequestR493(t *testing.T, srv *httptest.Server, path, body string) hijackRawResultR493 {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\n\r\n%s", path, addr, len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(conn)

	// Заголовки — до пустой строки.
	var head strings.Builder
	for {
		line, rerr := br.ReadString('\n')
		head.WriteString(line)
		if rerr != nil {
			t.Fatalf("чтение заголовков: %v; получено: %q", rerr, truncR493(head.String(), 400))
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	res := hijackRawResultR493{Headers: strings.TrimRight(head.String(), "\r\n")}

	// Тело: кадры chunked до кадра нулевой длины.
	for {
		sizeLine, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatalf("чтение размера кадра: %v (заголовки: %q)", rerr, res.Headers)
		}
		sizeLine = strings.TrimRight(sizeLine, "\r\n")
		if i := strings.Index(sizeLine, ";"); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		size, perr := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if perr != nil {
			t.Fatalf("не разобрал размер кадра %q: %v (заголовки: %q)", sizeLine, perr, res.Headers)
		}
		if size == 0 {
			res.HasZero = true
			break
		}
		buf := make([]byte, size)
		if _, rerr := io.ReadFull(br, buf); rerr != nil {
			t.Fatalf("кадр объявлен %d байт, прочитано меньше: %v", size, rerr)
		}
		if _, rerr := br.ReadString('\n'); rerr != nil {
			t.Fatalf("нет CRLF после данных кадра: %v", rerr)
		}
		res.ChunkData = append(res.ChunkData, string(buf))
		res.Raw += sizeLine + "\r\n" + string(buf) + "\r\n"
	}

	// Доказательство пути: после терминатора соединение обязано закрыться.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	probe := make([]byte, 1)
	if _, rerr := conn.Read(probe); rerr == io.EOF {
		res.ClosedByServer = true
	}
	if !res.ClosedByServer {
		t.Fatalf("соединение НЕ закрыто сервером после терминатора: тест шёл не по hijack-ветке "+
			"(или регресс R219/R297). Заголовки: %q", res.Headers)
	}
	return res
}

func truncR493(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// hijackUpstreamR493 — фейковый воркер для hijack-пути. На инференс отдаёт
// переданный SSE-текст как есть (без разбора — это и есть «что прислал воркер»).
func hijackUpstreamR493(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" || r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"count":1,"models":[{"name":"m","path":"/app/models/m.gguf","state":"loaded","context_length":65536,"max_slots":1}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-upstream-1")
		fmt.Fprint(w, sse)
	}))
}

// TestHijackStreamGolden_ContentAndDone — базовый контракт hijack-цикла: кадры
// содержат ровно то, что прислал воркер, граница события сохранена (`\n\n`),
// `[DONE]` ровно один, chunked-терминатор (`0\r\n\r\n`) присутствует.
func TestHijackStreamGolden_ContentAndDone(t *testing.T) {
	upstream := hijackUpstreamR493(t,
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	defer upstream.Close()

	p := t2Proxy(t, upstream, true)
	srv := httptest.NewServer(p)
	defer srv.Close()

	res := rawHijackRequestR493(t, srv, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(res.Headers, "Transfer-Encoding: chunked") {
		t.Errorf("нет Transfer-Encoding: chunked — strict-клиенты (aiohttp) падают на framing:\n%s", res.Headers)
	}
	if strings.Contains(res.Headers, "keep-alive") {
		t.Errorf("обещан keep-alive, хотя соединение закрывается (R219):\n%s", res.Headers)
	}
	if !res.HasZero {
		t.Fatalf("нет финального chunked-кадра 0\\r\\n\\r\\n — aiohttp: «Not enough data to satisfy transfer length header»; тело: %q", res.Raw)
	}

	body := res.Body()
	want := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	if body != want {
		t.Fatalf("golden разошёлся.\nполучено: %q\nожидалось: %q", body, want)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("DONE %d раз(а), ожидался ровно один: %q", n, body)
	}
	// Пустые строки-разделители НЕ должны уходить отдельными кадрами (Round 35e):
	// кадр из одного "\n" ломает strict chunked-парсеры.
	for i, c := range res.ChunkData {
		if c == "\n" {
			t.Fatalf("кадр #%d — одиночный перевод строки; так ломается strict chunked-разбор (Round 35e)", i)
		}
	}
}

// TestHijackStreamGolden_SyntheticDoneWhenUpstreamEndsSilently — воркер закрыл
// поток БЕЗ [DONE]: клиент обязан получить синтетический терминатор (R252),
// иначе он считает ответ оборванным.
func TestHijackStreamGolden_SyntheticDoneWhenUpstreamEndsSilently(t *testing.T) {
	upstream := hijackUpstreamR493(t,
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
	defer upstream.Close()

	p := t2Proxy(t, upstream, true)
	srv := httptest.NewServer(p)
	defer srv.Close()

	res := rawHijackRequestR493(t, srv, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !res.HasZero {
		t.Fatalf("нет chunked-терминатора; тело: %q", res.Raw)
	}
	body := res.Body()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("DONE %d раз(а), ожидался ровно один (R252): %q", n, body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("кадр контента потерян: %q", body)
	}
}

// TestHijackStreamGolden_UpstreamErrorFrameClosesStream — кадр-ошибка от воркера
// (R294) обязан: дойти до клиента, получить ровно один терминатор и закрыть поток.
// Живой дефект: клиент /v1 висел до своего таймаута, хотя причина уже пришла.
func TestHijackStreamGolden_UpstreamErrorFrameClosesStream(t *testing.T) {
	upstream := hijackUpstreamR493(t,
		"data: {\"error\":{\"message\":\"requested n_ctx=16384 exceeds max\",\"code\":2}}\n\n")
	defer upstream.Close()

	p := t2Proxy(t, upstream, true)
	srv := httptest.NewServer(p)
	defer srv.Close()

	start := time.Now()
	res := rawHijackRequestR493(t, srv, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("поток не закрылся по кадру-ошибке: ждали %.1f с (R294)", elapsed.Seconds())
	}
	body := res.Body()
	if !strings.Contains(body, "exceeds max") {
		t.Fatalf("кадр-ошибка не дошёл до клиента: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("DONE %d раз(а) при кадре-ошибке, ожидался ровно один: %q", n, body)
	}
	if !res.HasZero {
		t.Fatalf("нет chunked-терминатора после кадра-ошибки: %q", res.Raw)
	}
}
