package balancer

// preflight_dialog_frames_r493_test.go — этап 6, последний остаток: диалог
// preflight не заводит СВОЮ копию правил по кадрам.
//
// ЧТО ЭТОТ ФАЙЛ ЗАКРЫВАЕТ. preflight_stream_dialog.go (Round 34) — не пятая
// копия решения о кадрах: он не строит кадры из токенов, а проксирует байты
// апстрима через себя (self-proxy), пока идёт перезагрузка под окно. Его
// собственная часть — транспорт: ручной chunked-ответ на hijack'нутом
// соединении, keepalive в окне перезагрузки, терминатор по последней строке.
// Но правило границы SSE-события (R277) и набор кадров закрытия выхода (R264)
// жили здесь ВТОРОЙ копией — и разошлись бы с основными путями молча.
//
// Теперь кадры закрытия и граница берутся из finalizeStreamFramesR493, а здесь
// остаётся только «записать и запомнить последнюю строку». Страж фиксирует это
// конструктивно: повторная копия правила падает на страте, а не через полгода в
// проде.
//
// ПОЧЕМУ СТРАЖ, А НЕ ПОВЕДЕНЧЕСКИЙ ТЕСТ. Поведение диалога уже покрыто
// (preflight_egress_r264_test.go, preflight_agent_window_r253_test.go), и там
// отличие «своя копия правила» от «общая функция» не видно: байты те же. Видно
// его только в исходнике — как и остальные стражи серии R493.

import (
	"regexp"
	"strings"
	"testing"
)

// TestR493r_DialogFinalizeUsesSharedFrames — кадры закрытия выхода в диалоге
// обязаны приходить из общего места.
func TestR493r_DialogFinalizeUsesSharedFrames(t *testing.T) {
	body := funcBodyR493(t, "preflight_stream_dialog.go",
		"func writeEgressFinalizeFrames(egress *reasoningEgressState", "")

	if !strings.Contains(body, "finalizeStreamFramesR493(egress)") {
		t.Fatal("writeEgressFinalizeFrames не зовёт finalizeStreamFramesR493 — значит у диалога " +
			"снова СВОЯ копия набора кадров закрытия выхода (разойдётся с основными путями молча)")
	}
	for _, f := range []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`egress\.finalizeFrames\(\)`),
			"свой вызов finalizeFrames обходит общее правило кадров"},
		{regexp.MustCompile(`withSSEEventBoundary\(`),
			"своя расстановка границы SSE-события (R277) — правило должно быть в одном месте"},
	} {
		if f.re.MatchString(body) {
			t.Errorf("в writeEgressFinalizeFrames найдено %q: %s", f.re.String(), f.why)
		}
	}
}

// TestR493r_DialogHasNoOwnStreamDecision — диалог не должен разбирать строки
// потока и решать их судьбу: он проксирует апстрим как есть. Если здесь
// появится planStreamLineR493 — это значит, что диалог начал строить кадры
// сам, то есть стал ещё одним циклом стрима со своим решением.
func TestR493r_DialogHasNoOwnStreamDecision(t *testing.T) {
	body := funcBodyR493(t, "preflight_stream_dialog.go",
		"func selfProxyEgressLine(line []byte", "")

	if strings.Contains(body, "planStreamLineR493(") {
		t.Error("диалог строит кадры сам (planStreamLineR493): он обязан проксировать байты, " +
			"иначе появляется второй цикл со своим решением о кадрах")
	}
	// isSSEDoneLine — законная проверка «терминатор уже был»: диалог не может
	// знать счётчик стока, он видит только последнюю записанную строку.
	if !strings.Contains(body, "isSSEDoneLine(") {
		t.Error("в selfProxyEgressLine нет проверки isSSEDoneLine — без неё возможен " +
			"второй [DONE] или закрытие выхода после терминатора")
	}
}
