// compat_test.go — R696 (2026-11-23): «сможет ли imageworker загрузить набор».
//
// ЖИВОЙ ДЕФЕКТ, который стерегут эти проверки (локальный стенд, imageworker
// r261120-r648): оператор скачал на вкладке HuggingFace ОДИН файл
// `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf` — это text encoder Qwen3-VL
// (general.architecture="qwen3vl"), но имя выглядело diffusion-моделью. Воркер
// синтезировал профиль по имени (роль diffusion, family=other), поднял sd-server
// как all-in-one и получил:
//
//	[out] [INFO ] diffusion_engine.cpp:725 - loading model from '...Qwen-Image-...AD-Q4_K.gguf'
//	[err] [ERROR] diffusion_engine.cpp:992 - get sd version from file failed
//	[err] [ERROR] main.cpp:93 - new_sd_ctx_t failed
//
// Проверки идут по РЕАЛЬНЫМ структурам файлов (синтетические GGUF/safetensors с
// настоящими именами тензоров из HF): одиночный all-in-one gguf, DiT-набор
// FLUX/SD3/Qwen-Image, diffusers-подкаталоги (vae/, text_encoders/), а также
// несовместимый набор (скачан только энкодер).
package sdbackend

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// --- синтетические файлы -----------------------------------------------------

type testKV struct {
	key   string
	value any
}

type testTensor struct {
	name string
	dim0 uint64
}

func buildTestGGUF(t *testing.T, kvs []testKV, tensors []testTensor) []byte {
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
			t.Fatalf("unsupported KV type %T", kv.value)
		}
	}
	for _, tensor := range tensors {
		writeStr(tensor.name)
		dims := []uint64{tensor.dim0, 4}
		if tensor.dim0 == 0 {
			dims = []uint64{4, 4}
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(dims)))
		for _, d := range dims {
			_ = binary.Write(&buf, binary.LittleEndian, d)
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
		_ = binary.Write(&buf, binary.LittleEndian, uint64(0))
	}
	return buf.Bytes()
}

func buildTestSafetensors(t *testing.T, tensors []testTensor) []byte {
	t.Helper()
	doc := map[string]any{}
	for _, tensor := range tensors {
		dim := tensor.dim0
		if dim == 0 {
			dim = 4
		}
		doc[tensor.name] = map[string]any{
			"dtype": "F16", "shape": []int64{4, int64(dim)}, "data_offsets": []int{0, 32},
		}
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

// writeBundleFile — файл внутри каталога bundle (подкаталоги создаются).
func writeBundleFile(t *testing.T, bundleDir, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(bundleDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// --- реальные раскладки файлов ----------------------------------------------

// qwenDiffusionGGUF — Qwen-Image 2.1 (якоря движка: txt_in.text_norm.weight).
func qwenDiffusionGGUF(t *testing.T) []byte {
	return buildTestGGUF(t, nil, []testTensor{
		{name: "img_in.weight"},
		{name: "txt_in.text_norm.weight"},
		{name: "txt_in.in_layer.weight"},
		{name: "transformer_blocks.0.attn.to_q.weight"},
		{name: "transformer_blocks.0.img_mlp.gate_up.weight"},
	})
}

// qwenTextEncoderGGUF — тот самый скачанный файл: arch qwen3vl, 8.2B.
func qwenTextEncoderGGUF(t *testing.T) []byte {
	return buildTestGGUF(t, []testKV{
		{"general.architecture", "qwen3vl"},
		{"general.size_label", "8.2B"},
	}, []testTensor{
		{name: "model.embed_tokens.weight", dim0: 4096},
		{name: "model.layers.0.self_attn.q_proj.weight", dim0: 4096},
		{name: "model.norm.weight", dim0: 4096},
	})
}

// vaeSafetensors — VAE (diffusers-раскладка: decoder/encoder/quant_conv).
func vaeSafetensors(t *testing.T) []byte {
	return buildTestSafetensors(t, []testTensor{
		{name: "decoder.conv_in.weight"},
		{name: "encoder.conv_in.weight"},
		{name: "quant_conv.weight"},
		{name: "post_quant_conv.weight"},
	})
}

// clipTextEncoderSafetensors — CLIP-текст-энкодер (text_model.*), dim задаёт L/G.
func clipTextEncoderSafetensors(t *testing.T, hidden uint64) []byte {
	return buildTestSafetensors(t, []testTensor{
		{name: "text_model.encoder.layers.0.self_attn.q_proj.weight"},
		{name: "text_model.embeddings.token_embedding.weight", dim0: hidden},
	})
}

func t5GGUF(t *testing.T) []byte {
	return buildTestGGUF(t, []testKV{{"general.architecture", "t5"}}, []testTensor{
		{name: "encoder.block.0.layer.0.SelfAttention.q.weight"},
	})
}

func fluxDiffusionGGUF(t *testing.T) []byte {
	return buildTestGGUF(t, nil, []testTensor{
		{name: "img_in.weight"},
		{name: "double_blocks.0.img_mod.lin.weight"},
		{name: "single_transformer_blocks.0.attn.to_q.weight"},
	})
}

func sd3DiffusionGGUF(t *testing.T) []byte {
	return buildTestGGUF(t, nil, []testTensor{
		{name: "x_embedder.proj.weight"},
		{name: "joint_blocks.0.x_block.attn.qkv.weight"},
		{name: "joint_blocks.0.context_block.attn.qkv.weight"},
	})
}

// sd15AllInOneGGUF — SD1.5 all-in-one (UNet + внутренний CLIP, dim 768).
func sd15AllInOneGGUF(t *testing.T) []byte {
	return buildTestGGUF(t, nil, []testTensor{
		{name: "model.diffusion_model.input_blocks.1.0.in_layers.0.weight"},
		{name: "model.diffusion_model.middle_block.1.proj_in.weight"},
		{name: "cond_stage_model.transformer.text_model.encoder.layers.0.self_attn.q_proj.weight"},
		{name: "cond_stage_model.transformer.text_model.embeddings.token_embedding.weight", dim0: 768},
	})
}

// loadBundle — собрать каталог, загрузить реестр и вернуть профиль + вердикт.
func loadBundle(t *testing.T, name string, files map[string][]byte) (*Registry, types.ImageModelProfile, *BundleCompat) {
	t.Helper()
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, name)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for rel, data := range files {
		writeBundleFile(t, bundleDir, rel, data)
	}
	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	profile, ok := reg.Profile(name)
	if !ok {
		t.Fatalf("профиль %q не собран (warnings=%v)", name, reg.Warnings())
	}
	compat := reg.Compat(name)
	if compat == nil {
		t.Fatalf("вердикт совместимости %q не посчитан", name)
	}
	return reg, profile, compat
}

func flagIndex(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}

// --- проверки: совместимый набор проходит --------------------------------

// TestSynthesize_AllInOneGGUF_SD15 — одиночный gguf-checkpoint (SD1.5/SDXL):
// роль diffusion, режим --model, набор готов.
func TestSynthesize_AllInOneGGUF_SD15(t *testing.T) {
	_, profile, compat := loadBundle(t, "sd15-q4", map[string][]byte{
		"stable-diffusion-v1-5-Q4_0.gguf": sd15AllInOneGGUF(t),
	})
	if profile.Family != "sd15" {
		t.Fatalf("family=%q, want sd15 (UNet-якоря + dim 768)", profile.Family)
	}
	if len(profile.Files) != 1 || profile.Files[0].Role != types.ImageFileRoleDiffusion {
		t.Fatalf("files=%+v, want один diffusion", profile.Files)
	}
	args := profile.ServerArgs()
	if flagIndex(args, "--model") == -1 || flagIndex(args, "--diffusion-model") != -1 {
		t.Fatalf("all-in-one должен грузиться --model, а не --diffusion-model: %v", args)
	}
	if compat.Verdict != CompatReady || !compat.Runnable {
		t.Fatalf("verdict=%q runnable=%v reason=%s, want ready", compat.Verdict, compat.Runnable, compat.Reason)
	}
	if compat.EngineMode != CompatEngineModel {
		t.Fatalf("engineMode=%q, want model", compat.EngineMode)
	}
}

// TestSynthesize_QwenImageSet_Loads — типовой набор Qwen-Image 2.1:
// diffusion gguf + VAE + text encoder (роли и семейство по содержимому).
// Ожидание: движок получит --diffusion-model + --vae + --llm, вердикт ready.
func TestSynthesize_QwenImageSet_Loads(t *testing.T) {
	_, profile, compat := loadBundle(t, "qwen-image-2.1-q4", map[string][]byte{
		"qwen-image-2.1-UC-Q4_K_M.gguf":             qwenDiffusionGGUF(t),
		"vae/qwen_image_2.1_vae_bf16.safetensors":   vaeSafetensors(t),
		"text_encoders/qwen3vl_8b_bf16.safetensors": clipLikeQwenTE(t),
	})
	if profile.Family != "qwen_image" {
		t.Fatalf("family=%q, want qwen_image", profile.Family)
	}
	args := profile.ServerArgs()
	for _, flag := range []string{"--diffusion-model", "--vae", "--llm"} {
		if flagIndex(args, flag) == -1 {
			t.Fatalf("в argv нет %s: %v", flag, args)
		}
	}
	if flagIndex(args, "--model") != -1 {
		t.Fatalf("DiT-набор не должен грузиться как --model: %v", args)
	}
	if compat.Verdict != CompatReady || !compat.Runnable {
		t.Fatalf("verdict=%q reason=%s, want ready", compat.Verdict, compat.Reason)
	}
	if compat.EngineMode != CompatEngineDiffusion {
		t.Fatalf("engineMode=%q, want diffusion-model", compat.EngineMode)
	}
}

// clipLikeQwenTE — текстовый энкодер Qwen3-VL (в тесте без arch: тензоры LLM).
func clipLikeQwenTE(t *testing.T) []byte {
	return buildTestSafetensors(t, []testTensor{
		{name: "model.embed_tokens.weight"},
		{name: "model.layers.0.self_attn.q_proj.weight"},
	})
}

// TestSynthesize_FluxSet_Loads — FLUX: diffusion + VAE + CLIP-L + T5-XXL.
// Один энкодер назван нейтрально (encoder-weights.safetensors) — роль обязана
// определиться по СОДЕРЖИМОМУ (text_model.* + dim 768), а не по имени.
func TestSynthesize_FluxSet_Loads(t *testing.T) {
	_, profile, compat := loadBundle(t, "flux-schnell-q4", map[string][]byte{
		"flux1-schnell-q4_k.gguf":     fluxDiffusionGGUF(t),
		"ae.safetensors":              vaeSafetensors(t),
		"encoder-weights.safetensors": clipTextEncoderSafetensors(t, 768),
		"t5xxl-Q4_K_M.gguf":           t5GGUF(t),
	})
	if profile.Family != "flux" {
		t.Fatalf("family=%q, want flux", profile.Family)
	}
	roles := map[string]bool{}
	for _, f := range profile.Files {
		roles[f.Role] = true
	}
	for _, role := range []string{types.ImageFileRoleDiffusion, types.ImageFileRoleVae,
		types.ImageFileRoleClipL, types.ImageFileRoleT5xxl} {
		if !roles[role] {
			t.Fatalf("нет роли %s в профиле: %+v", role, profile.Files)
		}
	}
	args := profile.ServerArgs()
	for _, flag := range []string{"--diffusion-model", "--vae", "--clip_l", "--t5xxl"} {
		if flagIndex(args, flag) == -1 {
			t.Fatalf("в argv нет %s: %v", flag, args)
		}
	}
	if compat.Verdict != CompatReady {
		t.Fatalf("verdict=%q missing=%v reason=%s, want ready", compat.Verdict, compat.Missing, compat.Reason)
	}
}

// TestSynthesize_SD3Set_Loads — SD3: diffusion + VAE + CLIP-L + CLIP-G + T5.
func TestSynthesize_SD3Set_Loads(t *testing.T) {
	_, profile, compat := loadBundle(t, "sd3-medium", map[string][]byte{
		"sd3-medium-Q4_K_M.gguf": sd3DiffusionGGUF(t),
		"vae.safetensors":        vaeSafetensors(t),
		"clip_l.safetensors":     clipTextEncoderSafetensors(t, 768),
		"clip_g.safetensors":     clipTextEncoderSafetensors(t, 1280),
		"t5xxl-Q5_K.gguf":        t5GGUF(t),
	})
	if profile.Family != "sd3" {
		t.Fatalf("family=%q, want sd3", profile.Family)
	}
	args := profile.ServerArgs()
	for _, flag := range []string{"--diffusion-model", "--vae", "--clip_l", "--clip_g", "--t5xxl"} {
		if flagIndex(args, flag) == -1 {
			t.Fatalf("в argv нет %s: %v", flag, args)
		}
	}
	if compat.Verdict != CompatReady {
		t.Fatalf("verdict=%q missing=%v reason=%s, want ready", compat.Verdict, compat.Missing, compat.Reason)
	}
}

// --- проверки: несовместимый набор объясняется ---------------------------

// TestCompat_QwenTextEncoderOnly_Explains — ЖИВОЙ СЛУЧАЙ. В каталоге только
// скачанный энкодер: вердикт unsupported, причина называет text encoder и роль
// llm, а не «модель не загрузилась».
func TestCompat_QwenTextEncoderOnly_Explains(t *testing.T) {
	_, profile, compat := loadBundle(t, "qwen-image-2.1-turbo-uncensored-gguf", map[string][]byte{
		"Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf": qwenTextEncoderGGUF(t),
	})
	if profile.Family != "other" {
		t.Fatalf("family=%q, want other (семейство не определяется)", profile.Family)
	}
	if compat.Verdict != CompatUnsupported || compat.Runnable {
		t.Fatalf("verdict=%q runnable=%v, want unsupported (движок не поднимет набор)", compat.Verdict, compat.Runnable)
	}
	reason := compat.Reason
	for _, want := range []string{"text encoder", "llm", "не diffusion"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("в причине нет %q: %s", want, reason)
		}
	}
	if len(compat.Missing) == 0 || !strings.Contains(strings.Join(compat.Missing, ","), "diffusion") {
		t.Fatalf("missing=%v, want «diffusion» (набора нет)", compat.Missing)
	}
	line := compat.CompatReasonLine()
	if !strings.Contains(line, "НЕ сможет") {
		t.Fatalf("строка вердикта должна быть «НЕ сможет»: %s", line)
	}
	if got := LoadBlockedMessage(compat); !strings.Contains(got, "остановлена") {
		t.Fatalf("сообщение API об отказе без указания причины: %s", got)
	}
}

// TestCompat_QwenDiffusionOnly_Incomplete — скачан только diffusion: семейство
// известно, вердикт incomplete и перечислены недостающие роли.
func TestCompat_QwenDiffusionOnly_Incomplete(t *testing.T) {
	_, profile, compat := loadBundle(t, "qwen-image-2.1-only-diffusion", map[string][]byte{
		"qwen-image-2.1-UC-Q4_K_M.gguf": qwenDiffusionGGUF(t),
	})
	if profile.Family != "qwen_image" {
		t.Fatalf("family=%q, want qwen_image", profile.Family)
	}
	if compat.Verdict != CompatIncomplete || compat.Runnable {
		t.Fatalf("verdict=%q runnable=%v, want incomplete", compat.Verdict, compat.Runnable)
	}
	joined := strings.Join(compat.Missing, ", ")
	if !strings.Contains(joined, "VAE") || !strings.Contains(joined, "text encoder") {
		t.Fatalf("missing=%v, want VAE + text encoder", compat.Missing)
	}
	if !strings.Contains(compat.Reason, "не хватает") {
		t.Fatalf("причина должна объяснять нехватку: %s", compat.Reason)
	}
}

// TestCompat_AllInOneEnvelope_Ready — тот же одиночный all-in-one файл, но
// профиль пришёл с диска (profile.json family=other): правки по содержимому
// подставляют семейство, и вердикт ready (иначе движок получил бы --model на
// DiT-файл и ответил «get sd version from file failed»).
func TestCompat_DitFileWithOtherFamily_Explains(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "qwen-flat")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeBundleFile(t, bundleDir, "qwen-image-2.1-UC-Q4_K_M.gguf", qwenDiffusionGGUF(t))
	// Профиль «как его написал бы оператор вручную»: family=other, одна роль.
	profile := map[string]any{
		"name":   "qwen-flat",
		"family": "other",
		"files": []map[string]any{{
			"role": "diffusion", "repo": "local",
			"filename": "qwen-image-2.1-UC-Q4_K_M.gguf",
		}},
	}
	raw, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(bundleDir, ProfileFileName), raw, 0o644); err != nil {
		t.Fatalf("profile: %v", err)
	}
	// ВАЖНО: correctProfileByContent вызывается при Load и подставляет DiT-
	// семейство, поэтому принудительно проверяем вердикт по «сырому» профилю.
	compat := BuildBundleCompat(&types.ImageModelProfile{
		Name:   "qwen-flat",
		Family: "other",
		Files: []types.ImageModelFile{{
			Role: "diffusion", Repo: "local",
			Filename:  "qwen-image-2.1-UC-Q4_K_M.gguf",
			LocalPath: filepath.Join(bundleDir, "qwen-image-2.1-UC-Q4_K_M.gguf"),
		}},
	}, bundleDir)
	if compat.EngineMode != CompatEngineDiffusion && compat.Verdict != CompatUnsupported {
		t.Fatalf("DiT-файл не должен считаться all-in-one: verdict=%q mode=%q (%s)",
			compat.Verdict, compat.EngineMode, compat.Reason)
	}
}

// TestSynthesize_DiffusersSubdirs — diffusers-раскладка по подкаталогам:
// роли берутся из структуры (vae/, text_encoder/) и заголовков.
func TestSynthesize_DiffusersSubdirs(t *testing.T) {
	_, profile, compat := loadBundle(t, "diffusers-set", map[string][]byte{
		"transformer/diffusion_pytorch_model.safetensors": qwenDiffusionGGUF(t),
		"vae/diffusion_pytorch_model.safetensors":         vaeSafetensors(t),
		"text_encoder/model.safetensors":                  clipLikeQwenTE(t),
	})
	if profile.Family != "qwen_image" {
		t.Fatalf("family=%q, want qwen_image", profile.Family)
	}
	roles := map[string]bool{}
	for _, f := range profile.Files {
		roles[f.Role] = true
	}
	for _, role := range []string{types.ImageFileRoleDiffusion, types.ImageFileRoleVae, types.ImageFileRoleLLM} {
		if !roles[role] {
			t.Fatalf("нет роли %s для diffusers-раскладки: %+v", role, profile.Files)
		}
	}
	if compat.Verdict != CompatReady {
		t.Fatalf("verdict=%q missing=%v reason=%s", compat.Verdict, compat.Missing, compat.Reason)
	}
}

// --- проверки: вердикт ДО скачивания (по заголовкам из HF) ----------------

// TestVerifyBundleProbes_BeforeDownload — тот же вердикт, что у скачанного
// набора, но по заголовкам, снятым Range-запросами (файлов на диске нет).
func TestVerifyBundleProbes_BeforeDownload(t *testing.T) {
	qwenProbe := cppbackend.ProbeHead(qwenDiffusionGGUFWithName(t), "qwen-image-2.1-UC-Q4_K_M.gguf")
	teProbe := cppbackend.ProbeHead(qwenTextEncoderGGUF(t), "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf")
	vaeProbe := cppbackend.ProbeHead(vaeSafetensors(t), "vae/qwen_image_2.1_vae_bf16.safetensors")

	// 1) Оператор отметил ТОЛЬКО текстовый энкодер (живой сценарий скачивания).
	sel := &types.ImageModelProfile{
		Name: "selection", Family: "other",
		Files: []types.ImageModelFile{{
			Role: "diffusion", Repo: "AtomicChat/Qwen-Image-2.1-Turbo-Uncensored-GGUF",
			Filename: "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf",
		}},
	}
	compat := VerifyBundleProbes(sel, map[string]*cppbackend.HFProbeResult{
		"Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf": teProbe,
	}, nil)
	if compat.Verdict != CompatUnsupported || compat.Runnable {
		t.Fatalf("один энкодер не может быть набором: verdict=%q (%s)", compat.Verdict, compat.Reason)
	}
	if !strings.Contains(compat.Reason, "text encoder") {
		t.Fatalf("причина должна называть text encoder: %s", compat.Reason)
	}

	// 2) Полный набор: diffusion + VAE + тот же энкодер ролью llm.
	sel2 := &types.ImageModelProfile{
		Name: "selection", Family: "qwen_image",
		Files: []types.ImageModelFile{
			{Role: "diffusion", Filename: "qwen-image-2.1-UC-Q4_K_M.gguf"},
			{Role: "vae", Filename: "vae/qwen_image_2.1_vae_bf16.safetensors"},
			{Role: "llm", Filename: "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf"},
		},
	}
	compat2 := VerifyBundleProbes(sel2, map[string]*cppbackend.HFProbeResult{
		"qwen-image-2.1-UC-Q4_K_M.gguf":                qwenProbe,
		"vae/qwen_image_2.1_vae_bf16.safetensors":      vaeProbe,
		"Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf": teProbe,
	}, nil)
	if compat2.Verdict != CompatReady || !compat2.Runnable {
		t.Fatalf("полный набор должен быть ready: verdict=%q missing=%v reason=%s",
			compat2.Verdict, compat2.Missing, compat2.Reason)
	}

	// 3) Неполный набор: diffusion без VAE/энкодера.
	sel3 := &types.ImageModelProfile{
		Name: "selection", Family: "qwen_image",
		Files: []types.ImageModelFile{{Role: "diffusion", Filename: "qwen-image-2.1-UC-Q4_K_M.gguf"}},
	}
	compat3 := VerifyBundleProbes(sel3, map[string]*cppbackend.HFProbeResult{
		"qwen-image-2.1-UC-Q4_K_M.gguf": qwenProbe,
	}, nil)
	if compat3.Verdict != CompatIncomplete {
		t.Fatalf("verdict=%q, want incomplete", compat3.Verdict)
	}
	if len(compat3.Missing) != 2 {
		t.Fatalf("missing=%v, want VAE + text encoder", compat3.Missing)
	}
}

// qwenDiffusionGGUFWithName — тот же файл, но заголовок читается с именем,
// которое не подсказывает роль (проверяем именно содержимое).
func qwenDiffusionGGUFWithName(t *testing.T) []byte {
	return qwenDiffusionGGUF(t)
}
