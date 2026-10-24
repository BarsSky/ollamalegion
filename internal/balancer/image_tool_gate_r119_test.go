//go:build llama_stub

// image_tool_gate_r119_test.go — R119 (2026-10-15), пункт 2 плана.
//
// ЖИВОЙ ДЕФЕКТ. Прямой image-API гейт проходит и при нехватке VRAM отвечает
// `503 {code:"insufficient_vram", hint:…, freeMb, requiredMb}`. А ИНСТРУМЕНТ
// генерации картинок (`generate_image`), который балансер подкладывает в чат,
// гейт не проходил вообще: `generateImageForTool` резолвил цель, при нужде грузил
// модель и сразу POST-ил `/api/image/generate`. То есть путь, которым пользуется
// модель по просьбе пользователя, не проверял ни VRAM, ни эксклюзивный лок GPU, а
// ошибку возвращал как `{"status":"error","error":"<невнятный текст>"}` — без кода
// и без чисел. Модель не могла объяснить пользователю, чего не хватает.
//
// Тесты фиксируют: инструмент проходит тот же гейт, а отказ доезжает до модели
// машиночитаемо (код + числа + подсказка).
package balancer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR119_ImageTool_BlockedByVRAMGateWithNumbers — инструмент обязан быть
// остановлен тем же гейтом, что и прямой API, и вернуть код с числами.
func TestR119_ImageTool_BlockedByVRAMGateWithNumbers(t *testing.T) {
	// Рецепт отказа из TestImageGate_InsufficientVRAM: рабочему набору нужно
	// 1024 MB, а воркер сообщает 700 MB свободных.
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a red cat"})
	if err == nil {
		t.Fatalf("генерация прошла вопреки гейту: res=%v", res)
	}

	var toolErr *imageToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("ошибка не структурирована: %T %v — модель получит невнятный текст", err, err)
	}
	if toolErr.Code != types.ImageGateInsufficientVRAM {
		t.Errorf("code = %q, want %q", toolErr.Code, types.ImageGateInsufficientVRAM)
	}
	if toolErr.Hint == "" {
		t.Error("hint пуст: пользователю не сказано, что делать (OOM-лестница)")
	}
	payload := imageToolErrorPayload(err)
	if payload["code"] != types.ImageGateInsufficientVRAM {
		t.Errorf("payload.code = %v, want %q", payload["code"], types.ImageGateInsufficientVRAM)
	}
	if _, ok := payload["requiredMb"]; !ok {
		t.Errorf("payload без requiredMb: %v — модель не сможет сказать, СКОЛЬКО нужно", payload)
	}
	if _, ok := payload["freeMb"]; !ok {
		t.Errorf("payload без freeMb: %v — модель не сможет сказать, СКОЛЬКО свободно", payload)
	}
	if msg, _ := payload["error"].(string); !strings.Contains(msg, "MB") {
		t.Errorf("payload.error = %q, want числа в человеческой фразе", msg)
	}
	if gen, _ := stub.hits(); gen != 0 {
		t.Errorf("воркер получил %d запросов генерации: при отказе гейта запроса быть не должно", gen)
	}
}

// TestR119_ImageTool_AllowsAndReleasesLock — когда VRAM хватает, инструмент
// работает и ОТПУСКАЕТ лок GPU: иначе следующая генерация не пройдёт гейт.
func TestR119_ImageTool_AllowsAndReleasesLock(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenBody(`{"created":1,"model":"flux-test","output_format":"png","seed":7,"width":512,"height":512,"steps":8,"duration_ms":1500,"data":[{"index":0,"url":"/images/img_1_ab.png"}]}`)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}

	if _, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a red cat"}); err != nil {
		t.Fatalf("генерация не прошла: %v", err)
	}
	if gen, _ := stub.hits(); gen != 1 {
		t.Fatalf("воркер получил %d запросов, want 1", gen)
	}

	// Второй вызов обязан пройти: лок не утёк.
	if _, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a blue dog"}); err != nil {
		t.Fatalf("вторая генерация не прошла — лок GPU не освобождён: %v", err)
	}
}

// TestR119_ImageToolErrorPayload_GenericError — обычная ошибка остаётся прежней
// формы (не ломаем существующие потребители результата).
func TestR119_ImageToolErrorPayload_GenericError(t *testing.T) {
	payload := imageToolErrorPayload(errors.New("upstream вернул 500"))
	if payload["status"] != "error" {
		t.Errorf("status = %v, want error", payload["status"])
	}
	if payload["error"] != "upstream вернул 500" {
		t.Errorf("error = %v, want исходный текст", payload["error"])
	}
	if _, ok := payload["code"]; ok {
		t.Errorf("у обычной ошибки не должно быть машинного кода: %v", payload)
	}
}

// TestR119_ImageToolErrorPayload_Nil — nil не паникует.
func TestR119_ImageToolErrorPayload_Nil(t *testing.T) {
	payload := imageToolErrorPayload(nil)
	if payload["status"] != "error" {
		t.Errorf("status = %v, want error", payload["status"])
	}
}

// TestR119_ImageToolError_ErrorTextCarriesNumbers — текст ошибки (он же уходит в
// лог и в поле error) содержит числа и подсказку.
func TestR119_ImageToolError_ErrorTextCarriesNumbers(t *testing.T) {
	e := &imageToolError{
		Code:       types.ImageGateInsufficientVRAM,
		Message:    "image does not fit",
		Hint:       types.ImageOOMHint,
		RequiredMB: 4200, FreeMB: 900, HeadroomMB: 512,
	}
	msg := e.Error()
	for _, want := range []string{"4200", "900", "512", types.ImageOOMHint} {
		if !strings.Contains(msg, want) {
			t.Errorf("текст ошибки %q не содержит %q", msg, want)
		}
	}
}

// TestR119_ImageTool_GateDeniedIsStructuredForBothLoops — защита от отката: оба
// цикла (OpenAI и Ollama) обязаны отдавать структурированный payload, а не
// err.Error() напрямую.
func TestR119_ImageTool_GateDeniedIsStructuredForBothLoops(t *testing.T) {
	for _, file := range []string{"image_tool_loop.go", "image_tool_loop_ollama.go"} {
		raw, rerr := os.ReadFile(file)
		if rerr != nil {
			t.Fatalf("read %s: %v", file, rerr)
		}
		src := string(raw)
		if !strings.Contains(src, "imageToolErrorPayload(err)") {
			t.Errorf("%s больше не использует imageToolErrorPayload — код и числа "+
				"отказа гейта снова потеряются", file)
		}
		// Ветка «генерация не удалась» обязана идти через общий маппер, иначе
		// код и числа отказа гейта снова потеряются. (Ветка «некорректные
		// аргументы вызова» собирает результат сама — там кода гейта нет.)
	}
}

// TestR119_ImageTool_GenerationForToolUsesGate — защита от отката: путь инструмента
// обязан звать beforeGeneration.
func TestR119_ImageTool_GenerationForToolUsesGate(t *testing.T) {
	raw, rerr := os.ReadFile("image_tool.go")
	if rerr != nil {
		t.Fatalf("read image_tool.go: %v", rerr)
	}
	src := string(raw)
	if !strings.Contains(src, "beforeGeneration(ctx, resolved.BackendID)") {
		t.Error("generateImageForTool больше не проходит гейт ресурсов: инструмент обходит " +
			"проверку VRAM и эксклюзивный лок GPU")
	}
	if !strings.Contains(src, "defer gate.Release()") {
		t.Error("generateImageForTool не отпускает лок GPU — следующая генерация зависнет")
	}
	if !strings.Contains(src, "imageToolGateDenied(gate, model)") {
		t.Error("отказ гейта снова превращается в обычный fmt.Errorf — код и числа потеряются")
	}
}

// TestR119_ImageTool_HintIsActionable — подсказка должна быть действием, а не
// «попробуйте позже»: пользователь просит картинку, и ему нужно знать, ЧТО менять.
func TestR119_ImageTool_HintIsActionable(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target для image-инструмента")
	}
	_, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a cat"})
	var toolErr *imageToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("ожидалась структурированная ошибка, получено %v", err)
	}
	// Вердикт гейта обязан нести hint (ImageOOMHint) — проверяем, что он не потерян
	// по дороге в payload.
	if toolErr.Hint == "" {
		t.Error("hint потерян при превращении отказа гейта в ошибку инструмента")
	}
	if toolErr.RequiredMB <= 0 {
		t.Errorf("requiredMb = %d, want > 0 — иначе пользователю нечего уменьшать", toolErr.RequiredMB)
	}
}
