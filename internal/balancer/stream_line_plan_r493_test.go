// stream_line_plan_r493_test.go — R493, этап 6: ЕДИНОЕ решение «что видит
// клиент» по строке апстрима (planStreamLineR493).
//
// ЗАЧЕМ ОТДЕЛЬНЫЕ ТЕСТЫ, КОГДА ЕСТЬ GOLDEN. Golden проверяет, что сквозной поток
// клиента не изменился. Здесь — само ПРАВИЛО: какие кадры, в каком порядке и
// почему. Без этого регресс вида «хвост фильтра ушёл раньше кадра-ошибки» ловится
// только на клиенте, который, прочитав объяснение, обрывает разбор.
package balancer

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// planForR493 — план для одной строки на заданном транспорте.
func planForR493(line string, egress *reasoningEgressState, tr streamLineTransportR493) streamLinePlanR493 {
	return planStreamLineR493([]byte(line), egress, nil, &toolRepairState{}, tr)
}

// TestStreamPlan_DataLineBoundaryDependsOnTransport — граница события: на
// chunked-транспорте она ДОПИСЫВАЕТСЯ в кадр (пустые строки там не пишутся), на
// обычном — приходит своей строкой и в кадр не добавляется.
func TestStreamPlan_DataLineBoundaryDependsOnTransport(t *testing.T) {
	const line = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n"

	chunked := planForR493(line, &reasoningEgressState{}, streamLineTransportChunkedR493)
	if !bytes.HasSuffix(chunked.Line, []byte("\n\n")) {
		t.Fatalf("chunked: кадр %q не несёт границу события «\\n\\n» — strict-клиент склеит события", chunked.Line)
	}

	plain := planForR493(line, &reasoningEgressState{}, streamLineTransportPlainR493)
	if string(plain.Line) != line {
		t.Fatalf("plain: строка изменена (%q → %q) — на этом транспорте границу даёт пустая строка", line, plain.Line)
	}
}

// TestStreamPlan_EmptyLineSkippedOnlyOnChunked — пустая строка-разделитель на
// chunked-транспорте не пишется вовсе (Round 35e: chunk из одного "\n" ломает
// strict chunked-разбор), а на обычном — пишется как есть (она и есть граница).
func TestStreamPlan_EmptyLineSkippedOnlyOnChunked(t *testing.T) {
	if p := planForR493("\n", &reasoningEgressState{}, streamLineTransportChunkedR493); p.Line != nil {
		t.Fatalf("chunked: пустая строка стала кадром %q — ломает strict-парсеры", p.Line)
	}
	if p := planForR493("\n", &reasoningEgressState{}, streamLineTransportPlainR493); p.Line == nil {
		t.Fatalf("plain: пустая строка-разделитель потеряна — события склеятся")
	}
}

// TestStreamPlan_ErrorFrameTerminatesOnBothTransports — R294: кадр-ошибка воркера
// закрывает поток на ЛЮБОМ транспорте. Раньше это правило жило только в
// hijack-цикле, и клиент /v1 без Hijack'а ждал EOF вместо готового ответа.
func TestStreamPlan_ErrorFrameTerminatesOnBothTransports(t *testing.T) {
	const errLine = "data: {\"error\":{\"message\":\"requested n_ctx=16384 exceeds\",\"code\":2}}\n"
	for _, tr := range []streamLineTransportR493{streamLineTransportPlainR493, streamLineTransportChunkedR493} {
		p := planForR493(errLine, &reasoningEgressState{}, tr)
		if !p.Terminate {
			t.Fatalf("транспорт %d: кадр-ошибки не закрывает поток (R294)", tr)
		}
		if p.Line == nil {
			t.Fatalf("транспорт %d: кадр-ошибки не доходит до клиента", tr)
		}
	}
	// Контроль: текст модели со словом error в content — НЕ повод закрывать поток.
	const contentLine = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the error is in line 12\"},\"finish_reason\":null}]}\n"
	for _, tr := range []streamLineTransportR493{streamLineTransportPlainR493, streamLineTransportChunkedR493} {
		if p := planForR493(contentLine, &reasoningEgressState{}, tr); p.Terminate {
			t.Fatalf("транспорт %d: обычный текст ответа закрыл поток — клиент потерял бы ответ", tr)
		}
	}
}

// TestStreamPlan_HeldFinishGoesBeforeDone — R261: придержанный finish-кадр
// отдаётся ПЕРЕД [DONE] (в Pre, а не после), иначе клиент видит кадр после конца
// потока.
func TestStreamPlan_HeldFinishGoesBeforeDone(t *testing.T) {
	egress := &reasoningEgressState{}
	// Пустой ответ, ушедший целиком в размышления: finish придерживается.
	held := planForR493(string(sseFrame("", "stop"))+"\n", egress, streamLineTransportChunkedR493)
	if held.Line != nil {
		t.Fatalf("finish-кадр должен быть придержан, а он ушёл клиенту: %q", held.Line)
	}

	// Теперь [DONE]: перед ним обязан уйти хвост фильтра.
	done := planForR493("data: [DONE]\n", egress, streamLineTransportChunkedR493)
	if len(done.Pre) == 0 {
		t.Fatalf("перед [DONE] не отдан хвост фильтра (R261/R258) — клиент не узнает, почему ответ пуст")
	}
	if done.Line == nil || !bytes.Contains(done.Line, []byte("[DONE]")) {
		t.Fatalf("сам [DONE] потерян: %q", done.Line)
	}
}

// TestStreamPlan_LegacyPathClosesOnUpstreamError — поведенческое следствие
// единого решения: legacy-цикл (без Hijack) ТОЖЕ закрывает поток по кадру-ошибке
// и отдаёт ровно один терминатор.
//
// ПОЧЕМУ ЭТОТ ТЕСТ ВАЖЕН. До этапа 6 R294 существовал только на hijack-пути:
// клиент /v1, попавший на legacy (HTTP/2, обёрнутый writer, тест), получал
// кадр-ошибку и продолжал ждать EOF. Тест закрепляет, что путь больше не решает
// это по-своему: он исполняет общий план.
func TestStreamPlan_LegacyPathClosesOnUpstreamError(t *testing.T) {
	p := r219Proxy(t)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"message\":\"requested n_ctx=16384 exceeds max\",\"code\":2}}\n\n")),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.proxyRequestOpenAIStreamingLegacy(rec, req, resp, "b1")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy-цикл не закрыл поток по кадру-ошибке (R294) — клиент ждал бы EOF")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "exceeds max") {
		t.Fatalf("кадр-ошибка не дошёл до клиента: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("терминаторов %d, ожидался ровно один (R252/R294): %q", n, body)
	}
}

// TestStreamPlan_ErrorTailComesAfterLineNotBefore — порядок записи: кадр-причина
// уходит ПЕРВЫМ, хвост фильтра — после него (Post). Обратный порядок заставил бы
// клиента прочитать объяснение раньше ошибки.
func TestStreamPlan_ErrorTailComesAfterLineNotBefore(t *testing.T) {
	egress := &reasoningEgressState{}
	// Сначала придержим finish, чтобы у фильтра был непустой хвост.
	_ = planForR493(string(sseFrame("", "length"))+"\n", egress, streamLineTransportChunkedR493)

	p := planForR493("data: {\"error\":\"worker failed\"}\n", egress, streamLineTransportChunkedR493)
	if !p.Terminate {
		t.Fatal("кадр-ошибка не закрыл поток")
	}
	if len(p.Post) == 0 {
		t.Fatal("хвост фильтра не отдан вместе с кадром-ошибкой — объяснение потеряно")
	}
	for _, fr := range p.Pre {
		if bytes.Contains(fr, []byte("finish_reason")) {
			t.Fatalf("хвост фильтра попал в Pre (до кадра-ошибки): %q", fr)
		}
	}
}
