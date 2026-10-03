// hf_probe.go — пред-проверка файла модели на HuggingFace ДО скачивания:
// читаем только заголовок (Range-запрос) и говорим, КАК движок увидит этот файл.
//
// ЗАЧЕМ. Оператор скачал 4 ГБ GGUF и получил на загрузке «get sd version from
// file failed». Разбор (см. debug-сессию и docs/image-generation.md §8.2) показал
// важное: сам файл при этом РАБОЧИЙ. sd.cpp определяет версию модели по именам
// тензоров (ModelLoader::get_sd_version, src/model_loader.cpp), и имя тензоров
// зависит от того, КАК файл подключён:
//
//	--diffusion-model f.gguf  → все имена получают префикс "model.diffusion_model."
//	                            (diffusion_engine.cpp:733) — DiT-файлы (flux, sd3,
//	                            qwen_image) узнаются именно так;
//	--model f.gguf            → имена остаются как в файле (diffusion_engine.cpp:726);
//	                            так грузятся all-in-one чекпойнты (SD1.x/SD2/SDXL),
//	                            где имена уже с префиксом.
//
// Поэтому «голые» diffusers-имена (transformer_blocks.*, double_blocks.*) — это
// НЕ признак «чужого формата»: ровно так выглядят и официальные сборки leejet
// под sd.cpp. Ложный вердикт «движок это не прочитает» тут дороже отсутствия
// пометки, поэтому пред-проверка НЕ выносит приговоров: она сообщает ФАКТЫ
// (формат, архитектура, имя семейства так, как его узнает движок) и подсказывает,
// каким флагом файл нужно подключать.
//
// Сверка с движком (pinned master-929-3f8527a, контейнер ol-stack-imageworker):
// синтетические GGUF с реальными именами тензоров из HF — оба qwen-2.1 файла
// (leejet и city96/abenzerps-экспорт) дают «Version: Qwen Image 2.1» через
// --diffusion-model и «get sd version from file failed» через --model.
//
// Экспорт: ProbeFile, HFProbeResult, EngineAnchors (для тестов и docs).
package cppbackend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// HFProbeRangeBytes — сколько байт заголовка читаем. GGUF-метаданные (KV + описание
// тензоров) у диффузионных моделей занимают единицы-десятки килобайт; 512 КБ — с
// большим запасом и всё ещё «ничего» по сравнению с гигабайтами весов.
const HFProbeRangeBytes = 512 * 1024

// Вердикты пред-проверки.
//
// Их всего два, и это осознанно: «unsupported» мы не выносим. Заголовок не
// содержит всего, что нужно движку (часть решений — по размерностям и по
// комбинациям тензоров), поэтому честный ответ бывает либо «распознаётся как X»,
// либо «по заголовку не определить».
const (
	HFProbeSupported = "supported" // семейство/версия движка узнаны по именам тензоров
	HFProbeUnknown   = "unknown"   // по заголовку вердикта нет
)

// engineAnchor — правило узнавания семейства: имя тензора, которое ищет
// get_sd_version (src/model_loader.cpp pinned master-929-3f8527a).
//
// ПОРЯДОК ВАЖЕН: сначала узкие DiT-семейства, потом общие. Паттерны записаны в
// «сыром» виде (как в файле), потому что префикс model.diffusion_model. движок
// добавляет сам при загрузке через --diffusion-model.
type engineAnchor struct {
	Family  string   // наше семейство (pkg/types.ImageModelFamilies)
	Version string   // как назовёт движок (для человека)
	DiT     bool     // требует --diffusion-model (отдельные VAE/text encoder)
	Any     []string // достаточно любого совпадения подстроки
}

// EngineAnchors — таблица якорей. Экспортируется для тестов и документации.
var EngineAnchors = []engineAnchor{
	{Family: "qwen_image", Version: "Qwen Image 2.1", DiT: true, Any: []string{"txt_in.text_norm.weight"}},
	{Family: "qwen_image", Version: "Qwen Image", DiT: true, Any: []string{"transformer_blocks.0.img_mod.1.weight"}},
	{Family: "flux2", Version: "Flux 2", DiT: true, Any: []string{"double_stream_modulation_img.lin.weight"}},
	{Family: "flux", Version: "Flux", DiT: true, Any: []string{"double_blocks.", "single_transformer_blocks."}},
	{Family: "sd3", Version: "SD3.x", DiT: true, Any: []string{"joint_blocks."}},
	{Family: "chroma", Version: "Chroma", DiT: true, Any: []string{"nerf_final_layer_conv."}},
	{Family: "z_image", Version: "Z-Image", DiT: true, Any: []string{"cap_embedder.0.weight"}},
	{Family: "other", Version: "Wan", DiT: true, Any: []string{"blocks.0.cross_attn.norm_k.weight"}},
	{Family: "other", Version: "PixArt", DiT: true, Any: []string{"t_block.1.weight"}},
	{Family: "other", Version: "LLaDA Image", DiT: true, Any: []string{"sigvq_embedder.1.weight"}},
	{Family: "other", Version: "Hunyuan Video", DiT: true, Any: []string{"txt_in.individual_token_refiner.blocks.0.adaLN_modulation.1.weight"}},
}

// diffusionPrefix — префикс, который движок сам добавляет к именам при загрузке
// через --diffusion-model. Имена в файле могут быть и с ним (all-in-one
// чекпойнты грузятся через --model), поэтому сравнение идёт по нормализованному
// имени — без этого префикса.
const diffusionPrefix = "model.diffusion_model."

// HFProbeResult — результат пред-проверки файла.
type HFProbeResult struct {
	ModelID   string   `json:"modelId"`
	Filename  string   `json:"filename"`
	Revision  string   `json:"revision"`
	SizeBytes int64    `json:"sizeBytes,omitempty"`
	Format    string   `json:"format"`                 // gguf | safetensors | unknown
	Verdict   string   `json:"verdict"`                // supported | unknown
	Family    string   `json:"family,omitempty"`       // семейство для профиля bundle
	Version   string   `json:"versionLabel,omitempty"` // как назовёт движок
	DiT       bool     `json:"dit,omitempty"`          // нужен --diffusion-model
	Arch      string   `json:"architecture,omitempty"`
	Tensors   int      `json:"tensorCount,omitempty"`
	Prefixes  []string `json:"tensorPrefixes,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	CheckedB  int      `json:"checkedBytes,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// tensorSet — имена тензоров из заголовка: нормализованное имя → первая
// размерность (0, если размерности не читались, например у safetensors).
type tensorSet struct {
	dims map[string]int64
}

func (ts tensorSet) has(sub string) bool {
	for name := range ts.dims {
		if strings.Contains(name, sub) {
			return true
		}
	}
	return false
}

// dim0 — первая размерность первого тензора, чьё имя содержит sub.
func (ts tensorSet) dim0(sub string) int64 {
	for name, d := range ts.dims {
		if strings.Contains(name, sub) {
			return d
		}
	}
	return 0
}

// ProbeFile читает заголовок файла в HF и определяет, как его увидит движок.
func (d *HuggingFaceDownloader) ProbeFile(ctx context.Context, modelID, filename, revision string) (*HFProbeResult, error) {
	modelID = strings.TrimSpace(modelID)
	filename = strings.TrimSpace(filename)
	if modelID == "" || filename == "" {
		return nil, fmt.Errorf("modelId and filename are required")
	}
	if revision == "" {
		revision = "main"
	}
	url := d.getResolveURL(modelID, filename, revision)
	req, err := d.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Range: нам нужен только заголовок. HF отдаёт 206 Partial Content; если
	// зеркало/прокси Range не поддержит — придёт 200 и мы просто оборвём чтение
	// на том же лимите (io.LimitReader), не вычитывая гигабайты.
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", HFProbeRangeBytes-1))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch header: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("header request failed: HTTP %d", resp.StatusCode)
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, HFProbeRangeBytes))
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	res := &HFProbeResult{
		ModelID:   modelID,
		Filename:  filename,
		Revision:  revision,
		CheckedB:  len(head),
		SizeBytes: parseContentRangeTotal(resp.Header.Get("Content-Range")),
	}
	res.Format = detectWeightFormat(head, filename)
	switch res.Format {
	case "gguf":
		hdr, perr := parseGGUFHeader(head)
		if perr != nil {
			res.Verdict = HFProbeUnknown
			res.Reason = "GGUF-заголовок не читается: " + perr.Error()
			return res, nil
		}
		res.Arch = hdr.Arch
		res.Tensors = hdr.TensorCount
		res.Prefixes = hdr.Prefixes
		res.Truncated = hdr.Truncated
		res.Family, res.Version, res.DiT, res.Reason = classifyTensors(hdr.normalizedSet())
	case "safetensors":
		hdr, perr := parseSafetensorsHeader(head)
		if perr != nil {
			res.Verdict = HFProbeUnknown
			res.Reason = "safetensors-заголовок не читается: " + perr.Error()
			return res, nil
		}
		res.Arch = hdr.Arch
		res.Tensors = hdr.TensorCount
		res.Prefixes = hdr.Prefixes
		res.Family, res.Version, res.DiT, res.Reason = classifyTensors(hdr.normalizedSet())
	default:
		res.Verdict = HFProbeUnknown
		res.Reason = "не похоже на файл весов (.gguf/.safetensors)"
		return res, nil
	}
	if res.Version != "" {
		res.Verdict = HFProbeSupported
	} else {
		res.Verdict = HFProbeUnknown
	}
	return res, nil
}

// classifyTensors — «как движок увидит этот файл»: семейство, название версии и
// нужен ли ему --diffusion-model. Пустой version = по заголовку не определить.
func classifyTensors(ts tensorSet) (family, version string, dit bool, reason string) {
	for _, a := range EngineAnchors {
		for _, pat := range a.Any {
			if ts.has(pat) {
				why := fmt.Sprintf("движок узнаёт семейство по тензору %q", pat)
				if a.DiT {
					why += "; файл нужно подключать как --diffusion-model (семейство DiT: отдельные VAE/text encoder)"
				} else {
					why += "; файл подключается как --model"
				}
				return a.Family, a.Version, a.DiT, why
			}
		}
	}

	// UNet-семейства (all-in-one чекпойнты): движок отличает их по размерности
	// токен-эмбеддинга и по числу текстовых энкодеров (model_loader.cpp:680-702).
	if ts.has("input_blocks.") || ts.has("middle_block.1.") || ts.has("unet.down_blocks.") {
		sdxl := ts.has("conditioner.embedders.1") || ts.has("cond_stage_model.1") || ts.has("te.1")
		dim := ts.dim0("token_embedding.weight")
		switch {
		case sdxl:
			return "sdxl", "SDXL", false, "UNet с двумя текстовыми энкодерами — движок подключит как --model"
		case dim == 768:
			return "sd15", "SD1.x", false, "UNet с токен-эмбеддингом 768 — движок подключит как --model"
		case dim == 1024:
			return "sd21", "SD2.x", false, "UNet с токен-эмбеддингом 1024 — движок подключит как --model"
		default:
			return "", "", false, "UNet-тензоры есть, но размерности токен-эмбеддинга в заголовке нет — версию не определить (движок решит по своей таблице)"
		}
	}

	// Ни один якорь не совпал: это может быть VAE, text encoder, LoRA — или
	// семейство, которого мы не знаем. Обещать тут нечего.
	return "", "", false, "в заголовке нет тензоров, по которым движок узнаёт версию модели (возможно, это VAE/text encoder/LoRA, а не diffusion-файл)"
}

// parseContentRangeTotal достаёт полный размер файла из "bytes 0-524287/4151573280".
func parseContentRangeTotal(v string) int64 {
	if v == "" {
		return 0
	}
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0
	}
	var total int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v[slash+1:]), "%d", &total); err != nil {
		return 0
	}
	return total
}

// detectWeightFormat определяет контейнер по magic (а имя — как подсказка).
func detectWeightFormat(head []byte, filename string) string {
	if len(head) >= 4 && string(head[:4]) == "GGUF" {
		return "gguf"
	}
	if len(head) >= 8 {
		// safetensors: uint64 LE = длина JSON-заголовка, дальше JSON, начинающийся с '{'
		n := binary.LittleEndian.Uint64(head[:8])
		if n > 0 && n <= uint64(len(head)-8) && head[8] == '{' {
			return "safetensors"
		}
	}
	return "unknown"
}

// ggufHeader — то, что нам нужно из GGUF-заголовка.
type ggufHeader struct {
	Arch        string
	Name        string
	TensorCount int
	Prefixes    []string
	// Dims — нормализованное имя тензора → первая размерность. Нужны для
	// UNet-семейств: движок отличает SD1.x от SD2.x по размерности эмбеддинга.
	Dims map[string]int64
	// Truncated=true, если файл (или прочитанный кусок) оборвался до таблицы тензоров:
	// тогда про раскладку мы ничего не знаем и вердикт должен быть осторожным.
	Truncated bool
}

// normalizedSet отдаёт имена без префикса model.diffusion_model. — так их видит
// движок при загрузке через --diffusion-model, и так же выглядят DiT-файлы на HF.
func (h ggufHeader) normalizedSet() tensorSet {
	set := tensorSet{dims: make(map[string]int64, len(h.Dims))}
	for name, d := range h.Dims {
		set.dims[strings.TrimPrefix(name, diffusionPrefix)] = d
	}
	return set
}

// parseGGUFHeader читает magic/версию/счётчики, KV-пары и имена тензоров.
//
// Читаем ТОЛЬКО начало: если таблица тензоров не поместилась в прочитанный кусок,
// возвращаем Truncated=true и пустые имена (вывод «раскладка неизвестна»).
func parseGGUFHeader(b []byte) (ggufHeader, error) {
	h := ggufHeader{Dims: map[string]int64{}}
	if len(b) < 24 {
		return h, fmt.Errorf("слишком короткий файл (%d байт)", len(b))
	}
	if string(b[:4]) != "GGUF" {
		return h, fmt.Errorf("нет magic GGUF")
	}
	off := 4
	version := binary.LittleEndian.Uint32(b[off:])
	off += 4
	if version < 2 || version > 3 {
		return h, fmt.Errorf("неизвестная версия GGUF %d", version)
	}
	tensorCount := binary.LittleEndian.Uint64(b[off:])
	off += 8
	kvCount := binary.LittleEndian.Uint64(b[off:])
	off += 8
	h.TensorCount = int(tensorCount)

	skip := func(n int) bool {
		if n < 0 || off+n > len(b) {
			h.Truncated = true
			return false
		}
		off += n
		return true
	}
	readString := func() (string, bool) {
		if off+8 > len(b) {
			h.Truncated = true
			return "", false
		}
		n := binary.LittleEndian.Uint64(b[off:])
		off += 8
		if n > uint64(len(b)-off) {
			h.Truncated = true
			return "", false
		}
		s := string(b[off : off+int(n)])
		off += int(n)
		return s, true
	}

	for i := uint64(0); i < kvCount; i++ {
		key, ok := readString()
		if !ok {
			return h, nil
		}
		if off+4 > len(b) {
			h.Truncated = true
			return h, nil
		}
		vt := binary.LittleEndian.Uint32(b[off:])
		off += 4
		switch vt {
		case 8: // string
			v, ok := readString()
			if !ok {
				return h, nil
			}
			if key == "general.architecture" {
				h.Arch = v
			} else if key == "general.name" {
				h.Name = v
			}
		case 0, 1, 7:
			if !skip(1) {
				return h, nil
			}
		case 2, 3:
			if !skip(2) {
				return h, nil
			}
		case 4, 5, 6:
			if !skip(4) {
				return h, nil
			}
		case 10, 11, 12:
			if !skip(8) {
				return h, nil
			}
		case 9: // array
			if off+12 > len(b) {
				h.Truncated = true
				return h, nil
			}
			et := binary.LittleEndian.Uint32(b[off:])
			off += 4
			n := binary.LittleEndian.Uint64(b[off:])
			off += 8
			switch et {
			case 8:
				for j := uint64(0); j < n; j++ {
					if _, ok := readString(); !ok {
						return h, nil
					}
				}
			default:
				size := 0
				switch et {
				case 0, 1, 7:
					size = 1
				case 2, 3:
					size = 2
				case 4, 5, 6:
					size = 4
				case 10, 11, 12:
					size = 8
				default:
					return h, fmt.Errorf("неизвестный тип элемента массива %d у ключа %q", et, key)
				}
				if n > uint64(len(b)) || !skip(int(n)*size) {
					return h, nil
				}
			}
		default:
			return h, fmt.Errorf("неизвестный тип значения %d у ключа %q", vt, key)
		}
	}

	// Таблица тензоров: имя (string) + n_dims (uint32) + dims + type (uint32) + offset (uint64).
	seen := map[string]bool{}
	for i := uint64(0); i < tensorCount; i++ {
		name, ok := readString()
		if !ok {
			return h, nil
		}
		if off+4 > len(b) {
			h.Truncated = true
			return h, nil
		}
		nd := binary.LittleEndian.Uint32(b[off:])
		off += 4
		var dim0 int64
		if nd > 0 && off+8 <= len(b) {
			dim0 = int64(binary.LittleEndian.Uint64(b[off:]))
		}
		if !skip(int(nd) * 8) {
			return h, nil
		}
		if !skip(4 + 8) {
			return h, nil
		}
		h.Dims[name] = dim0
		if p := tensorPrefix(name); p != "" && !seen[p] {
			seen[p] = true
		}
	}
	h.Prefixes = make([]string, 0, len(seen))
	for p := range seen {
		h.Prefixes = append(h.Prefixes, p)
	}
	sort.Strings(h.Prefixes)
	return h, nil
}

// tensorPrefix — первый компонент имени тензора («transformer_blocks» из
// «transformer_blocks.0.attn.to_q.weight»).
func tensorPrefix(name string) string {
	if i := strings.Index(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

// sfHeader — то, что нужно из safetensors-заголовка.
type sfHeader struct {
	Arch         string
	TensorCount  int
	Prefixes     []string
	Dims         map[string]int64
	MetadataKeys []string
}

func (h sfHeader) normalizedSet() tensorSet {
	set := tensorSet{dims: make(map[string]int64, len(h.Dims))}
	for name, d := range h.Dims {
		set.dims[strings.TrimPrefix(name, diffusionPrefix)] = d
	}
	return set
}

// parseSafetensorsHeader читает JSON-заголовок safetensors (8 байт длины + JSON).
//
// У safetensors нет «архитектуры» как таковой: есть __metadata__ (иногда с
// modelspec.architecture) и имена тензоров — по ним и работает движок.
func parseSafetensorsHeader(b []byte) (sfHeader, error) {
	h := sfHeader{Dims: map[string]int64{}}
	if len(b) < 9 {
		return h, fmt.Errorf("слишком короткий файл (%d байт)", len(b))
	}
	n := binary.LittleEndian.Uint64(b[:8])
	if n == 0 || n > uint64(len(b)-8) {
		return h, fmt.Errorf("длина JSON-заголовка вне прочитанного куска (%d байт)", n)
	}
	raw := b[8 : 8+int(n)]
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return h, fmt.Errorf("JSON-заголовок: %w", err)
	}
	seen := map[string]bool{}
	for key, val := range doc {
		if key == "__metadata__" {
			var meta map[string]string
			if json.Unmarshal(val, &meta) == nil {
				for mk, mv := range meta {
					h.MetadataKeys = append(h.MetadataKeys, mk)
					if strings.EqualFold(mk, "modelspec.architecture") {
						h.Arch = strings.TrimSpace(mv)
					}
				}
			}
			continue
		}
		h.TensorCount++
		var info struct {
			Shape []int64 `json:"shape"`
		}
		// Shape в safetensors — в порядке torch (строка-мажор), а ggml/движок
		// считает ne[0] последней размерностью: для token_embedding [49408, 768]
		// движок видит ne[0] = 768. Поэтому берём ПОСЛЕДНЮЮ размерность.
		if json.Unmarshal(val, &info) == nil && len(info.Shape) > 0 {
			h.Dims[key] = info.Shape[len(info.Shape)-1]
		} else {
			h.Dims[key] = 0
		}
		if p := tensorPrefix(key); p != "" {
			seen[p] = true
		}
	}
	sort.Strings(h.MetadataKeys)
	for p := range seen {
		h.Prefixes = append(h.Prefixes, p)
	}
	sort.Strings(h.Prefixes)
	return h, nil
}
