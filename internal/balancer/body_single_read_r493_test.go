// body_single_read_r493_test.go — R493, этап 2 плана «два мозга»: ОДИН буфер тела,
// ОДИН лимит, одинаковый 413, и тело читается не больше одного раза.
//
// ЗАЧЕМ ПРОВЕРКА, А НЕ ПРАВКА. Пункт плана («тело читается ≤1 раза; 413 одинаков»)
// числился «не начат», но R479/R480 уже свели чтение и лимит: generic-путь читает
// тело в parseRequestBody (с тем же лимитом), handler-пути — в
// readRequestBodyLimited, 413 в обоих случаях формирует одна функция
// sendBodyTooLarge. Правка «по плану» здесь означала бы переписать работающее,
// поэтому сначала замер. Тест закрепляет оба инварианта, чтобы будущая правка не
// вернула второе чтение (история: base64-картинки давали несколько полных копий
// тела в памяти).
//
// ЧТО ИМЕННО ИЗМЕРЯЕТСЯ И ЧЕГО ЗДЕСЬ НЕТ. Счётчик стоит на ИСТОЧНИКЕ тела, поэтому
// тест ловит «источник прочитан дважды». Повторное чтение уже восстановленного
// буфера (bytes.Buffer внутри r.Body) им НЕ наблюдается — это ограничение метода, а
// не доказанное отсутствие; проверено мутацией: вторая копия чтения восстановленного
// тела тест не роняет. Поэтому инвариант этапа 2 здесь сформулирован узко: «до
// воркера доходит всё тело, и источник читается один раз», а «одно буферизирование»
// подтверждается тем, что в пакете ровно одно место чтения тела на путь
// (parseRequestBody для generic, readRequestBodyLimited для handler-ов).
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingBodyR493 — тело, считающее сколько байт реально вычитано из источника.
type countingBodyR493 struct {
	r     io.Reader
	bytes int64 // atomic: читается из нескольких горутин
}

func (c *countingBodyR493) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(&c.bytes, int64(n))
	return n, err
}

func (c *countingBodyR493) Close() error { return nil }

// bodyEchoUpstreamR493 — воркер, который принимает тело и отвечает 200, чтобы
// запрос дошёл до конца (иначе «тело прочитано один раз» проверялось бы на пути,
// который оборвался до проксирования).
func bodyEchoUpstreamR493(t *testing.T, seen *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" || r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"count":1,"models":[{"name":"m","path":"/app/models/m.gguf","state":"loaded","context_length":65536,"max_slots":1}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		atomic.StoreInt64(seen, int64(len(raw)))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	}))
}

// TestBodyReadAtMostOnce_GenericPath — на generic-пути (/api/chat через ServeHTTP)
// источник тела вычитывается ровно один раз, и до воркера доходит весь объём.
func TestBodyReadAtMostOnce_GenericPath(t *testing.T) {
	var upstreamSaw int64
	up := bodyEchoUpstreamR493(t, &upstreamSaw)
	defer up.Close()
	p := t2Proxy(t, up, true)

	payload := `{"model":"m","stream":false,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", 200_000) + `"}]}`
	counter := &countingBodyR493{r: strings.NewReader(payload)}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req.Body = counter
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200; тело: %q", rec.Code, rec.Body.String())
	}
	read := atomic.LoadInt64(&counter.bytes)
	// «Не больше одного раза» — это ровно len(payload) байт из источника.
	// peekBodyR479 читает начало и отдаёт остаток через MultiReader, поэтому
	// повторного чтения из источника быть не должно.
	if read != int64(len(payload)) {
		t.Fatalf("из источника прочитано %d байт при теле %d — тело читается больше одного раза "+
			"(лишние копии в памяти на длинных историях с картинками)", read, len(payload))
	}
	if got := atomic.LoadInt64(&upstreamSaw); got != int64(len(payload)) {
		t.Fatalf("до воркера дошло %d байт вместо %d — тело искажено при восстановлении", got, len(payload))
	}
}

// TestBodyTooLarge_SameShapeOnGenericAndHandlerPath — 413 обязан быть ОДИН:
// generic-путь и handler-путь формируют его одной функцией (sendBodyTooLarge),
// иначе клиент получает то {"error":"..."}, то {"error":{"message":...}} в
// зависимости от того, каким путём пошёл запрос.
func TestBodyTooLarge_SameShapeOnGenericAndHandlerPath(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "1") // 1 МиБ

	var upstreamSaw int64
	up := bodyEchoUpstreamR493(t, &upstreamSaw)
	defer up.Close()
	p := t2Proxy(t, up, true)
	lr := NewLlamaCppRouter(p)

	big := `{"model":"m","stream":false,"messages":[{"role":"user","content":"` +
		strings.Repeat("y", 2<<20) + `"}]}`

	// 1. generic-путь.
	recGeneric := httptest.NewRecorder()
	reqGeneric := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(big))
	reqGeneric.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(recGeneric, reqGeneric)

	// 2. handler-путь (тот же путь /api/chat, тот же лимит).
	recHandler := httptest.NewRecorder()
	reqHandler := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(big))
	reqHandler.Header.Set("Content-Type", "application/json")
	lr.handleChat(recHandler, reqHandler)

	if recGeneric.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("generic-путь: статус %d, ожидался 413", recGeneric.Code)
	}
	if recHandler.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("handler-путь: статус %d, ожидался 413", recHandler.Code)
	}
	var bg, bh map[string]interface{}
	if err := json.Unmarshal(recGeneric.Body.Bytes(), &bg); err != nil {
		t.Fatalf("generic-путь: 413 не JSON: %v (%q)", err, recGeneric.Body.String())
	}
	if err := json.Unmarshal(recHandler.Body.Bytes(), &bh); err != nil {
		t.Fatalf("handler-путь: 413 не JSON: %v (%q)", err, recHandler.Body.String())
	}
	if fmt.Sprintf("%v", bg) != fmt.Sprintf("%v", bh) {
		t.Fatalf("тело 413 разное на двух путях:\n generic: %v\n handler: %v\n"+
			"клиент не должен зависеть от того, каким путём пошёл запрос", bg, bh)
	}
	// Главный инвариант лимита: запрос больше лимита до воркера не уходит вовсе.
	if got := atomic.LoadInt64(&upstreamSaw); got != 0 {
		t.Fatalf("тело больше лимита всё равно ушло воркеру (%d байт) — лимит не сработал", got)
	}
}

// TestBodyTooLarge_EnvelopeMatchesClientSurface — форма 413 выбирается по
// поверхности клиента (Ollama / OpenAI) и в пределах поверхности одинакова.
func TestBodyTooLarge_EnvelopeMatchesClientSurface(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "1")
	big := `{"model":"m","stream":false,"messages":[{"role":"user","content":"` +
		strings.Repeat("z", 2<<20) + `"}]}`

	check := func(path string, wantOpenAI bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(big)))
		req.Header.Set("Content-Type", "application/json")
		sendBodyTooLarge(rec, req)
		var m map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: 413 не JSON: %v (%q)", path, err, rec.Body.String())
		}
		_, isObj := m["error"].(map[string]interface{})
		if isObj != wantOpenAI {
			t.Fatalf("%s: форма ошибки не соответствует поверхности клиента: %v", path, m)
		}
	}
	check("/v1/chat/completions", true)
	check("/api/chat", false)
}
