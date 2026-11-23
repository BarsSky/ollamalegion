//go:build llama_stub

// model_unsupported_r697_test.go — R697 (2026-11-24).
//
// ПРЕДМЕТ. Модель Inkling-2-Mini (заголовок GGUF: general.architecture="inkling2")
// на воркере gpu-r660 не загружалась, и наружу уходил ОБЩИЙ текст
//
//	load failed: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf
//
// — то есть причиной выглядел путь к файлу. Настоящая причина (llama.cpp не знает
// архитектуру) лежала в логе движка. Тесты ниже закрепляют:
//   - код причины model_unsupported вместо gguf_incompatible;
//   - имя архитектуры и сборку llama.cpp в ответе;
//   - 409 (повтор не поможет) вместо 500;
//   - то же в реестре провалов и в /api/models/load/progress.
//
// ВАЖНО про не-вакуумность: ожидаемые коды в проверках классификации записаны
// ЛИТЕРАЛАМИ ("model_unsupported"), поэтому файл компилируется и краснеет на версии
// до правки (git show 1a3a364d:cmd/cppworker/load_failure_reason.go), где такой
// причины не существует и текст попадает в gguf_incompatible.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Тексты взяты дословно из поведения c/llama.cpp b1b770d90 (пин сабмодуля):
//
//	src/llama-model.cpp:409  throw "unknown model architecture: '<arch>'"
//	src/llama-model.cpp:388  throw "unsupported model architecture: '<arch>'"
//	src/llama.cpp:374/432     LLAMA_LOG_ERROR "... error loading model: ..." + "failed to load model"
//	c/bridge/bridge.c:1905    set_error("failed to load model from <path>")
const (
	// Сырой текст ошибки воркера ПОСЛЕ R697: причина приклеена мостом
	// (internal/cppbackend/load_error_tail.go).
	liveInklingLoadErrText = "load model Inkling-2-Mini-UD-Q4_K_M: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf\n" +
		"[llama.cpp] llama_model_load: error loading model: unknown model architecture: 'inkling2'\n" +
		"[llama.cpp] llama_model_load_from_file_impl: failed to load model"
	// Тот же провал ДО R697: без причины, только путь к файлу.
	preFixInklingLoadErrText = "load model Inkling-2-Mini-UD-Q4_K_M: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf"
	// Ветка default в llama_model_mapping — LLM_ARCH_* есть, реализации нет.
	unsupportedArchErrText = "llama_model_load: error loading model: unsupported model architecture: 'zzz42'"
)

// TestR697_ClassifyUnknownArchAsModelUnsupported — «архитектура не поддерживается»
// обязана иметь СВОЙ код, а не прятаться в gguf_incompatible.
func TestR697_ClassifyUnknownArchAsModelUnsupported(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "живой кейс: unknown model architecture с приклеенным вердиктом",
			err:      errors.New(liveInklingLoadErrText),
			wantCode: "model_unsupported",
		},
		{
			name:     "unknown model architecture без хвоста",
			err:      errors.New("llama_model_load: error loading model: unknown model architecture: 'inkling2'"),
			wantCode: "model_unsupported",
		},
		{
			name:     "unsupported model architecture (ветка default)",
			err:      errors.New(unsupportedArchErrText),
			wantCode: "model_unsupported",
		},
		{
			name:     "регистр не важен: движок печатает разными регистрами",
			err:      errors.New("Unknown Model Architecture: 'Something'"),
			wantCode: "model_unsupported",
		},
		{
			// НЕ-ВАКУУМНОСТЬ обратной стороны: прежний текст (только путь) остаётся
			// общим «файл не принят». Иначе тест проходил бы на любом провале.
			name:     "текст до R697 (только путь) НЕ становится model_unsupported",
			err:      errors.New(preFixInklingLoadErrText),
			wantCode: "gguf_incompatible",
		},
		{
			name:     "нехватка памяти не превращается в архитектуру",
			err:      errors.New("CUDA error: out of memory"),
			wantCode: "insufficient_resources",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLoadFailureReason(tc.err, tc.err.Error()); got != tc.wantCode {
				t.Fatalf("classifyLoadFailureReason() = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestR697_ArchNameFromEngineText — имя архитектуры берётся из слов ДВИЖКА, а не из
// имени файла: имя файла это намерение автора модели, архитектура — факт из заголовка.
func TestR697_ArchNameFromEngineText(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{liveInklingLoadErrText, "inkling2"},
		{unsupportedArchErrText, "zzz42"},
		{"unknown model architecture: 'qwen4exp'", "qwen4exp"},
		{"load model x: failed to load model from /app/models/x.gguf", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := modelUnsupportedArchFromText(tc.text); got != tc.want {
			t.Errorf("modelUnsupportedArchFromText(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// TestR697_HintNamesArchAndUpstreamVersion — подсказка обязана быть действием:
// архитектура + версия upstream, где она появилась (для inkling2 это b12642).
func TestR697_HintNamesArchAndUpstreamVersion(t *testing.T) {
	hint := modelUnsupportedHintR697("inkling2")
	if !strings.Contains(hint, "c/llama.cpp") {
		t.Errorf("подсказка не называет сабмодуль: %q", hint)
	}
	if !strings.Contains(hint, "b12642") {
		t.Errorf("подсказка не называет версию upstream (b12642): %q", hint)
	}
	if msg := modelUnsupportedMessageR697("inkling2", "b1b770d90"); !strings.Contains(msg, "inkling2") ||
		!strings.Contains(msg, "b1b770d90") {
		t.Errorf("сообщение обязано называть архитектуру и сборку: %q", msg)
	}
	// Неизвестная архитектура — без выдуманной версии и без пустоты.
	other := modelUnsupportedHintR697("zzz42")
	if strings.Contains(other, "b12642") {
		t.Errorf("для незнакомой архитектуры версия не выдумывается: %q", other)
	}
	if strings.TrimSpace(modelUnsupportedMessageR697("", "")) == "" {
		t.Error("сообщение без имени архитектуры пусто — клиент не увидит причины")
	}
}

// TestR697_RespondModelUnsupportedIs409 — терминальный отказ по содержимому файла:
// 409 + машинный код + факты. 500 означал бы «воркер сломался, повтори».
func TestR697_RespondModelUnsupportedIs409(t *testing.T) {
	rec := httptest.NewRecorder()
	handled := respondModelUnsupportedR697(rec, "Inkling-2-Mini-UD-Q4_K_M",
		errors.New(liveInklingLoadErrText))
	if !handled {
		t.Fatal("провал по архитектуре не обработан — уйдёт общий 500 «load failed»")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (409)", rec.Code, http.StatusConflict)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got, _ := body["code"].(string); got != "model_unsupported" {
		t.Errorf("code = %q, want model_unsupported", got)
	}
	if got, _ := body["reason"].(string); got != "model_unsupported" {
		t.Errorf("reason = %q, want model_unsupported", got)
	}
	if got, _ := body["arch"].(string); got != "inkling2" {
		t.Errorf("arch = %q, want inkling2 (имя архитектуры из движка)", got)
	}
	if got, _ := body["model"].(string); got != "Inkling-2-Mini-UD-Q4_K_M" {
		t.Errorf("model = %q", got)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "inkling2") {
		t.Errorf("error не называет архитектуру: %q", msg)
	}
	if hint, _ := body["hint"].(string); !strings.Contains(hint, "c/llama.cpp") {
		t.Errorf("hint не говорит, что делать: %q", hint)
	}
	if raw, _ := body["raw_error"].(string); !strings.Contains(raw, "unknown model architecture") {
		t.Errorf("raw_error потерял строку llama.cpp: %q", raw)
	}
}

// TestR697_OtherLoadFailuresStayGeneric — обратная сторона: обычный провал загрузки
// (файла нет, память) НЕ должен подменяться отказом по архитектуре.
func TestR697_OtherLoadFailuresStayGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	if respondModelUnsupportedR697(rec, "gemma-4", errors.New("failed to open GGUF file x.gguf (No such file or directory)")) {
		t.Fatal("провал «файла нет» обработан как неподдерживаемая архитектура")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("для чужого провала ответ писать нельзя: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestR697_RegistryKeepsArchAndBuild — причина уходит в /api/models (поле load_failure)
// вместе с диагностикой: имя архитектуры и сборка нужны уведомлению оператору.
func TestR697_RegistryKeepsArchAndBuild(t *testing.T) {
	const model = "Inkling-2-Mini-UD-Q4_K_M"
	loadFailures.clear(model)
	defer loadFailures.clear(model)

	recordLoadFailureR697(model, errors.New(liveInklingLoadErrText))

	fe, ok := loadFailures.get(model)
	if !ok {
		t.Fatal("провал не записан — /api/models не покажет причину, поллер увидит вечное «грузится»")
	}
	if fe.Reason != "model_unsupported" {
		t.Errorf("reason = %q, want model_unsupported", fe.Reason)
	}
	if got, _ := fe.Diagnostics["arch"].(string); got != "inkling2" {
		t.Errorf("diagnostics.arch = %v, want inkling2", fe.Diagnostics["arch"])
	}
	if hint, _ := fe.Diagnostics["hint"].(string); !strings.Contains(hint, "c/llama.cpp") {
		t.Errorf("diagnostics.hint пуст или без действия: %q", hint)
	}
	if got, _ := fe.Diagnostics["arch_supported_since"].(string); got != "b12642" {
		t.Errorf("diagnostics.arch_supported_since = %v, want b12642", fe.Diagnostics["arch_supported_since"])
	}
	// Обычный провал диагностики архитектуры не получает.
	loadFailures.recordDetailed(model, "", errors.New("CUDA error: out of memory"), nil)
	if fe, _ := loadFailures.get(model); fe.Reason == "model_unsupported" {
		t.Error("нехватка памяти записана как неподдерживаемая архитектура")
	}
}

// TestR697_ProgressCarriesReasonAndHint — поллер (WebUI через балансер) получает
// state="failed" с кодом причины и подсказкой, а не только текст llama.cpp.
func TestR697_ProgressCarriesReasonAndHint(t *testing.T) {
	// Поллер читает реестр моделей воркера — подменяем пакетный backend (как R83).
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "Inkling-2-Mini-UD-Q4_K_M"
	loadFailures.clear(model)
	defer loadFailures.clear(model)

	recordLoadFailureR697(model, errors.New(liveInklingLoadErrText))

	req := httptest.NewRequest(http.MethodGet, "/api/models/load/progress?model="+model, nil)
	rec := httptest.NewRecorder()
	handleLoadProgress(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := body["state"].(string); got != "failed" {
		t.Fatalf("state = %q, want failed", got)
	}
	if got, _ := body["reason"].(string); got != "model_unsupported" {
		t.Errorf("reason = %q, want model_unsupported", got)
	}
	if hint, _ := body["hint"].(string); !strings.Contains(hint, "c/llama.cpp") {
		t.Errorf("hint отсутствует или пуст: %q", hint)
	}
	if errText, _ := body["error"].(string); !strings.Contains(errText, "unknown model architecture") {
		t.Errorf("error потерял строку llama.cpp: %q", errText)
	}
}

// TestR697_SeverityAndCatalog — код причины обязан быть валидным (клиенты присылают
// его назад) и требовать внимания оператора (error, не warning).
func TestR697_SeverityAndCatalog(t *testing.T) {
	if !isLoadFailureReason("model_unsupported") {
		t.Error("model_unsupported отсутствует в словаре причин — клиент не сможет его прислать")
	}
	if got := loadFailureSeverity("model_unsupported"); got != "error" {
		t.Errorf("severity = %q, want error (это действие оператора, а не транзиент)", got)
	}
	// Реестр живёт по TTL: запись обязана истекать, иначе «failed» висит вечно.
	reg := newLoadFailureRegistry(time.Minute)
	reg.recordDetailed("m", "model_unsupported", errors.New(liveInklingLoadErrText),
		modelUnsupportedDiagnosticsR697("inkling2", "b1b770d90"))
	if got := reg.m["m"].Reason; got != "model_unsupported" {
		t.Errorf("явный код причины переклассифицирован в %q", got)
	}
}
