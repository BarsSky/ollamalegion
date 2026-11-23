//go:build llama_stub

// load_failure_schema_r704_test.go — R704 (2026-11-24): таблица классификации
// «схема файла не поддержана».
//
// ПРЕДМЕТ. На новом пине llama.cpp (b12642, образ cppworker gpu-r703) файл
// Inkling-2-Mini-UD-Q4_K_M.gguf падает ПОЗЖЕ архитектуры:
//
//	[llama.cpp] llama_model_load: error loading model: check_tensor_dims:
//	            tensor 'blk.24.attn_q.weight' not found
//
// а reason в /api/models (и в ответе) был model_not_found — машинный код врал:
// файл на месте, архитектура распознана. Тесты фиксируют:
//   - новый код model_schema_unsupported для «not found» ПРО тензор;
//   - приоритет: неизвестная архитектура остаётся model_unsupported, OOM —
//     insufficient_resources, отсутствие файла — model_not_found, битый формат —
//     gguf_incompatible;
//   - «not found» в пути к файлу схемой НЕ становится.
//
// НЕ-ВАКУУМНОСТЬ. Ожидаемые коды записаны ЛИТЕРАЛАМИ ("model_schema_unsupported"),
// а проверяется существующая функция classifyLoadFailureReason, поэтому файл
// компилируется и краснеет на состоянии до правки (там этот текст уходил в
// model_not_found). RED-прогон приведён в отчёте.
package main

import (
	"errors"
	"testing"
)

// Тексты взяты из живого лога стенда (файл _diag/inkling-final.log, строка
// runAsyncLoad) — дословно, вместе с приклеенным мостом хвостом llama.cpp.
const (
	// Провал на b12642: архитектуру сборка знает, схемы — нет.
	liveInklingSchemaErrTextR704 = "load model Inkling-2-Mini-UD-Q4_K_M: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf\n" +
		"[llama.cpp] llama_model_load: error loading model: check_tensor_dims: tensor 'blk.24.attn_q.weight' not found\n" +
		"[llama.cpp] llama_model_load_from_file_impl: failed to load model"
	// Тот же провал БЕЗ приклеенного хвоста движка (старый образ/R697-эра):
	// остаётся общим «файл не принят», потому что причины в тексте нет.
	preR704InklingLoadErrText = "load model Inkling-2-Mini-UD-Q4_K_M: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf"
	// Реально отсутствующий файл: «not found» про ФАЙЛ, а не про тензор.
	missingFileErrTextR704 = "load model x: load model failed: failed to open GGUF file /app/models/x.gguf (No such file or directory)"
	// «tensor» в имени файла + «not found» в той же строке — не повод присвоить
	// чужой провал (защита от подстрочного совпадения).
	tensorInPathErrTextR704 = "failed to load model from /app/models/tensor-model.gguf: no such file"
)

// TestR704_ClassifySchemaUnsupported — таблица кейсов и приоритетов.
func TestR704_ClassifySchemaUnsupported(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "живой кейс стенда: check_tensor_dims + имя тензора",
			err:  errors.New(liveInklingSchemaErrTextR704),
			want: "model_schema_unsupported",
		},
		{
			name: "только строка движка: tensor '…' not found",
			err:  errors.New("llama_model_load: error loading model: tensor 'blk.47.attn_q.weight' not found"),
			want: "model_schema_unsupported",
		},
		{
			name: "check_tensor_dims без имени тензора",
			err:  errors.New("llama_model_load: error loading model: check_tensor_dims: 12 tensors not found"),
			want: "model_schema_unsupported",
		},
		{
			// ПОРЯДОК ПРИОРИТЕТОВ: вердикт про архитектуру (R697) важнее схемы.
			// Иначе «unknown model architecture» с хвостом про тензоры получил бы
			// код, советующий ре-экспорт файла, вместо «обновите сборку».
			name: "неизвестная архитектура остаётся model_unsupported",
			err:  errors.New("llama_model_load: error loading model: unknown model architecture: 'inkling2'"),
			want: "model_unsupported",
		},
		{
			name: "оба признака в одном тексте: архитектура выигрывает",
			err: errors.New("llama_model_load: error loading model: unknown model architecture: 'x'\n" +
				"check_tensor_dims: tensor 'blk.24.attn_q.weight' not found"),
			want: "model_unsupported",
		},
		{
			// Обратная сторона: «not found» про ФАЙЛ — прежний код причины.
			name: "нет файла модели — model_not_found",
			err:  errors.New(missingFileErrTextR704),
			want: "model_not_found",
		},
		{
			name: "«tensor» в пути к файлу не делает провал схемой",
			err:  errors.New(tensorInPathErrTextR704),
			want: "model_not_found",
		},
		{
			name: "битый формат (bad magic) — gguf_incompatible",
			err:  errors.New("llama_model_load: error loading model: bad magic"),
			want: "gguf_incompatible",
		},
		{
			name: "неизвестная версия GGUF — gguf_incompatible",
			err:  errors.New("validate_metadata: unsupported gguf version: 7"),
			want: "gguf_incompatible",
		},
		{
			name: "нехватка памяти — insufficient_resources",
			err:  errors.New("CUDA error: out of memory"),
			want: "insufficient_resources",
		},
		{
			name: "строки про тензоры без «not found» — не схема",
			err: errors.New("llama_model_load: error loading model: check_tensor_dims: " +
				"tensor 'blk.0.attn_q.weight' has wrong shape"),
			want: "gguf_incompatible",
		},
		{
			// Текст ДО R704: причины в нём нет (только путь) — «файл не принят».
			name: "текст без причины движка остаётся gguf_incompatible",
			err:  errors.New(preR704InklingLoadErrText),
			want: "gguf_incompatible",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLoadFailureReason(tc.err, tc.err.Error()); got != tc.want {
				t.Fatalf("classifyLoadFailureReason() = %q, want %q\nтекст: %s",
					got, tc.want, tc.err.Error())
			}
		})
	}
}

// TestR704_EmptyAndTypedErrors — классификатор не должен меняться на пустом
// тексте и на типизированных ошибках (OOM приходит типом, а не строкой).
func TestR704_EmptyAndTypedErrors(t *testing.T) {
	if got := classifyLoadFailureReason(nil, ""); got != "unknown" {
		t.Errorf("пустой провал = %q, want unknown", got)
	}
	oom := &InsufficientResourcesError{Model: "m", RequestedNCtx: 8192, MaxViableNCtx: 4096}
	if got := classifyLoadFailureReason(oom, oom.Error()); got != "insufficient_resources" {
		t.Errorf("типизированный OOM = %q, want insufficient_resources", got)
	}
}
