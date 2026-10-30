//go:build llama_stub

// model_missing_classifier_r316_test.go — R316 (2026-10-30): отказ про ОКНО не
// означает «модели нет».
//
// ЖИВАЯ АВАРИЯ. Потолок оператора подняли выше GGUF-максимума модели; запрос на
// окно сверх предела получил от воркера честный отказ «requested n_ctx=262144
// exceeds the physically feasible maximum 131072 … (обучающий контекст МОДЕЛИ)».
// Классификатор искал маркер «модели» — и эта фраза совпала, отказ записался в
// реестр «модели нет». После этого КАЖДЫЙ запрос к живой загруженной модели
// получал терминальный 404 `model_not_found` до истечения TTL: агент считает, что
// модели нет, и уходит на другую.
package balancer

import "testing"

func TestR316_WindowRefusalIsNotModelMissing(t *testing.T) {
	// Формулировка воркера (реальный ответ стенда, сокращённо).
	body := []byte(`{"code":"n_ctx_infeasible","error":"requested n_ctx=262144 exceeds the physically feasible maximum 131072 for model \"gemma-4-E4B-it-Q4_K_M\" on this hardware (max_vram_n_ctx=0, max_ram_n_ctx=131072, gguf_max_context=131072). ... stage does_not_fit Укажите n_ctx <= 131072 (обучающий контекст модели).","suggestion":"Укажите n_ctx <= 131072 (обучающий контекст модели)."}`)

	if isModelMissingResponse(422, body) {
		t.Fatal("отказ про окно записан как «модели нет»: после него живая модель получает 404 до конца TTL")
	}
	// Даже если статус придёт 400/404-подобным — отказ про окно всё равно не про
	// отсутствие модели: маркер n_ctx_infeasible/does_not_fit снимает вопрос.
	for _, status := range []int{400, 404, 413, 422, 500} {
		if isModelMissingResponse(status, body) {
			t.Errorf("status=%d: отказ про окно принят за «модели нет»", status)
		}
	}
}

func TestR316_GenuineMissingStillRecognized(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"error":"model \"x\" not found in ./models (tried \"models/x.gguf\")"}`),
		[]byte(`{"code":"model_file_not_found","error":"model file not found: /app/models/x.gguf"}`),
		[]byte(`{"error":"model not found"}`),
		[]byte(`{"error":"модель не найдена в ./models"}`),
	}
	for _, body := range cases {
		if !isModelMissingResponse(404, body) {
			t.Errorf("настоящее «модели нет» не распознано: %s", string(body))
		}
	}
}
