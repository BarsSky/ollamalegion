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

// TestR493r_LegacySSELoopWritesOnlyThroughTracker — цикл SSE без hijack пишет
// клиенту только через трекер терминатора (он же считает байты и флашит).
func TestR493r_LegacySSELoopWritesOnlyThroughTracker(t *testing.T) {
	body := funcBodyR493(t, "proxy_request.go",
		"func (p *Proxy) proxyRequestOpenAIStreamingLegacy(",
		"tw := newSSEDoneTrackerR252(")

	if !strings.Contains(body, "tw.Write(") {
		t.Fatal("цикл не пишет через трекер tw — значит учёт терминатора (R252) не работает")
	}
	// ВНИМАНИЕ: проверка регуляркой, а не Contains. Подстрока "w.Write(" есть
	// внутри "tw.Write(" — первая версия стража ловила саму разрешённую запись
	// (ложное срабатывание, которое я и получил).
	forbidden := []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`(^|[^A-Za-z0-9_.])w\.Write\(`),
			"запись мимо трекера: R252 не увидит [DONE]"},
		{regexp.MustCompile(`fmt\.Fprintf\(w,`),
			"запись мимо трекера через формат"},
		{regexp.MustCompile(`flusher\.Flush\(\)`),
			"второй набор правил флаша поверх трекера"},
	}
	for _, f := range forbidden {
		if f.re.MatchString(body) {
			t.Errorf("в цикле SSE без hijack найдено %q (%s): клиентская запись обязана идти "+
				"через ОДНУ точку (tw.Write). Иначе счётчик/терминатор/флаш снова разъедутся — "+
				"это и есть класс дефектов этапа 6", f.re.String(), f.why)
		}
	}
}

// TestR493r_HijackSSELoopWritesOnlyThroughHelpers — hijack-цикл пишет только через
// writeFrameR493/writeKeepaliveR493 (сток или исторический писатель).
func TestR493r_HijackSSELoopWritesOnlyThroughHelpers(t *testing.T) {
	body := funcBodyR493(t, "proxy_request_hijack.go",
		"func (p *Proxy) proxyRequestOpenAIStreamingHijacked(",
		"writeFrameR493 := func(")

	if !strings.Contains(body, "writeFrameR493(") {
		t.Fatal("hijack-цикл не использует общий хелпер записи кадра")
	}
	forbidden := []string{
		"writeChunkedFrame(bufrw, fr)",
		"writeChunkedString(bufrw, string(fr))",
		"bufrw.Write(",
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Errorf("в hijack-цикле найдена прямая запись %q — кадры обязаны идти "+
				"через writeFrameR493 (одно правило обрамления)", f)
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
