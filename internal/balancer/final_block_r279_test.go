// final_block_r279_test.go — R279 (2026-10-28): финальный блок ответа не теряется.
//
// ЖИВОЙ ДЕФЕКТ (Cline, /api/chat). Модель сгенерировала финальный блок ответа
// (итоговый разбор с планом правок), а клиент получил вместо него размышления и
// маркер конца; сам блок исчез.
//
// Тесты фиксируют два места, где текст мог пропасть на пути к клиенту:
//  1. done-чанк cppworker'а: content вычищался ЦЕЛИКОМ, даже когда в нём был
//     НОВЫЙ текст, которого дельты не содержали (dedupeDoneContent);
//  2. придержанный хвост метки (markerHold): на конце стрима он уходил в
//     накопитель статистики, а не в поток (heldTailChunk + ApplyFinal).
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// r279DoneChunk собирает done-чанк cppworker'а с заданным content.
func r279DoneChunk(content string) string {
	chunk := map[string]interface{}{
		"model":       "qwen3.8:latest",
		"created_at":  "2026-10-28T10:00:00Z",
		"done":        true,
		"done_reason": "stop",
		"eval_count":  16,
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": content,
		},
	}
	b, _ := json.Marshal(chunk)
	return string(b)
}

// r279Content — content из NDJSON-строки ("" если поля нет).
func r279Content(t *testing.T, line string) string {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("строка не парсится: %v (%s)", err, line)
	}
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		c, _ := msg["content"].(string)
		return c
	}
	if r, ok := obj["response"].(string); ok {
		return r
	}
	return ""
}

// TestR279_DoneChunkKeepsNewTail — главный живой случай: дельты отдали прозу и
// рассуждения, а финальный блок пришёл ТОЛЬКО в done-чанке. Раньше он вычищался
// как «дубль», хотя дублем не был.
func TestR279_DoneChunkKeepsNewTail(t *testing.T) {
	streamed := "Размышляю над кодом диспетчера. "
	finalBlock := "\n\n## План правок\n1. Добавить мьютекс\n2. Убрать гонку"
	out := stripNativeDoneContent(r279DoneChunk(streamed+finalBlock), streamed)

	if got := r279Content(t, out); got != finalBlock {
		t.Fatalf("финальный блок потерян: content = %q, want %q", got, finalBlock)
	}
	// Статистика обязана остаться: ради неё done-чанк и нужен.
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("чанк не парсится: %v", err)
	}
	if _, ok := obj["eval_count"]; !ok {
		t.Error("eval_count потерян — клиент лишится статистики")
	}
	if done, _ := obj["done"].(bool); !done {
		t.Error("done больше не true")
	}
}

// TestR279_DoneChunkStillDeduplicates — полный дубль (обычный случай) по-прежнему
// вычищается: иначе клиенты, конкатенирующие чанки, получат ответ дважды.
func TestR279_DoneChunkStillDeduplicates(t *testing.T) {
	full := "Ответ целиком"
	out := stripNativeDoneContent(r279DoneChunk(full), full)
	if got := r279Content(t, out); got != "" {
		t.Fatalf("дубль не снят: content = %q", got)
	}
	// Тот же ответ, но с ведущим переводом строки в done-чанке (cppworker так
	// делает) — тоже дубль, а не новый текст.
	out = stripNativeDoneContent(r279DoneChunk("\n"+full), full)
	if got := r279Content(t, out); got != "" {
		t.Fatalf("дубль с ведущим \\n не снят: content = %q", got)
	}
}

// TestR279_DoneChunkKeepsUnknownContent — апстрим отдал в done-чанке текст, не
// связанный с дельтами. Раньше его выбрасывали (с WARN в лог). Терять реальный
// текст хуже, чем показать возможный дубль.
func TestR279_DoneChunkKeepsUnknownContent(t *testing.T) {
	out := stripNativeDoneContent(r279DoneChunk("Совсем другой текст"), "предыдущая проза")
	if got := r279Content(t, out); got != "Совсем другой текст" {
		t.Fatalf("неизвестный текст потерян: content = %q", got)
	}
}

// TestR279_DedupeDoneContentTable — таблица решений dedupeDoneContent.
func TestR279_DedupeDoneContentTable(t *testing.T) {
	cases := []struct {
		name     string
		done     string
		streamed string
		want     string
	}{
		{"ничего не отдано — done единственный носитель", "весь ответ", "", "весь ответ"},
		{"пробельные дельты не считаются отданным", "весь ответ", "   ", "весь ответ"},
		{"полный дубль", "весь ответ", "весь ответ", ""},
		{"дубль с ведущим переводом строки", "\nвесь ответ", "весь ответ", ""},
		{"хвост дельт + продолжение", "начало и конец", "начало и ", "конец"},
		{"общего нет", "другой текст", "первый текст", "другой текст"},
	}
	for _, c := range cases {
		if got := dedupeDoneContent(c.done, c.streamed); got != c.want {
			t.Errorf("%s: dedupeDoneContent(%q, %q) = %q, want %q",
				c.name, c.done, c.streamed, got, c.want)
		}
	}
}

// TestR279_HeldTailReachesClient — придержанный хвост (текст, похожий на начало
// метки) обязан уйти клиенту отдельным чанком, а не только в накопитель.
func TestR279_HeldTailReachesClient(t *testing.T) {
	hold := &markerHold{}
	// Строка заканчивается на «<» — удержание придержит этот символ.
	first := `{"done":false,"message":{"content":"Код: if (a <","role":"assistant"}}`
	gotFirst := filterNDJSONLineMarkers(first, hold)
	if c := r279Content(t, gotFirst); c != "Код: if (a " {
		t.Fatalf("видимый текст = %q, want %q", c, "Код: if (a ")
	}
	if hold.pending != "<" {
		t.Fatalf("хвост не придержан: pending = %q", hold.pending)
	}
	// Поток кончился без продолжения — хвост должен уйти клиенту.
	tail := hold.Flush()
	if tail != "<" {
		t.Fatalf("Flush вернул %q, want %q", tail, "<")
	}
	chunk := heldTailChunk("/api/chat", "qwen3.8:latest", tail)
	if chunk == nil {
		t.Fatal("heldTailChunk вернул nil — хвост не будет отдан клиенту")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(chunk, &obj); err != nil {
		t.Fatalf("чанк хвоста не парсится: %v", err)
	}
	if done, _ := obj["done"].(bool); done {
		t.Error("хвост помечен done:true — клиент получит второй конец потока")
	}
	if c := r279Content(t, string(chunk)); c != "<" {
		t.Errorf("content хвоста = %q, want %q", c, "<")
	}
}

// TestR279_ApplyFinalDoesNotHold — финальное применение не придерживает хвост
// заново (иначе последние байты блока снова потерялись бы).
func TestR279_ApplyFinalDoesNotHold(t *testing.T) {
	hold := &markerHold{}
	hold.pending = "<"
	got := hold.ApplyFinal("div>")
	if got != "<div>" {
		t.Fatalf("ApplyFinal = %q, want %q", got, "<div>")
	}
	if hold.pending != "" {
		t.Fatalf("ApplyFinal снова придержал: %q", hold.pending)
	}
}

// TestR279_DoneLineReleasesHeldTail — сквозной сценарий: текст разрезан так, что
// хвост придержан, и продолжение приходит ФИНАЛЬНОЙ строкой. Клиент должен
// получить весь текст, без потерь и без меток.
func TestR279_DoneLineReleasesHeldTail(t *testing.T) {
	hold := &markerHold{}
	l1 := `{"done":false,"message":{"content":"abc<","role":"assistant"}}`
	l2 := `{"done":true,"message":{"content":"def>","role":"assistant"}}`
	got1 := r279Content(t, filterNDJSONLineMarkers(l1, hold))
	got2 := r279Content(t, filterNDJSONLineMarkers(l2, hold))
	if got1+got2 != "abc<def>" {
		t.Fatalf("текст разрезан неверно: %q + %q", got1, got2)
	}
	if hold.pending != "" {
		t.Fatalf("после финальной строки остался придержанный хвост: %q", hold.pending)
	}
}

// TestR279_SplitMarkerInFinalLineIsStripped — та же схема, но склейка даёт
// служебную метку: она обязана быть вырезана, а текст вокруг — цел.
func TestR279_SplitMarkerInFinalLineIsStripped(t *testing.T) {
	hold := &markerHold{}
	l1 := `{"done":false,"message":{"content":"итог<|as","role":"assistant"}}`
	l2 := `{"done":true,"message":{"content":"sistant|> plan","role":"assistant"}}`
	got1 := r279Content(t, filterNDJSONLineMarkers(l1, hold))
	got2 := r279Content(t, filterNDJSONLineMarkers(l2, hold))
	if strings.Contains(got1+got2, "<|") || strings.Contains(got1+got2, "sistant") {
		t.Fatalf("метка утекла клиенту: %q", got1+got2)
	}
	if got1+got2 != "итог plan" {
		t.Fatalf("текст вокруг метки потерян: %q", got1+got2)
	}
}

// TestR279_HeldTailChunkShapes — форма чанка хвоста для обоих путей.
func TestR279_HeldTailChunkShapes(t *testing.T) {
	if got := heldTailChunk("/v1/chat/completions", "m", "<"); got != nil {
		t.Error("для /v1 чанк не нужен — там другой носитель (SSE)")
	}
	if got := heldTailChunk("/api/chat", "", ""); got != nil {
		t.Error("пустой хвост не должен порождать чанк")
	}
	gen := heldTailChunk("/api/generate", "m", "хвост")
	if gen == nil {
		t.Fatal("/api/generate: чанк не создан")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(gen, &obj); err != nil {
		t.Fatalf("/api/generate: чанк не парсится: %v", err)
	}
	if r, _ := obj["response"].(string); r != "хвост" {
		t.Errorf("/api/generate: response = %q, want %q", r, "хвост")
	}
}
