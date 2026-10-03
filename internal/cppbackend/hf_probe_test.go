package cppbackend

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- синтетический GGUF -----------------------------------------------------

type ggufKV struct {
	key   string
	value any
}

type ggufTensor struct {
	name string
	dims []uint64
}

// buildGGUF собирает минимальный, но валидный GGUF: magic/версия/счётчики, KV-пары
// и таблицу тензоров (имя + dims + тип + offset).
func buildGGUF(t *testing.T, kvs []ggufKV, tensors []ggufTensor) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(tensors)))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	writeStr := func(s string) {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(len(s)))
		buf.WriteString(s)
	}
	for _, kv := range kvs {
		writeStr(kv.key)
		switch v := kv.value.(type) {
		case string:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(8))
			writeStr(v)
		case uint32:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(4))
			_ = binary.Write(&buf, binary.LittleEndian, v)
		default:
			t.Fatalf("неподдержанный тип KV %T", kv.value)
		}
	}
	for _, tensor := range tensors {
		writeStr(tensor.name)
		dims := tensor.dims
		if len(dims) == 0 {
			dims = []uint64{4, 4}
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(dims)))
		for _, d := range dims {
			_ = binary.Write(&buf, binary.LittleEndian, d)
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // тип
		_ = binary.Write(&buf, binary.LittleEndian, uint64(0)) // offset
	}
	return buf.Bytes()
}

func names(list ...string) []ggufTensor {
	out := make([]ggufTensor, 0, len(list))
	for _, n := range list {
		out = append(out, ggufTensor{name: n})
	}
	return out
}

// buildSafetensors собирает safetensors-заголовок: uint64 длина + JSON.
func buildSafetensors(t *testing.T, meta map[string]string, tensorNames []string) []byte {
	t.Helper()
	doc := map[string]any{}
	if len(meta) > 0 {
		doc["__metadata__"] = meta
	}
	for _, n := range tensorNames {
		doc[n] = map[string]any{"dtype": "F16", "shape": []int{4, 4}, "data_offsets": []int{0, 32}}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(raw)))
	buf.Write(raw)
	return buf.Bytes()
}

// --- классификация: как движок увидит файл ----------------------------------

// РЕАЛЬНЫЙ ФАЙЛ (leejet/Qwen-Image-2.1-GGUF::qwen_image_2.1-Q4_0.gguf, заголовок
// с HF): «голые» diffusers-имена, general.architecture отсутствует. Проверено на
// движке pinned master-929-3f8527a: с --diffusion-model он печатает
// «Version: Qwen Image 2.1», с --model — «get sd version from file failed».
func TestClassify_QwenImage21_OfficialBuild(t *testing.T) {
	raw := buildGGUF(t, nil, names(
		"img_in.weight",
		"txt_in.text_norm.weight",
		"txt_in.in_layer.weight",
		"transformer_blocks.0.attn.to_q.weight",
		"transformer_blocks.0.img_mlp.gate_up.weight",
	))
	hdr, err := parseGGUFHeader(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	family, version, dit, reason := classifyTensors(hdr.normalizedSet())
	if family != "qwen_image" || version != "Qwen Image 2.1" {
		t.Fatalf("family=%q version=%q (reason: %s)", family, version, reason)
	}
	if !dit {
		t.Fatal("qwen_image — DiT-семейство: нужен --diffusion-model")
	}
	if !strings.Contains(reason, "diffusion-model") {
		t.Fatalf("причина должна говорить про --diffusion-model: %s", reason)
	}
}

// РЕАЛЬНЫЙ ФАЙЛ (abenzerps/Qwen-Image-2.1-Uncensored-GGUF::qwen-image-2.1-UC-Q4_0.gguf):
// ComfyUI-экспорт — general.architecture="qwen_image21" и РАЗДЕЛЬНЫЕ
// img_mlp.gate_layer/img_mlp.proj вместо fused gate_up. Движок поддерживает обе
// раскладки (qwen_image_2_1.hpp:38-43), поэтому вердикт тот же: файл читается.
func TestClassify_QwenImage21_ComfyUIExport(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "qwen_image21"}}, names(
		"img_in.weight",
		"txt_in.text_norm.weight",
		"transformer_blocks.0.img_mlp.gate_layer.weight",
		"transformer_blocks.0.img_mlp.proj.weight",
		"transformer_blocks.0.attn.to_q.weight",
	))
	hdr, err := parseGGUFHeader(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.Arch != "qwen_image21" {
		t.Fatalf("arch=%q", hdr.Arch)
	}
	family, version, dit, reason := classifyTensors(hdr.normalizedSet())
	if family != "qwen_image" || version != "Qwen Image 2.1" || !dit {
		t.Fatalf("family=%q version=%q dit=%v (reason: %s)", family, version, dit, reason)
	}
	// ГЛАВНОЕ: вердикт НЕ «движок это не прочитает» — раскладка ComfyUI читается.
	if strings.Contains(strings.ToLower(reason), "не читает") {
		t.Fatalf("пред-проверка не должна приговаривать ComfyUI-раскладку: %s", reason)
	}
}

func TestClassify_Flux(t *testing.T) {
	// РЕАЛЬНЫЙ ФАЙЛ: leejet/FLUX.1-schnell-gguf::flux1-schnell-q4_0.gguf.
	raw := buildGGUF(t, nil, names(
		"double_blocks.0.img_attn.qkv.weight",
		"single_blocks.0.linear1.weight",
		"img_in.weight",
		"txt_in.weight",
	))
	hdr, _ := parseGGUFHeader(raw)
	family, version, dit, _ := classifyTensors(hdr.normalizedSet())
	if family != "flux" || version != "Flux" || !dit {
		t.Fatalf("family=%q version=%q dit=%v", family, version, dit)
	}
}

func TestClassify_SD15AllInOne(t *testing.T) {
	// РЕАЛЬНЫЙ ФАЙЛ на стенде: models/image/sd15-q4/stable-diffusion-v1-5-pruned-emaonly-Q4_0.gguf
	// (имена уже с префиксом model.diffusion_model., token_embedding ne[0]=768).
	raw := buildGGUF(t, nil, []ggufTensor{
		{name: "model.diffusion_model.input_blocks.0.0.weight", dims: []uint64{3, 3, 4, 320}},
		{name: "model.diffusion_model.middle_block.1.proj_in.weight", dims: []uint64{320, 320}},
		{name: "cond_stage_model.transformer.text_model.embeddings.token_embedding.weight", dims: []uint64{768, 49408}},
		{name: "first_stage_model.decoder.conv_in.weight", dims: []uint64{4, 4}},
	})
	hdr, _ := parseGGUFHeader(raw)
	family, version, dit, reason := classifyTensors(hdr.normalizedSet())
	if family != "sd15" || version != "SD1.x" || dit {
		t.Fatalf("family=%q version=%q dit=%v (reason: %s)", family, version, dit, reason)
	}
	if !strings.Contains(reason, "--model") {
		t.Fatalf("all-in-one грузится через --model: %s", reason)
	}
}

func TestClassify_SD2ByTokenEmbeddingDim(t *testing.T) {
	raw := buildGGUF(t, nil, []ggufTensor{
		{name: "model.diffusion_model.input_blocks.0.0.weight", dims: []uint64{3, 3, 4, 320}},
		{name: "model.diffusion_model.middle_block.1.proj_in.weight", dims: []uint64{320, 320}},
		{name: "cond_stage_model.transformer.text_model.embeddings.token_embedding.weight", dims: []uint64{1024, 49408}},
	})
	hdr, _ := parseGGUFHeader(raw)
	family, _, _, _ := classifyTensors(hdr.normalizedSet())
	if family != "sd21" {
		t.Fatalf("family=%q, want sd21 (token_embedding ne[0]=1024)", family)
	}
}

func TestClassify_SDXLBySecondEncoder(t *testing.T) {
	raw := buildGGUF(t, nil, []ggufTensor{
		{name: "model.diffusion_model.input_blocks.0.0.weight", dims: []uint64{3, 3, 4, 320}},
		{name: "model.diffusion_model.middle_block.1.proj_in.weight", dims: []uint64{320, 320}},
		{name: "cond_stage_model.1.transformer.text_model.embeddings.token_embedding.weight", dims: []uint64{1280, 49408}},
	})
	hdr, _ := parseGGUFHeader(raw)
	family, version, _, _ := classifyTensors(hdr.normalizedSet())
	if family != "sdxl" || version != "SDXL" {
		t.Fatalf("family=%q version=%q, want sdxl", family, version)
	}
}

// VAE/text encoder/LoRA: якорей нет — пред-проверка обязана молчать, а не
// приговаривать файл («unsupported» мы не выносим вообще).
func TestClassify_VAEAndTextEncoderAreUnknown(t *testing.T) {
	for _, tc := range [][]string{
		{"encoder.down.0.block.0.norm1.weight", "decoder.conv_in.weight"},
		{"text_model.encoder.layers.0.self_attn.q_proj.weight"},
		{"lora_unet_down_blocks_0_layers_0.weight"},
	} {
		raw := buildGGUF(t, nil, names(tc...))
		hdr, _ := parseGGUFHeader(raw)
		family, version, dit, reason := classifyTensors(hdr.normalizedSet())
		if family != "" || version != "" || dit {
			t.Fatalf("%v: family=%q version=%q dit=%v, want пусто", tc, family, version, dit)
		}
		if reason == "" {
			t.Fatalf("%v: причина должна объяснять, почему вердикта нет", tc)
		}
	}
}

func TestClassify_TruncatedHeaderIsUnknown(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "flux"}}, names("double_blocks.0.x.weight"))
	cut := raw[:len(raw)-10]
	hdr, err := parseGGUFHeader(cut)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !hdr.Truncated {
		t.Fatal("ожидали Truncated=true на обрезанном заголовке")
	}
	_, version, _, _ := classifyTensors(hdr.normalizedSet())
	if version != "" {
		t.Fatalf("version=%q, want пусто: по обрезанному заголовку вердикта быть не может", version)
	}
}

func TestProbe_RejectsNonWeightFile(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("not a model at all, just text")
	if got := detectWeightFormat(buf.Bytes(), "README.md"); got != "unknown" {
		t.Fatalf("format=%q, want unknown", got)
	}
}

// --- safetensors ------------------------------------------------------------

func TestProbeSafetensors_FluxNamesDetected(t *testing.T) {
	raw := buildSafetensors(t, nil, []string{
		"double_blocks.0.img_attn.qkv.weight",
		"single_blocks.0.linear1.weight",
	})
	hdr, err := parseSafetensorsHeader(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.TensorCount != 2 {
		t.Fatalf("tensors=%d, want 2", hdr.TensorCount)
	}
	family, version, dit, _ := classifyTensors(hdr.normalizedSet())
	if family != "flux" || version != "Flux" || !dit {
		t.Fatalf("family=%q version=%q dit=%v", family, version, dit)
	}
}

func TestProbeSafetensors_VAEIsUnknown(t *testing.T) {
	raw := buildSafetensors(t, map[string]string{"modelspec.architecture": "stable-diffusion-xl-v1-base"},
		[]string{"encoder.down_blocks.0.resnets.0.norm1.weight"})
	hdr, _ := parseSafetensorsHeader(raw)
	if hdr.Arch == "" {
		t.Fatal("modelspec.architecture должен читаться")
	}
	family, version, _, reason := classifyTensors(hdr.normalizedSet())
	if family != "" || version != "" {
		t.Fatalf("family=%q version=%q: VAE не diffusion-файл", family, version)
	}
	if reason == "" {
		t.Fatal("нужна причина, почему вердикта нет")
	}
}

// --- HTTP: Range-запрос и вердикт -------------------------------------------

func TestProbeFile_UsesRangeAndReturnsVerdict(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "qwen_image21"}}, names(
		"txt_in.text_norm.weight",
		"transformer_blocks.0.img_mlp.gate_layer.weight",
		"transformer_blocks.0.img_mlp.proj.weight",
		"img_in.weight",
	))
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if !strings.Contains(r.URL.Path, "/resolve/main/") {
			t.Errorf("неожиданный путь: %s", r.URL.Path)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(raw)-1, len(raw)+999))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	d := NewHuggingFaceDownloader("", srv.URL, t.TempDir(), t.TempDir())
	res, err := d.ProbeFile(context.Background(), "abenzerps/Qwen-Image-2.1-Uncensored-GGUF", "qwen-image-2.1-UC-Q4_0.gguf", "main")
	if err != nil {
		t.Fatalf("ProbeFile: %v", err)
	}
	if gotRange == "" || !strings.HasPrefix(gotRange, "bytes=0-") {
		t.Fatalf("Range-заголовок не отправлен (%q) — иначе мы бы качали весь файл", gotRange)
	}
	if res.Verdict != HFProbeSupported {
		t.Fatalf("verdict=%q (reason: %s)", res.Verdict, res.Reason)
	}
	if res.Family != "qwen_image" || res.Version != "Qwen Image 2.1" || !res.DiT {
		t.Fatalf("family=%q version=%q dit=%v", res.Family, res.Version, res.DiT)
	}
	if res.Format != "gguf" || res.Arch != "qwen_image21" {
		t.Fatalf("format=%q arch=%q", res.Format, res.Arch)
	}
	if res.SizeBytes != int64(len(raw)+999) {
		t.Fatalf("size=%d, want %d (из Content-Range)", res.SizeBytes, len(raw)+999)
	}
}

func TestProbeFile_EmptyParams(t *testing.T) {
	d := NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())
	if _, err := d.ProbeFile(context.Background(), "", "x.gguf", "main"); err == nil {
		t.Fatal("ожидали ошибку на пустой modelId")
	}
	if _, err := d.ProbeFile(context.Background(), "a/b", "", "main"); err == nil {
		t.Fatal("ожидали ошибку на пустой filename")
	}
}

// Заголовок должен читаться быстро и не зависеть от размера файла: проверяем,
// что при 200 (без Range-поддержки) мы всё равно не вычитываем больше лимита.
func TestProbeFile_LimitsReadWhenRangeUnsupported(t *testing.T) {
	raw := buildGGUF(t, nil, names("double_blocks.0.img_attn.qkv.weight"))
	big := make([]byte, HFProbeRangeBytes*3)
	copy(big, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < len(big); i += 4096 {
			end := i + 4096
			if end > len(big) {
				end = len(big)
			}
			_, _ = w.Write(big[i:end])
		}
	}))
	defer srv.Close()

	d := NewHuggingFaceDownloader("", srv.URL, t.TempDir(), t.TempDir())
	res, err := d.ProbeFile(context.Background(), "a/b", "m.gguf", "main")
	if err != nil {
		t.Fatalf("ProbeFile: %v", err)
	}
	if res.Verdict != HFProbeSupported {
		t.Fatalf("verdict=%q (reason: %s)", res.Verdict, res.Reason)
	}
	if res.CheckedB > HFProbeRangeBytes {
		t.Fatalf("прочитано %d байт > лимита %d", res.CheckedB, HFProbeRangeBytes)
	}
}
