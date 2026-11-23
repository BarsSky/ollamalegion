//go:build llama_stub

// model_schema_unsupported_r704_test.go — R704 (2026-11-24): ответ 409, реестр
// провалов и поллинг-поверхность для кода model_schema_unsupported.
//
// ПРЕДМЕТ. Причина «схема файла не поддержана» (loop-развёрнутый GGUF: 24
// физических блока при block_count=48) обязана доезжать до клиента теми же
// поверхностями, что и model_unsupported (R697), но со своими фактами:
//   - 409 (повтор не поможет) + code/reason + error + hint + raw_error;
//   - arch и block_count из ЗАГОЛОВКА файла, physical_blocks и tensor — из
//     строки движка;
//   - реестр провалов (/api/models → agent → балансер → WebUI) и
//     /api/models/load/progress отдают reason и hint.
//
// НЕ-ВАКУУМНОСТЬ. Ожидаемые коды — литералами; проверяется реальный backend с
// настоящим (минимальным) GGUF-заголовком, поэтому arch/block_count не
// подставлены в тест, а прочитаны парсером. Внешние провалы (файла нет, OOM)
// обязаны остаться общими — это проверяет TestR704_DispatchKeepsForeignFailuresGeneric.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// schemaTestModelR704 — имя модели в тестах (как в живом кейсе стенда).
const schemaTestModelR704 = "Inkling-2-Mini-UD-Q4_K_M"

// newSchemaTestBackendR704 — backend с настоящим минимальным GGUF-заголовком
// (arch=inkling2, block_count=48, head_count=24, head_count_kv=8, n_embd=2304).
//
// Заголовок пишется существующим тестовым помощником makeFakeGGUF
// (lazyload_calc_test.go), поэтому arch/block_count в ответе — результат
// работы парсера, а не константы теста.
func newSchemaTestBackendR704(t *testing.T) *cppbackend.Backend {
	t.Helper()
	dir := t.TempDir()
	makeFakeGGUF(t, filepath.Join(dir, schemaTestModelR704+".gguf"), "inkling2", 48, 24, 8, 2304)
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: dir,
	})
	if err := b.Init(); err != nil {
		// stub-сборка не имеет настоящего llama.cpp — Init может ругнуться,
		// менеджер моделей при этом рабочий.
		_ = err
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	// Флейк TempDir RemoveAll на Windows: .name_history.json пишется в фоне
	// (тот же приём, что newTestBackendR303).
	t.Cleanup(func() {
		if mm := b.ModelManager(); mm != nil {
			if !mm.WaitForPendingWrites(2 * time.Second) {
				t.Logf("warning: nameHistory persist не завершился за 2 с")
			}
		}
	})
	return b
}

// TestR704_DetectsSchemaFailureText — предикат и его обратная сторона: чужие
// провалы (нет файла, битый формат, OOM, неизвестная архитектура) не должны
// попадать в новый код.
func TestR704_DetectsSchemaFailureText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"живой текст стенда", liveInklingSchemaErrTextR704, true},
		{"только строка движка", "tensor 'blk.24.attn_q.weight' not found", true},
		{"check_tensor_dims без имени тензора", "check_tensor_dims: 3 tensors not found", true},
		{"пусто", "", false},
		{"нет файла модели", missingFileErrTextR704, false},
		{"«tensor» в пути", tensorInPathErrTextR704, false},
		{"битый формат", "llama_model_load: error loading model: bad magic", false},
		{"OOM", "CUDA error: out of memory", false},
		{"неизвестная архитектура", "unknown model architecture: 'zzz42'", false},
		{
			// Вердикт про архитектуру приоритетнее даже при хвосте про тензоры.
			name: "архитектура + тензор в одном тексте",
			text: "unknown model architecture: 'x'\ntensor 'blk.1.a' not found",
			want: false,
		},
		{
			name: "тензор без «not found»",
			text: "check_tensor_dims: tensor 'blk.0.attn_q.weight' has wrong shape",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelSchemaUnsupportedText(tc.text); got != tc.want {
				t.Fatalf("isModelSchemaUnsupportedText() = %v, want %v (%q)", got, tc.want, tc.text)
			}
		})
	}
}

// TestR704_TensorAndBlockFromText — из строки движка берутся имя тензора и номер
// блока: номер = число физических блоков (движок падает на первом блоке, которого
// в файле нет).
func TestR704_TensorAndBlockFromText(t *testing.T) {
	cases := []struct {
		text       string
		wantTensor string
		wantBlock  int
	}{
		{liveInklingSchemaErrTextR704, "blk.24.attn_q.weight", 24},
		{"tensor 'blk.47.ffn_down.weight' not found", "blk.47.ffn_down.weight", 47},
		{"tensor \"blk.3.attn_k.weight\" not found", "blk.3.attn_k.weight", 3},
		{"check_tensor_dims: 3 tensors not found", "", 0},
		{"tensor 'output.weight' not found", "output.weight", 0},
		{"no tensor here", "", 0},
	}
	for _, tc := range cases {
		tensor := modelSchemaUnsupportedTensorFromText(tc.text)
		if tensor != tc.wantTensor {
			t.Errorf("tensor(%q) = %q, want %q", tc.text, tensor, tc.wantTensor)
		}
		if got := modelSchemaUnsupportedBlockFromTensor(tensor); got != tc.wantBlock {
			t.Errorf("block(%q) = %d, want %d", tensor, got, tc.wantBlock)
		}
	}
}

// TestR704_RespondIs409WithFacts — терминальный отказ по содержимому файла:
// 409 + машинный код + факты (arch/block_count из заголовка, tensor и
// physical_blocks из строки движка) + подсказка с ре-экспортом.
func TestR704_RespondIs409WithFacts(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newSchemaTestBackendR704(t)

	loadFailures.clear(schemaTestModelR704)
	defer loadFailures.clear(schemaTestModelR704)

	rec := httptest.NewRecorder()
	handled := respondModelSchemaUnsupportedR704(rec, schemaTestModelR704,
		errors.New(liveInklingSchemaErrTextR704))
	if !handled {
		t.Fatal("провал по схеме не обработан — уйдёт общий 500 «load failed»")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (409 — повтор не поможет)", rec.Code, http.StatusConflict)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got, _ := body["code"].(string); got != "model_schema_unsupported" {
		t.Errorf("code = %q, want model_schema_unsupported", got)
	}
	if got, _ := body["reason"].(string); got != "model_schema_unsupported" {
		t.Errorf("reason = %q, want model_schema_unsupported", got)
	}
	if got, _ := body["model"].(string); got != schemaTestModelR704 {
		t.Errorf("model = %q", got)
	}
	// arch и block_count — из ЗАГОЛОВКА файла (парсер), а не из имени модели.
	if got, _ := body["arch"].(string); got != "inkling2" {
		t.Errorf("arch = %q, want inkling2 (general.architecture из GGUF)", got)
	}
	if got, _ := body["block_count"].(float64); got != 48 {
		t.Errorf("block_count = %v, want 48 (из заголовка)", body["block_count"])
	}
	// physical_blocks и tensor — из строки движка.
	if got, _ := body["physical_blocks"].(float64); got != 24 {
		t.Errorf("physical_blocks = %v, want 24 (первый отсутствующий блок)", body["physical_blocks"])
	}
	if got, _ := body["tensor"].(string); got != "blk.24.attn_q.weight" {
		t.Errorf("tensor = %q", got)
	}
	// llama_build присутствует всегда (пустая строка = сборка не запечена), hint
	// обязан быть действием: ре-экспорт как inkling2_loop с block_count=24.
	if _, ok := body["llama_build"]; !ok {
		t.Error("в ответе нет поля llama_build — оператор не узнает сборку")
	}
	hint, _ := body["hint"].(string)
	if !strings.Contains(hint, "inkling2_loop") || !strings.Contains(hint, "block_count=24") {
		t.Errorf("hint не называет ре-экспорт (inkling2_loop, block_count=24): %q", hint)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "num_loops") || !strings.Contains(msg, "24") || !strings.Contains(msg, "48") {
		t.Errorf("error не объясняет loop-развёрнутую схему числами: %q", msg)
	}
	if raw, _ := body["raw_error"].(string); !strings.Contains(raw, "check_tensor_dims") {
		t.Errorf("raw_error потерял строку llama.cpp: %q", raw)
	}
}

// TestR704_RespondWithoutHeaderFacts — если заголовок прочитать нельзя (файла на
// диске нет), отказ всё равно уходит: 409 с кодом и подсказкой без чисел. Иначе
// провал по схеме прятался бы в общий 500 из-за отсутствия метаданных.
func TestR704_RespondWithoutHeaderFacts(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t) // каталог моделей пуст — arch/block_count неизвестны

	rec := httptest.NewRecorder()
	if !respondModelSchemaUnsupportedR704(rec, "unknown-model", errors.New(liveInklingSchemaErrTextR704)) {
		t.Fatal("отказ по схеме не обработан")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := body["arch"]; ok {
		t.Error("arch выдуман при неизвестной архитектуре")
	}
	if hint, _ := body["hint"].(string); strings.TrimSpace(hint) == "" {
		t.Error("подсказка пуста — оператор не узнает, что делать")
	}
}

// TestR704_DispatchKeepsForeignFailuresGeneric — обратная сторона: обычный провал
// (файла нет, память) НЕ подменяется отказом по схеме, и ответ не пишется.
func TestR704_DispatchKeepsForeignFailuresGeneric(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"нет файла", errors.New(missingFileErrTextR704)},
		{"OOM", errors.New("CUDA error: out of memory")},
		{"битый формат", errors.New("bad magic")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if respondLoadRefusalR704(rec, "gemma-4", tc.err) {
				t.Fatalf("%q обработан как отказ по схеме/архитектуре", tc.err)
			}
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Fatalf("для чужого провала ответ писать нельзя: status=%d body=%s",
					rec.Code, rec.Body.String())
			}
		})
	}
}

// TestR704_DispatchOrderAndBothRefusals — единый вход отвечает на оба
// «содержательных» отказа и различает их: архитектура → model_unsupported
// (R697), схема → model_schema_unsupported (R704).
func TestR704_DispatchOrderAndBothRefusals(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newSchemaTestBackendR704(t)

	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"неизвестная архитектура", errors.New("unknown model architecture: 'inkling2'"), "model_unsupported"},
		{"схема файла", errors.New(liveInklingSchemaErrTextR704), "model_schema_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !respondLoadRefusalR704(rec, schemaTestModelR704, tc.err) {
				t.Fatal("отказ не обработан общим входом")
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409", rec.Code)
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got, _ := body["code"].(string); got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestR704_RegistryKeepsSchemaFacts — причина уходит в /api/models (поле
// load_failure) вместе с диагностикой: arch, block_count, physical_blocks, tensor
// и hint. Без них уведомление оператору снова станет «файл не принят».
func TestR704_RegistryKeepsSchemaFacts(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newSchemaTestBackendR704(t)

	loadFailures.clear(schemaTestModelR704)
	defer loadFailures.clear(schemaTestModelR704)

	recordLoadFailureR697(schemaTestModelR704, errors.New(liveInklingSchemaErrTextR704))

	fe, ok := loadFailures.get(schemaTestModelR704)
	if !ok {
		t.Fatal("провал не записан — /api/models не покажет причину")
	}
	if fe.Reason != "model_schema_unsupported" {
		t.Fatalf("reason = %q, want model_schema_unsupported", fe.Reason)
	}
	if got, _ := fe.Diagnostics["arch"].(string); got != "inkling2" {
		t.Errorf("diagnostics.arch = %v", fe.Diagnostics["arch"])
	}
	if got, _ := fe.Diagnostics["block_count"].(int); got != 48 {
		t.Errorf("diagnostics.block_count = %v (тип %T), want 48", fe.Diagnostics["block_count"], fe.Diagnostics["block_count"])
	}
	if got, _ := fe.Diagnostics["physical_blocks"].(int); got != 24 {
		t.Errorf("diagnostics.physical_blocks = %v, want 24", fe.Diagnostics["physical_blocks"])
	}
	if got, _ := fe.Diagnostics["tensor"].(string); got != "blk.24.attn_q.weight" {
		t.Errorf("diagnostics.tensor = %v", fe.Diagnostics["tensor"])
	}
	if hint, _ := fe.Diagnostics["hint"].(string); !strings.Contains(hint, "inkling2_loop") {
		t.Errorf("diagnostics.hint пуст или без действия: %q", hint)
	}
	// Чужой провал диагностики схемы не получает.
	loadFailures.recordDetailed(schemaTestModelR704, "", errors.New("CUDA error: out of memory"), nil)
	if fe, _ := loadFailures.get(schemaTestModelR704); fe.Reason == "model_schema_unsupported" {
		t.Error("нехватка памяти записана как неподдерживаемая схема")
	}
}

// TestR704_ProgressCarriesReasonAndHint — поллер (WebUI через балансер) получает
// state="failed" с кодом причины и подсказкой, а не только текст llama.cpp.
func TestR704_ProgressCarriesReasonAndHint(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newSchemaTestBackendR704(t)

	loadFailures.clear(schemaTestModelR704)
	defer loadFailures.clear(schemaTestModelR704)

	recordLoadFailureR697(schemaTestModelR704, errors.New(liveInklingSchemaErrTextR704))

	req := httptest.NewRequest(http.MethodGet, "/api/models/load/progress?model="+schemaTestModelR704, nil)
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
	if got, _ := body["reason"].(string); got != "model_schema_unsupported" {
		t.Errorf("reason = %q, want model_schema_unsupported", got)
	}
	if hint, _ := body["hint"].(string); !strings.Contains(hint, "block_count=24") {
		t.Errorf("hint отсутствует или без чисел: %q", hint)
	}
	if errText, _ := body["error"].(string); !strings.Contains(errText, "check_tensor_dims") {
		t.Errorf("error потерял строку llama.cpp: %q", errText)
	}
}

// TestR704_CatalogAndSeverity — код причины валиден (клиенты присылают его назад)
// и требует внимания оператора: это его действие (ре-экспорт), а не транзиент.
func TestR704_CatalogAndSeverity(t *testing.T) {
	if !isLoadFailureReason("model_schema_unsupported") {
		t.Error("model_schema_unsupported отсутствует в словаре причин")
	}
	if got := loadFailureSeverity("model_schema_unsupported"); got != "error" {
		t.Errorf("severity = %q, want error", got)
	}
	// Подсказка отдаётся обеими «содержательными» причинами и не отдаётся прочим.
	diag := map[string]interface{}{"hint": "x"}
	if loadFailureHintR697("model_schema_unsupported", diag) != "x" {
		t.Error("подсказка схемы не отдаётся в progress")
	}
	if loadFailureHintR697("model_not_found", diag) != "" {
		t.Error("подсказка выдана причине, у которой её нет")
	}
	// Реестр живёт по TTL: запись обязана истекать.
	reg := newLoadFailureRegistry(time.Minute)
	reg.recordDetailed("m", "model_schema_unsupported", errors.New(liveInklingSchemaErrTextR704),
		modelSchemaDiagnosticsR704(schemaTestModelR704, errors.New(liveInklingSchemaErrTextR704)))
	if got := reg.m["m"].Reason; got != "model_schema_unsupported" {
		t.Errorf("явный код причины переклассифицирован в %q", got)
	}
}
