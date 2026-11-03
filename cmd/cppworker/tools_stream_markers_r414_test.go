// tools_stream_markers_r414_test.go — R414 (2026-11-03): служебные маркеры
// размышлений не должны утекать в видимый content потокового ответа.
//
// Живой случай (Cline в VS Code, gemma-4, /api/chat): последняя дельта
// заканчивалась на `<channel|>`, и клиент сохранил маркер в истории переписки.
package main

import (
	"strings"
	"testing"
)

// TestR414_MarkerScanFindsCloseMarker — точный живой случай: текст ответа,
// к которому прилип закрывающий маркер канала.
func TestR414_MarkerScanFindsCloseMarker(t *testing.T) {
	live := "…and finally run the check command.<channel|>"
	pos, mlen, ok := toolsStreamMarkerScanR414(live)
	if !ok {
		t.Fatalf("маркер <channel|> не найден в живом хвосте")
	}
	if want := strings.Index(live, "<channel|>"); pos != want {
		t.Fatalf("позиция маркера = %d, ожидалась %d", pos, want)
	}
	if mlen != len("<channel|>") {
		t.Fatalf("длина маркера = %d, ожидалась %d", mlen, len("<channel|>"))
	}
}

// TestR414_MarkerScanCoversAllReasoningTags — любой маркер из thinkTagPairs
// (открывающий и закрывающий) обязан находиться.
func TestR414_MarkerScanCoversAllReasoningTags(t *testing.T) {
	for _, pair := range thinkTagPairs {
		for _, m := range []string{pair.open, pair.close} {
			if m == "" {
				continue
			}
			pos, mlen, ok := toolsStreamMarkerScanR414("текст ответа " + m + " хвост")
			if !ok || pos != len("текст ответа ") || mlen != len(m) {
				t.Fatalf("маркер %q: pos=%d len=%d ok=%v", m, pos, mlen, ok)
			}
		}
	}
}

// TestR414_NoMarkerNoCut — обычный текст обязан проходить без изменений.
func TestR414_NoMarkerNoCut(t *testing.T) {
	if pos, _, ok := toolsStreamMarkerScanR414("обычный текст ответа"); ok || pos != -1 {
		t.Fatalf("найден маркер в обычном тексте: pos=%d ok=%v", pos, ok)
	}
	if hb := toolsStreamMarkerPrefixHoldbackR414("обычный текст ответа"); hb != 0 {
		t.Fatalf("удержан безопасный хвост: %d байт", hb)
	}
}

// TestR414_PartialMarkerHeldBack — недособранный маркер в хвосте удерживается.
func TestR414_PartialMarkerHeldBack(t *testing.T) {
	cases := map[string]int{
		"текст <chan":            len("<chan"),            // префикс <channel|>
		"текст <channel|":        len("<channel|"),        // префикс <channel|>
		"текст <|chan":           len("<|chan"),           // префикс <|channel>
		"текст </thi":            len("</thi"),            // префикс </think>
		"текст <|channel>though": len("<|channel>though"), // префикс <|channel>thought
		"обычный хвост заканчивается": 0,
	}
	for in, want := range cases {
		if got := toolsStreamMarkerPrefixHoldbackR414(in); got != want {
			t.Errorf("%q: удержано %d байт, ожидалось %d", in, got, want)
		}
	}
}

// TestR414_WiringToolsStreamStripsMarkers — сторож проводки: tools-путь
// (/api/chat + tools) обязан вызывать новые хелперы в месте выдачи content-дельты.
// Поведенчески проверить без живого bridge нельзя (callback замкнут на
// writeChatStreamResponseWithTools), поэтому проверяем исходник — как R404/R402.
func TestR414_WiringToolsStreamStripsMarkers(t *testing.T) {
	src := readMainSourceR402(t, "handlers_chat.go")
	for _, want := range []string{
		"toolsStreamMarkerPrefixHoldbackR414(delta)",
		"toolsStreamMarkerScanR414(delta)",
		"emittedLen += pos + markerLen",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("в handlers_chat.go нет %q: служебные маркеры снова утекут в content", want)
		}
	}
}
