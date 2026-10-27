package main

import (
	"strings"
	"testing"
)

// R248/B3: путь проектора берётся ТОЛЬКО из явного поля запроса. Пустая
// строка/пробелы = vision не запрашивался (никакого угадывания рядом с моделью).
func TestR248_MmprojPathFromRequest(t *testing.T) {
	if got := mmprojPathFromRequest(nil); got != "" {
		t.Fatalf("nil → %q, ожидалось пусто", got)
	}
	empty := "   "
	if got := mmprojPathFromRequest(&empty); got != "" {
		t.Fatalf("пробелы → %q, ожидалось пусто", got)
	}
	p := "  /models/mmproj-SmolVLM-256M-Q8_0.gguf "
	if got := mmprojPathFromRequest(&p); got != "/models/mmproj-SmolVLM-256M-Q8_0.gguf" {
		t.Fatalf("путь обрезан неверно: %q", got)
	}
}

// R248/B3: reload не должен выключать vision. Явный путь из запроса выигрывает,
// иначе сохраняется уже загруженный проектор.
func TestR248_MmprojPathOrCurrent(t *testing.T) {
	current := "/models/mmproj-A.gguf"
	if got := mmprojPathOrCurrent(nil, current); got != current {
		t.Fatalf("без req должен сохраниться текущий: %q", got)
	}
	req := "/models/mmproj-B.gguf"
	if got := mmprojPathOrCurrent(&req, current); got != req {
		t.Fatalf("явный req должен выигрывать: %q", got)
	}
	empty := ""
	if got := mmprojPathOrCurrent(&empty, current); got != "" {
		t.Fatalf("явная пустая строка должна снимать проектор: %q", got)
	}
}

// R248/B3: capability-карта — переопределение оператора, не связанное с загрузкой.
func TestR248_VisionCapabilityMap(t *testing.T) {
	resetVisionCaps()
	defer resetVisionCaps()

	if modelVisionCapable("SmolVLM-256M") {
		t.Fatal("до явного включения capability должна быть false")
	}
	setModelVisionCapable("  SmolVLM-256M  ", true)
	if !modelVisionCapable("smolvlm-256m") {
		t.Fatal("capability должна учитывать регистр и пробелы")
	}
	setModelVisionCapable("SmolVLM-256M", false)
	if modelVisionCapable("SmolVLM-256M") {
		t.Fatal("capability должна сниматься")
	}
}

// R248/B3: имя модели не должно давать capability само по себе — только явный
// факт. Проверяем, что «похожие» имена остаются false.
func TestR248_VisionCapabilityNotInferredFromName(t *testing.T) {
	resetVisionCaps()
	defer resetVisionCaps()
	for _, name := range []string{"gemma-4-E4B-it-Q4_K_M", "qwen2.5-vl-3b", "llava-v1.6", "SmolVLM-256M"} {
		if modelVisionCapable(name) {
			t.Fatalf("%q: capability не должна выводиться из имени", name)
		}
		if strings.Contains(strings.ToLower(name), "mmproj") {
			t.Fatalf("тест сам по себе неверен: %q", name)
		}
	}
}
