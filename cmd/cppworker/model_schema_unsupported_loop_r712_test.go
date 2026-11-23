//go:build llama_stub

// model_schema_unsupported_loop_r712_test.go — R712 (2026-11-25): ветка «arch уже
// loop-схема» в ответе 409 model_schema_unsupported (поверх R704).
//
// ПРЕДМЕТ. Живой дефект со стенда (R709): файл УЖЕ переведён в loop-схему
// (general.architecture=inkling2_loop, block_count=24 — это физические блоки,
// ключи loop.start/loop.end/loop.count на месте), но в нём нет обязательного
// тензора blk.{6..17}.loop_msg_query.weight. R704 отвечал так:
//
//	physical_blocks: 6
//	hint: «...ре-экспорт GGUF как inkling2_loop_loop с block_count=6...»
//
// то есть предлагал сделать уже сделанное и с неверным числом: 6 — это НЕ число
// физических блоков, а номер блока из имени тензора blk.6.loop_msg_query.weight
// (core_start loop-схемы). Причина врала дважды: число бралось из имени
// отсутствующего тензора, а архитектура предполагалась до-патчевой (inkling2).
//
// НЕ-ВАКУМНОСТЬ. Каждый кейс — НАСТОЯЩИЙ GGUF-заголовок на диске: его пишет
// writeSchemaGGUFR712, а читает cppbackend (ReadGGUFHeader/ScanModels) в
// newSchemaGGUFBackendR712. Поэтому arch, block_count и признаки loop-ключей в
// ответе — результат парсера, а не константы теста. Ожидаемые числа записаны
// литералами (24/48/42) и проверяются по телу JSON; тексты — по смыслу
// (действие, атрибуция числа, отсутствие выдумки «_loop_loop»/«block_count=6»),
// а не дословно по i18n.
//
// RED на e745e81e: TestR712_LoopSchema*, TestR712_PrePatchArch*,
// TestR712_NonLoopArch* красные (см. отчёт); TestR712_OtherMissingTensor_* —
// сторож прежней общей ветки, зелёный и до, и после.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
)

// ggufUint32R712 — пара «ключ метаданных → uint32» для тестового заголовка.
type ggufUint32R712 struct {
	key string
	val uint32
}

// liveLoopMsgQueryErrTextR712 — текст движка для дефекта R709/R712: файл уже в
// loop-схеме, не хватает только свёрнутого тензора запроса.
const liveLoopMsgQueryErrTextR712 = "load model Inkling-2-Mini-UD-Q4_K_M: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf\n" +
	"[llama.cpp] llama_model_load: error loading model: check_tensor_dims: tensor 'blk.6.loop_msg_query.weight' not found\n" +
	"[llama.cpp] llama_model_load_from_file_impl: failed to load model"

// withSchemaLangR712 — детерминированный язык ответа: тексты проверяются по
// русским подстрокам, а bootstrap i18n может зависеть от окружения.
func withSchemaLangR712(t *testing.T) {
	t.Helper()
	old := i18n.Get()
	i18n.Set(i18n.RU)
	t.Cleanup(func() { i18n.Set(old) })
}

// writeSchemaGGUFR712 — минимальный валидный GGUF v3: general.architecture +
// перечисленные uint32-ключи. Тензоров ноль (ответ 409 строится по строке
// движка, файл нужен только как источник заголовка).
func writeSchemaGGUFR712(t *testing.T, path, arch string, ints []ggufUint32R712) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake gguf: %v", err)
	}
	defer f.Close()

	if _, err := f.Write([]byte{'G', 'G', 'U', 'F'}); err != nil {
		t.Fatalf("write magic: %v", err)
	}
	binary.Write(f, binary.LittleEndian, uint32(3))
	binary.Write(f, binary.LittleEndian, uint64(0))           // tensor count
	binary.Write(f, binary.LittleEndian, uint64(1+len(ints))) // metadata KV count
	writeGGUFString(f, "general.architecture", arch)
	for _, kv := range ints {
		writeGGUFUint32(f, kv.key, kv.val)
	}
}

// newSchemaGGUFBackendR712 — backend с одним настоящим (минимальным) GGUF и
// просканированным каталогом: arch/block_count/loop-ключи читает сам cppbackend.
func newSchemaGGUFBackendR712(t *testing.T, model, arch string, ints []ggufUint32R712) *cppbackend.Backend {
	t.Helper()
	dir := t.TempDir()
	writeSchemaGGUFR712(t, filepath.Join(dir, model+".gguf"), arch, ints)
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18092,
		ModelsDir: dir,
	})
	if err := b.Init(); err != nil {
		// stub-сборка: llama.cpp нет, менеджер моделей при этом рабочий.
		_ = err
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	// Флейк TempDir RemoveAll на Windows: .name_history.json пишется в фоне.
	t.Cleanup(func() {
		if mm := b.ModelManager(); mm != nil {
			if !mm.WaitForPendingWrites(2 * time.Second) {
				t.Logf("warning: nameHistory persist не завершился за 2 с")
			}
		}
	})
	return b
}

// useSchemaBackendR712 — подменить пакетный backend на время теста.
func useSchemaBackendR712(t *testing.T, b *cppbackend.Backend) {
	t.Helper()
	old := backend
	backend = b
	t.Cleanup(func() { backend = old })
}

// schemaRespondR712 — вызвать отказ по схеме и вернуть разобранное тело.
func schemaRespondR712(t *testing.T, model, loadErrText string) map[string]interface{} {
	t.Helper()
	loadFailures.clear(model)
	t.Cleanup(func() { loadFailures.clear(model) })

	rec := httptest.NewRecorder()
	if !respondModelSchemaUnsupportedR704(rec, model, errors.New(loadErrText)) {
		t.Fatalf("отказ по схеме не обработан для %q", loadErrText)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (повтор не поможет)", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	// Публичный контракт R704: код причины не меняется ради новой ветки.
	if got, _ := body["code"].(string); got != "model_schema_unsupported" {
		t.Fatalf("code = %q, want model_schema_unsupported", got)
	}
	return body
}

// bodyStringR712 — строковое поле тела (пустая строка, если поля нет).
func bodyStringR712(body map[string]interface{}, key string) string {
	s, _ := body[key].(string)
	return s
}

// TestR712_LoopSchema_MissingQueryTensor — кейс (а): arch inkling2_loop,
// block_count=24 (физические блоки), нет blk.6.loop_msg_query.weight.
//
// Ответ обязан: взять physical_blocks из ЗАГОЛОВКА (24, а не 6 из имени
// тензора), назвать действие (scripts/gguf_loop_meta_patch.py
// --fuse-loop-queries и/или штатный конвертер) и не предлагать «_loop_loop» с
// block_count=6. Имя отсутствующего тензора остаётся фактом в поле tensor.
func TestR712_LoopSchema_MissingQueryTensor(t *testing.T) {
	withSchemaLangR712(t)
	const model = "Inkling-2-Mini-loop-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "inkling2_loop", []ggufUint32R712{
		{"inkling2_loop.block_count", 24},
		{"inkling2_loop.num_loops", 3},
		{"inkling2_loop.loop.start", 6},
		{"inkling2_loop.loop.end", 18},
		{"inkling2_loop.loop.count", 3},
	}))

	body := schemaRespondR712(t, model, liveLoopMsgQueryErrTextR712)

	if got := bodyStringR712(body, "arch"); got != "inkling2_loop" {
		t.Errorf("arch = %q, want inkling2_loop (general.architecture из заголовка)", got)
	}
	if got, _ := body["block_count"].(float64); got != 24 {
		t.Errorf("block_count = %v, want 24 (из заголовка)", body["block_count"])
	}
	// ГЛАВНОЕ: physical_blocks — из заголовка, а не номер блока 6 из имени
	// отсутствующего тензора.
	if got, _ := body["physical_blocks"].(float64); got != 24 {
		t.Errorf("physical_blocks = %v, want 24 (block_count из заголовка; 6 — это core_start, а не число блоков)",
			body["physical_blocks"])
	}
	if got := bodyStringR712(body, "tensor"); got != "blk.6.loop_msg_query.weight" {
		t.Errorf("tensor = %q, want blk.6.loop_msg_query.weight (факт движка)", got)
	}

	hint := bodyStringR712(body, "hint")
	for _, want := range []string{
		"уже в loop-схеме",
		"inkling2_loop",
		"block_count=24",
		"scripts/gguf_loop_meta_patch.py --fuse-loop-queries",
		"docs/features/inkling2-loop.md",
		"конвертером llama.cpp",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint не содержит %q: %q", want, hint)
		}
	}
	for _, unwanted := range []string{"_loop_loop", "block_count=6"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint содержит враньё %q: %q", unwanted, hint)
		}
	}

	msg := bodyStringR712(body, "error")
	for _, want := range []string{"loop-схема", "24"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error не объясняет loop-схему числом %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, "_loop_loop") {
		t.Errorf("error содержит _loop_loop: %q", msg)
	}

	// Тот же ответ уходит в реестр провалов (/api/models): физические блоки и
	// действие обязаны совпадать с телом 409.
	diag := modelSchemaDiagnosticsR704(model, errors.New(liveLoopMsgQueryErrTextR712))
	if got, _ := diag["physical_blocks"].(int); got != 24 {
		t.Errorf("diagnostics.physical_blocks = %v, want 24 (из заголовка)", diag["physical_blocks"])
	}
	if hint, _ := diag["hint"].(string); !strings.Contains(hint, "--fuse-loop-queries") {
		t.Errorf("diagnostics.hint без действия --fuse-loop-queries: %q", hint)
	}
}

// TestR712_LoopSchema_CanonicalKeysWithOldArchName — кейс «в заголовке есть
// loop.start/end/count»: файл уже в loop-схеме, даже если general.architecture
// ещё не переименован. Ветка обязана включиться по КЛЮЧАМ, а не только по имени
// архитектуры.
func TestR712_LoopSchema_CanonicalKeysWithOldArchName(t *testing.T) {
	withSchemaLangR712(t)
	const model = "Inkling-2-Mini-canonical-keys-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "inkling2", []ggufUint32R712{
		{"inkling2.block_count", 24},
		{"inkling2.loop.start", 6},
		{"inkling2.loop.end", 18},
		{"inkling2.loop.count", 3},
	}))

	body := schemaRespondR712(t, model, liveLoopMsgQueryErrTextR712)

	if got := bodyStringR712(body, "arch"); got != "inkling2" {
		t.Errorf("arch = %q, want inkling2 (заголовок этого теста)", got)
	}
	if got, _ := body["physical_blocks"].(float64); got != 24 {
		t.Errorf("physical_blocks = %v, want 24 (block_count из заголовка)", body["physical_blocks"])
	}
	hint := bodyStringR712(body, "hint")
	if !strings.Contains(hint, "уже в loop-схеме") {
		t.Errorf("hint не распознал loop-схему по ключам loop.start/end/count: %q", hint)
	}
	if !strings.Contains(hint, "scripts/gguf_loop_meta_patch.py --fuse-loop-queries") {
		t.Errorf("hint без действия --fuse-loop-queries: %q", hint)
	}
	for _, unwanted := range []string{"_loop_loop", "block_count=6"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint содержит враньё %q: %q", unwanted, hint)
		}
	}
}

// TestR712_PrePatchArch_VisitsVsPhysical — кейс (б): arch inkling2 (до патча),
// block_count=48 — визиты, физических блоков 24, падение на blk.24.
//
// Поведение прежнее (ре-экспорт как inkling2_loop с block_count=24), но
// формулировка обязана атрибутировать числа: 24 — из имени отсутствующего
// тензора, 48 — из заголовка, и назвать оба пути (скрипт и штатный конвертер).
func TestR712_PrePatchArch_VisitsVsPhysical(t *testing.T) {
	withSchemaLangR712(t)
	const model = "Inkling-2-Mini-prepatch-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "inkling2", []ggufUint32R712{
		{"inkling2.block_count", 48},
		{"inkling2.num_loops", 3},
		{"inkling2.loop.core_start", 6},
		{"inkling2.loop.core_end", 18},
	}))

	body := schemaRespondR712(t, model, liveInklingSchemaErrTextR704)

	if got := bodyStringR712(body, "arch"); got != "inkling2" {
		t.Errorf("arch = %q, want inkling2", got)
	}
	if got, _ := body["block_count"].(float64); got != 48 {
		t.Errorf("block_count = %v, want 48 (визиты из заголовка)", body["block_count"])
	}
	if got, _ := body["physical_blocks"].(float64); got != 24 {
		t.Errorf("physical_blocks = %v, want 24 (по имени отсутствующего тензора blk.24; у до-патчевого файла другого источника нет)",
			body["physical_blocks"])
	}

	hint := bodyStringR712(body, "hint")
	for _, want := range []string{
		"inkling2_loop",
		"block_count=24",
		"по имени отсутствующего тензора",
		"scripts/gguf_loop_meta_patch.py --fuse-loop-queries",
		"конвертером llama.cpp",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint не содержит %q: %q", want, hint)
		}
	}
	for _, unwanted := range []string{"_loop_loop", "block_count=6", "уже в loop-схеме"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint содержит враньё %q: %q", unwanted, hint)
		}
	}

	msg := bodyStringR712(body, "error")
	for _, want := range []string{"физических блоков: 24", "по имени отсутствующего тензора", "48", "num_loops"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error не атрибутирует число %q: %q", want, msg)
		}
	}
}

// TestR712_LoopSchema_OtherMissingTensorNeedsFullReexport — файл уже в
// loop-схеме, но пропал НЕ loop_msg_query (здесь blk.3.attn_q.weight). Фолд
// запросов такой тензор не вернёт, поэтому подсказка обязана вести к ПОЛНОМУ
// ре-экспорту, а не к --fuse-loop-queries; физические блоки по-прежнему из
// заголовка (24), а не номер блока 3 из имени тензора.
func TestR712_LoopSchema_OtherMissingTensorNeedsFullReexport(t *testing.T) {
	withSchemaLangR712(t)
	const model = "Inkling-2-Mini-loop-other-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "inkling2_loop", []ggufUint32R712{
		{"inkling2_loop.block_count", 24},
		{"inkling2_loop.loop.start", 6},
		{"inkling2_loop.loop.end", 18},
		{"inkling2_loop.loop.count", 3},
	}))
	loadErr := "load model " + model + ": failed to load model\n" +
		"[llama.cpp] llama_model_load: error loading model: check_tensor_dims: tensor 'blk.3.attn_q.weight' not found"

	body := schemaRespondR712(t, model, loadErr)

	if got, _ := body["physical_blocks"].(float64); got != 24 {
		t.Errorf("physical_blocks = %v, want 24 (block_count из заголовка, не номер блока 3)", body["physical_blocks"])
	}
	hint := bodyStringR712(body, "hint")
	for _, want := range []string{"уже в loop-схеме", "полный ре-экспорт", "восстанавливает только"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint не ведёт к полному ре-экспорту (%q): %q", want, hint)
		}
	}
	for _, unwanted := range []string{"_loop_loop", "block_count=3"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint содержит враньё %q: %q", unwanted, hint)
		}
	}
	if msg := bodyStringR712(body, "error"); !strings.Contains(msg, "24") || !strings.Contains(msg, "loop-схема") {
		t.Errorf("error не объясняет loop-схему (24): %q", msg)
	}
}

// TestR712_OtherMissingTensor_GenericBranchUnchanged — кейс (в), сторож прежней
// ветки: НЕ-loop архитектура и отсутствующий тензор без номера блока (output.
// weight) — прежняя общая подсказка (ре-экспорт под схему + сборка), без
// loop-советов и без выдуманных чисел. Ветка R712 её не трогает.
func TestR712_OtherMissingTensor_GenericBranchUnchanged(t *testing.T) {
	withSchemaLangR712(t)
	const model = "qwen3-4b-broken-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "qwen3", []ggufUint32R712{
		{"qwen3.block_count", 42},
	}))

	body := schemaRespondR712(t, model, "load model "+model+": failed to load model\n"+
		"[llama.cpp] llama_model_load: error loading model: check_tensor_dims: tensor 'output.weight' not found")

	if got := bodyStringR712(body, "arch"); got != "qwen3" {
		t.Errorf("arch = %q, want qwen3", got)
	}
	if _, ok := body["physical_blocks"]; ok {
		t.Errorf("physical_blocks выдуманы для тензора без номера блока: %v", body["physical_blocks"])
	}
	hint := bodyStringR712(body, "hint")
	for _, want := range []string{"ре-экспорт GGUF под схему тензоров", "более новый загрузчик"} {
		if !strings.Contains(hint, want) {
			t.Errorf("общая подсказка изменилась, нет %q: %q", want, hint)
		}
	}
	for _, unwanted := range []string{"loop", "_loop", "gguf_loop_meta_patch.py"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("в общей подсказке появилось loop-враньё %q: %q", unwanted, hint)
		}
	}
	msg := bodyStringR712(body, "error")
	if !strings.Contains(msg, "схема тензоров, не совпадающая с ожиданиями движка") {
		t.Errorf("общий текст причины изменился: %q", msg)
	}
	if strings.Contains(msg, "loop-развёрнутая") {
		t.Errorf("общий текст причины приписал loop-схему файлу qwen3: %q", msg)
	}
}

// TestR712_NonLoopArch_BlockTensorNoLoopAdvice — тот же сторож на тензоре С
// номером блока, но у архитектуры, для которой loop-схемы не существует
// (qwen3, отсутствует blk.5.attn_q.weight): номер блока остаётся фактом в
// physical_blocks (как и до R712), но подсказка — общая. Прежняя подсказка
// предлагала «ре-экспорт как qwen3_loop с block_count=5» — то же враньё, что и
// в дефекте, только для чужой архитектуры; для не-inkling2 loop-совета больше
// нет.
func TestR712_NonLoopArch_BlockTensorNoLoopAdvice(t *testing.T) {
	withSchemaLangR712(t)
	const model = "qwen3-4b-truncated-R712"
	useSchemaBackendR712(t, newSchemaGGUFBackendR712(t, model, "qwen3", []ggufUint32R712{
		{"qwen3.block_count", 42},
	}))

	body := schemaRespondR712(t, model, "load model "+model+": failed to load model\n"+
		"[llama.cpp] llama_model_load: error loading model: check_tensor_dims: tensor 'blk.5.attn_q.weight' not found")

	if got, _ := body["physical_blocks"].(float64); got != 5 {
		t.Errorf("physical_blocks = %v, want 5 (номер блока из имени тензора — прежний факт)", body["physical_blocks"])
	}
	hint := bodyStringR712(body, "hint")
	if strings.Contains(hint, "_loop") || strings.Contains(hint, "loop-схем") {
		t.Errorf("не-loop архитектуре снова приписана loop-схема: %q", hint)
	}
	if !strings.Contains(hint, "ре-экспорт GGUF под схему тензоров") {
		t.Errorf("для не-loop архитектуры нужна общая подсказка: %q", hint)
	}
	msg := bodyStringR712(body, "error")
	if strings.Contains(msg, "loop-развёрнутая") {
		t.Errorf("loop-развёрнутая схема приписана архитектуре qwen3: %q", msg)
	}
}
