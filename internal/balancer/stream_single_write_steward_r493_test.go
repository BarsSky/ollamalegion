package balancer

// stream_single_write_steward_r493_test.go — R493-12r (этап 6): «в цикле стрима
// ОДНА точка записи клиенту».
//
// ЖИВОЙ ФАКТ, ради которого страж. Каждый цикл стрима писал клиенту по-своему
// (`fmt.Fprintf(w, …)`, `w.Write(…)`, `bufrw.WriteString(…)`, плюс свой
// `flusher.Flush()`), и расхождение видно клиенту: склеенные кадры, «замер» из-за
// забытого флаша, двойной `data: [DONE]` (R252). На NDJSON-пути и на hijack-пути
// запись теперь идёт через один сток (`streamSinkR493`), а на SSE-пути без hijack
// — через трекер терминатора `sseDoneTrackerR252`, который сам пишет, считает и
// флашит. Оба варианта — «одна точка записи»; стражи ниже фиксируют это
// конструктивно, чтобы следующая правка не добавила ещё одну запись «рядом».
//
// ПОЧЕМУ СТРАЖ, А НЕ ТЕСТ ПОВЕДЕНИЯ. Проверить «есть лишний flush» поведением
// нельзя: клиент получает те же байты, лишний флаш не наблюдаем. А вот лишний
// `w.Write` в обход счётчика/трекера — это ровно тот дефект, который ловится
// глазами в ревью и не ловится тестом. Поэтому правило проверяется по исходнику,
// как и остальные стражи серии (T4/T5).

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// funcBodyR493 — ЧАСТЬ тела функции после маркера: от строки с сигнатурой до
// следующей функции верхнего уровня, затем от маркера и до конца.
//
// ПОЧЕМУ НЕ ВСЁ ТЕЛО. До стрима в этих функциях есть законные записи не-стрима:
// кадр ошибки апстрима и тело не-SSE ответа. Они пишут один раз и не являются
// «второй точкой записи стрима», поэтому проверка начинается с маркера, который
// существует только в стриминговой части.
func funcBodyR493(t *testing.T, file, signature, marker string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("не прочитать %s: %v", file, err)
	}
	text := string(src)
	start := strings.Index(text, signature)
	if start < 0 {
		t.Fatalf("%s: не найдена сигнатура %q", file, signature)
	}
	rest := text[start+len(signature):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}
	if marker != "" {
		at := strings.Index(rest, marker)
		if at < 0 {
			t.Fatalf("%s: в функции нет маркера %q — страж смотрит не туда", file, marker)
		}
		rest = rest[at:]
	}
	return rest
}

// TestR493r_SSELoopWritesOnlyThroughTheSink — общий SSE-цикл пишет клиенту только
// через сток (run.writeFrame), а не напрямую в ResponseWriter.
//
// ПОЧЕМУ СТРАЖ ПЕРЕЕХАЛ. До этапа 6 у каждого транспорта был свой цикл, и страж
// смотрел в оба тела. Теперь цикл ОДИН (stream_openai_sse.go): если в нём
// появится прямая запись, она обойдёт и счётчик, и флаш, и терминатор — то есть
// вернёт ровно тот класс дефектов, ради которого этап 6 и делался.
func TestR493r_SSELoopWritesOnlyThroughTheSink(t *testing.T) {
	body := funcBodyR493(t, "stream_openai_sse.go",
		"func (p *Proxy) runOpenAISSEStreamR493(", "")

	if !strings.Contains(body, "run.writeFrame(") {
		t.Fatal("цикл не пишет через сток run.writeFrame — значит у него снова своя точка записи")
	}
	forbidden := []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`(^|[^A-Za-z0-9_.])w\.Write\(`),
			"запись мимо стока: счётчик/терминатор/флаш не увидят кадр"},
		{regexp.MustCompile(`fmt\.Fprintf\(w,`),
			"запись мимо стока через формат"},
		{regexp.MustCompile(`flusher\.Flush\(\)`),
			"второй набор правил флаша поверх стока"},
	}
	for _, f := range forbidden {
		if f.re.MatchString(body) {
			t.Errorf("в общем SSE-цикле найдено %q (%s): клиентская запись обязана идти "+
				"через ОДНУ точку (run.writeFrame)", f.re.String(), f.why)
		}
	}
}

// TestR493r_HijackTransportOwnsFraming — обрамление chunked-кадра принадлежит
// стоку, а не открывателю транспорта: там только решение «перехватить или нет» и
// заголовки.
func TestR493r_HijackTransportOwnsFraming(t *testing.T) {
	body := funcBodyR493(t, "stream_openai_sink.go",
		"func (p *Proxy) openOpenAIStreamSinkR493(", "")

	if !strings.Contains(body, "newHijackStreamSinkR493(") {
		t.Fatal("перехваченное соединение не заводит hijack-сток — обрамление кадра " +
			"снова окажется в цикле")
	}
	for _, forbidden := range []string{"writeChunkedFrame(", "writeChunkedString("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("в открывателе транспорта найдено ручное обрамление %q — "+
				"форма кадра обязана быть делом стока", forbidden)
		}
	}
}

// TestR493r_StreamLinePlanIsTheOnlyDecisionPoint — этап 6: решение «что видит
// клиент» принимается в ОДНОМ месте (planStreamLineR493), а общий цикл только
// пишет решённое.
//
// ЖИВОЙ ДЕФЕКТ, ради которого страж. В двух циклах дословно дублировались фильтр
// размышлений, нормализация формы вызова (R186), придержанный finish (R261),
// граница события (R277) и закрытие по кадру-ошибке (R294). Расхождение было не
// теорией: R294 существовал ТОЛЬКО на hijack-пути, поэтому клиент /v1 без
// Hijack\'а получал кадр-ошибку и продолжал ждать EOF.
//
// Почему страж, а не тест поведения: покрыть все комбинации поведением можно, но
// «следующая правка в одном цикле» поведением не ловится — обе ветки по
// отдельности остаются корректными, расходятся только реакции на редкий кадр.
func TestR493r_StreamLinePlanIsTheOnlyDecisionPoint(t *testing.T) {
	// R493 (2026-11-14): к этому списку добавлен авто-стрим R258 — он был ЧЕТВЁРТОЙ
	// копией правила и уже разошёлся (не приводил форму tool_call и не закрывал
	// поток по кадру-ошибке).
	loops := map[string]string{
		"stream_openai_sse.go":                "func (p *Proxy) runOpenAISSEStreamR493(",
		"proxy_request_openai_auto_stream_limits.go": "func (p *Proxy) streamUpstreamSSEToClient(",
	}
	for file, sig := range loops {
		checkSSEPipelineStewardR493(t, file, sig)
	}
}

// checkSSEPipelineStewardR493 — правило для одного стримингового цикла.
func checkSSEPipelineStewardR493(t *testing.T, file, sig string) {
	t.Helper()
	loop := funcBodyR493(t, file, sig, "")
	if !strings.Contains(loop, "planStreamLineR493(") {
		t.Error("общий цикл не использует planStreamLineR493 — значит у него снова своя " +
			"копия правила (класс дефекта этапа 6)")
	}
	if !strings.Contains(loop, "finalizeStreamFramesR493(") {
		t.Error("общий цикл не закрывает стрим через finalizeStreamFramesR493")
	}
	forbidden := []struct {
		needle string
		why    string
	}{
		{"filterAssistantEgressForClient(", "фильтр размышлений решается в плане"},
		{"normalizeOpenAISSEPayload(", "форма вызова (R186) решается в плане"},
		{"tryHoldFinishFrame(", "придержанный finish (R261) решается в плане"},
		{"isUpstreamErrorPayload(", "закрытие по кадру-ошибке (R294) решается в плане"},
		{"withSSEEventBoundary(", "граница события (R277) восстанавливается в плане"},
		{"isSSEDoneLine(", "момент выдачи терминатора решается в плане"},
		{"finalizeFrames(", "хвост стрима отдаётся через finalizeStreamFramesR493"},
	}
	for _, f := range forbidden {
		if strings.Contains(loop, f.needle) {
			t.Errorf("в общем цикле снова вызывается %q (%s). Правило обязано быть в "+
				"planStreamLineR493: иначе правка доедет до одного транспорта из двух — "+
				"ровно так R294 годами работал только на hijack-пути", f.needle, f.why)
		}
	}

	// Обратная сторона: само правило обязано содержать все шаги. Если кто-то
	// «упростит» план, выкинув нормализацию или придержанный finish, это будет
	// видно здесь, а не у клиента.
	plan := funcBodyR493(t, "stream_line_plan.go", "func planStreamLineR493(", "")
	for _, needle := range []string{
		"filterAssistantEgressForClient(", "normalizeOpenAISSEPayload(",
		"tryHoldFinishFrame(", "isUpstreamErrorPayload(", "finalizeStreamFramesR493(",
	} {
		if !strings.Contains(plan, needle) {
			t.Errorf("в planStreamLineR493 нет шага %q — правило стало неполным", needle)
		}
	}
}

// TestR493r_StreamPipeFlagIsRegistered — флаг обязан быть в реестре горячих
// настроек (урок 0.7.261: он был «мёртвым», а тесты «при 0 и при 1» проходили оба).
func TestR493r_StreamPipeFlagIsRegistered(t *testing.T) {
	t.Setenv("LB_STREAM_PIPE", "1")
	if !streamPipeEnabledR493() {
		t.Fatal("LB_STREAM_PIPE=1 не включает сток: настройка не зарегистрирована или " +
			"читается не тем путём — прогон «со стоком» был бы тем же прогоном без стока")
	}
	t.Setenv("LB_STREAM_PIPE", "0")
	if streamPipeEnabledR493() {
		t.Fatal("LB_STREAM_PIPE=0 не выключает сток")
	}
}
