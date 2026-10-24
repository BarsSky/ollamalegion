// auto_continue_native_accum_r118_17_test.go — R118.17 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд, мастер). Ответ gemma-4-E4B-it-Q4_K_M, обрезанный капом
// (num_predict=48), выглядел у клиента так:
//
//	```python
//	import numpy as np
//	...
//	    def __init__(self, mass, initial_speed, launch_angle
//
// то есть незакрытый блок кода — ровно случай, ради которого существует
// авто-продолжение (LB_AUTO_CONTINUE_ON_TRUNCATION=1, R60.21). Оно НЕ
// срабатывало: счётчики /api/v1/metrics (emitted/suppressed/failed) оставались
// нулями, в логе нет строки «R60.21 detected truncation».
//
// Причина: на нативном пути cppworker отдаёт текст дважды — дельтами и полным
// текстом в финальном done-чанке (клиенту дубль снимает stripNativeDoneContent),
// а накопление приклеивало done-текст к уже накопленным дельтам. Детектор получал
// 147+147 символов: число ``` удваивалось (становилось чётным) и последняя строка
// выглядела «полной» → TruncateReason возвращал "".
//
// Тесты ниже фиксируют правило накопления и — главное — что детектор обрыва
// срабатывает на том, что реально увидел клиент.
package balancer

import (
	"strings"
	"testing"
)

// Живой фрагмент (начало ответа из лога стенда).
const liveTruncatedHead = "```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n\nclass Projectile:\n" +
	"    \"\"\"Represents a projectile.\"\"\"\n    def __init__(self, mass, initial_speed, launch_angle"

func TestR118_17_NativeAccumulatorDoesNotDoubleDoneChunk(t *testing.T) {
	// Дельтами ушло начало ответа.
	acc := appendNativeContent("", false, "```python\nimport numpy as np\n")
	if acc != "```python\nimport numpy as np\n" {
		t.Fatalf("первый чанк накоплен неверно: %q", acc)
	}
	// Второй чанк-дельта.
	acc = appendNativeContent(acc, false, "import matplotlib.pyplot as plt\n")
	// done-чанк несёт ТОТ ЖЕ полный текст (так делает cppworker).
	acc = appendNativeContent(acc, true, "```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n")

	if got := strings.Count(acc, "```python"); got != 1 {
		t.Errorf("done-текст приклеился к дельтам: ```python встречается %d раз (ожидался 1)\n%s",
			got, acc)
	}
	if acc != "```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n" {
		t.Errorf("накопленное = %q", acc)
	}
}

func TestR118_17_NativeAccumulatorKeepsAnswerFromDoneChunkOnly(t *testing.T) {
	// R91: апстрим отдал весь ответ ОДНИМ финальным чанком — терять его нельзя.
	acc := appendNativeContent("", true, "целый ответ без дельт")
	if acc != "целый ответ без дельт" {
		t.Errorf("ответ из done-чанка потерян: %q", acc)
	}
}

func TestR118_17_NativeAccumulatorAppendsGenuinelyNewDoneText(t *testing.T) {
	// Апстрим сменил семантику: в done-чанке текст, которого не было в дельтах.
	acc := appendNativeContent("начало ответа", true, " а это хвост, которого не было")
	if !strings.Contains(acc, "хвост") {
		t.Errorf("новый текст из done-чанка потерян: %q", acc)
	}
}

// TestR118_17_TruncationDetectedOnClientVisibleText — суть дефекта: детектор
// обязан видеть обрыв на том тексте, который получил клиент.
// TestR118_17_ContinueResponseParsedInBothSurfaceFormats — вторая половина того
// же дефекта: продолжение запрашивается по URL исходного запроса, поэтому ответ
// приходит в формате ЕГО поверхности, а разбирался только OpenAI-формат.
// Живой лог стенда: «R60.21 auto-continue failed ... error="upstream response had
// no choices" url=http://cppworker-gpu:18092/api/chat».
func TestR118_17_ContinueResponseParsedInBothSurfaceFormats(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		want     string
		wantEval int
		wantErr  bool
	}{
		{
			name:     "OpenAI /v1/chat/completions",
			payload:  `{"choices":[{"message":{"role":"assistant","content":"продолжение"}}],"usage":{"completion_tokens":7}}`,
			want:     "продолжение",
			wantEval: 7,
		},
		{
			name:     "Ollama /api/chat",
			payload:  `{"model":"m","message":{"role":"assistant","content":"продолжение из native"},"done":true,"eval_count":11}`,
			want:     "продолжение из native",
			wantEval: 11,
		},
		{
			name:     "Ollama /api/generate",
			payload:  `{"model":"m","response":"продолжение generate","done":true,"eval_count":5}`,
			want:     "продолжение generate",
			wantEval: 5,
		},
		{
			name:    "пустой ответ",
			payload: `{"done":true,"message":{"content":""}}`,
			wantErr: true,
		},
		{
			name:    "не JSON",
			payload: `<html>502</html>`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, eval, err := extractAutoContinueContent([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получено %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("разбор упал: %v", err)
			}
			if got != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if eval != tc.wantEval {
				t.Errorf("eval_count = %d, want %d", eval, tc.wantEval)
			}
		})
	}
}

func TestR118_17_TruncationDetectedOnClientVisibleText(t *testing.T) {
	// То, что реально ушло клиенту дельтами (живой ответ, обрезанный капом).
	clientVisible := liveTruncatedHead
	if reason := TruncateReason(clientVisible); reason == "" {
		t.Fatal("детектор не видит обрыв на тексте клиента — авто-продолжение не сработает")
	}

	// То, что накапливал балансер ДО правки: дельты + тот же текст из done-чанка.
	doubled := clientVisible + clientVisible
	if reason := TruncateReason(doubled); reason != "" {
		t.Errorf("на удвоенном тексте детектор сработал (%q) — тест перестал ловить дефект", reason)
	}

	// То, что накапливается ПОСЛЕ правки: одна копия → обрыв виден.
	acc := ""
	acc = appendNativeContent(acc, false, clientVisible)
	acc = appendNativeContent(acc, true, clientVisible)
	if acc != clientVisible {
		t.Fatalf("накопленное не равно тексту клиента: %d против %d символов", len(acc), len(clientVisible))
	}
	if reason := TruncateReason(acc); reason == "" {
		t.Error("после правки детектор обрыва не срабатывает")
	}
}
