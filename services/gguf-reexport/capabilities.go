// capabilities.go — что этот сервис умеет ПРЯМО СЕЙЧАС.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ЭНДПОИНТ, А НЕ КОНСТАНТА В WEBUI. WebUI обязан отличать
// «модуля нет» от «модуль есть, но не умеет полный ре-экспорт»: первое — не
// показывать блок вовсе, второе — показать блок и честно выключить недоступную
// цель с причиной. Список умений зависит от сборки образа (torch ставится
// отдельным таргетом), поэтому источник истины — сервис, а не догадка фронтенда.
package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// targetSpec — цель ре-экспорта: одна операция, которую сервис умеет выполнить.
type targetSpec struct {
	ID                    string             `json:"id"`
	Label                 string             `json:"label"`
	Kind                  string             `json:"kind"` // metadata_patch | convert
	Architectures         []string           `json:"architectures"`
	RequiresSourceWeights bool               `json:"requires_source_weights"`
	Available             bool               `json:"available"`
	Reason                string             `json:"reason,omitempty"`
	Options               []targetOptionSpec `json:"options,omitempty"`
}

// targetOptionSpec — опция цели. Описывается данными, чтобы форма WebUI
// строилась из ответа сервиса, а не из захардкоженного списка полей.
type targetOptionSpec struct {
	ID      string      `json:"id"`
	Type    string      `json:"type"` // bool | string
	Default interface{} `json:"default"`
	Label   string      `json:"label"`
	Hint    string      `json:"hint,omitempty"`
}

// registry — цели + их доступность. Доступность меняется после пробы python
// (torch/конвертер), поэтому чтение и запись под замком.
type registry struct {
	mu    sync.RWMutex
	specs []targetSpec
}

func defaultTargets() []targetSpec {
	return []targetSpec{
		{
			ID:            "inkling2_loop",
			Label:         "Inkling-2 loop (схема inkling2_loop)",
			Kind:          "metadata_patch",
			Architectures: []string{"inkling2"},
			// Исходные веса НЕ нужны: это переупаковка метаданных и раскладка уже
			// лежащих в файле матриц запросов. Именно поэтому цель работает без
			// HF-чекпойнта и без torch.
			RequiresSourceWeights: false,
			Available:             true,
			Options: []targetOptionSpec{
				{ID: "fuse_loop_queries", Type: "bool", Default: true,
					Label: "Разложить матрицы запросов по блокам ядра",
					Hint:  "Без этого загрузчик дойдёт до отсутствующего blk.N.loop_msg_query.weight и упадёт"},
				{ID: "dry_run", Type: "bool", Default: false,
					Label: "Только план (файл не пишется)",
					Hint:  "Читает заголовок, показывает изменения и не занимает диск"},
				{ID: "force", Type: "bool", Default: false,
					Label: "Перезаписать существующий результат"},
				{ID: "verify", Type: "bool", Default: true,
					Label: "Проверить результат после записи",
					Hint:  "Сверяет схему, длины массивов и байты данных вне заменённых областей"},
			},
		},
		{
			ID:                    "hf_to_gguf",
			Label:                 "HF-чекпойнт в GGUF (convert_hf_to_gguf.py)",
			Kind:                  "convert",
			Architectures:         []string{"*"},
			RequiresSourceWeights: true,
			Available:             false,
			Reason:                "проверка возможностей python ещё не выполнена",
			Options: []targetOptionSpec{
				{ID: "outtype", Type: "string", Default: "q8_0",
					Label: "Тип выхода (f16, bf16, q8_0, q4_k_m)"},
				{ID: "dry_run", Type: "bool", Default: false, Label: "Только проверка входа"},
				{ID: "force", Type: "bool", Default: false, Label: "Перезаписать существующий результат"},
			},
		},
	}
}

var targetRegistry = &registry{specs: defaultTargets()}

func (r *registry) list() []targetSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]targetSpec, len(r.specs))
	copy(out, r.specs)
	return out
}

func (r *registry) byID(id string) (targetSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.specs {
		if s.ID == id {
			return s, true
		}
	}
	return targetSpec{}, false
}

// setAvailability меняет доступность цели и причину отказа.
func (r *registry) setAvailability(id string, available bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.specs {
		if r.specs[i].ID == id {
			r.specs[i].Available = available
			r.specs[i].Reason = reason
			return
		}
	}
}

func targetByID(id string) (targetSpec, bool) { return targetRegistry.byID(id) }

func targetIDs() []string {
	specs := targetRegistry.list()
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.ID)
	}
	return out
}

// pythonInfo — факты о python-части образа, добытые ПРОБОЙ, а не константой.
//
// ЗАЧЕМ ПРОБА. Образ собирается в двух вариантах (лёгкий и с torch), а один и тот
// же тег может быть собран кем угодно. Если бы возможности объявлялись по
// константе сборки, «полный ре-экспорт» мог бы оказаться доступной кнопкой,
// которая падает на `import torch`. Здесь возможность = факт: импорт удался.
type pythonInfo struct {
	OK               bool   `json:"ok"`
	PythonVersion    string `json:"python_version,omitempty"`
	TorchVersion     string `json:"torch_version,omitempty"`
	ConverterPath    string `json:"converter_path,omitempty"`
	ConverterPresent bool   `json:"converter_present"`
	DriverPath       string `json:"driver_path,omitempty"`
	PatchScript      string `json:"patch_script,omitempty"`
	Error            string `json:"error,omitempty"`
	ProbedAt         string `json:"probed_at,omitempty"`
}

// infoProbe — кэш пробы. Перезапрашивается не чаще InfoTTL, чтобы
// `GET /capabilities` (его дергает форма) не запускал python каждый раз.
type infoProbe struct {
	cfg config
	mu  sync.Mutex
	cur pythonInfo
	at  time.Time
}

func newInfoProbe(cfg config) *infoProbe {
	p := &infoProbe{cfg: cfg}
	p.cur = pythonInfo{Error: "проба ещё не выполнялась"}
	return p
}

// Get возвращает сведения, при необходимости перезапрашивая их.
func (p *infoProbe) Get(ctx context.Context) pythonInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < p.cfg.InfoTTL {
		return p.cur
	}
	p.cur = p.probe(ctx)
	p.at = time.Now()
	applyAvailability(p.cur)
	return p.cur
}

// Refresh — принудительная проба (нужна после изменения образа/драйвера).
func (p *infoProbe) Refresh(ctx context.Context) pythonInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cur = p.probe(ctx)
	p.at = time.Now()
	applyAvailability(p.cur)
	return p.cur
}

// probe запускает `python driver --info` и разбирает ответ.
//
// Ограничение времени здесь нужно: это не работа, а вопрос «ты живой».
func (p *infoProbe) probe(parent context.Context) pythonInfo {
	info := pythonInfo{DriverPath: p.cfg.DriverPath, ConverterPath: p.cfg.ConverterPath}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.cfg.PythonBin, p.cfg.DriverPath, "--info")
	out, err := cmd.Output()
	if err != nil {
		info.Error = "python-драйвер не ответил: " + err.Error()
		return info
	}
	// Драйвер печатает один JSON-объект; всё до первой `{` — служебный шум.
	text := strings.TrimSpace(string(out))
	if i := strings.Index(text, "{"); i > 0 {
		text = text[i:]
	}
	var parsed struct {
		OK          *bool  `json:"ok"`
		Python      string `json:"python"`
		Torch       string `json:"torch"`
		Converter   string `json:"converter"`
		ConverterOK bool   `json:"converter_present"`
		PatchScript string `json:"patch_script"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		info.Error = "ответ драйвера не разобран как JSON: " + err.Error()
		return info
	}
	info.OK = parsed.OK == nil || *parsed.OK
	info.PythonVersion = parsed.Python
	info.TorchVersion = parsed.Torch
	info.ConverterPath = firstNonEmptyStr(parsed.Converter, p.cfg.ConverterPath)
	info.ConverterPresent = parsed.ConverterOK
	info.PatchScript = parsed.PatchScript
	info.Error = parsed.Error
	info.ProbedAt = time.Now().UTC().Format(time.RFC3339)
	return info
}

// applyAvailability переводит сведения пробы в доступность целей.
func applyAvailability(info pythonInfo) {
	// inkling2_loop: нужен только python + скрипт патча. Проверяем честно.
	reason := ""
	avail := info.OK && info.PatchScript != ""
	if !info.OK {
		reason = "python-часть образа недоступна: " + info.Error
	} else if info.PatchScript == "" {
		reason = "в образе нет скрипта патча loop-метаданных (scripts/gguf_loop_meta_patch.py)"
	}
	targetRegistry.setAvailability("inkling2_loop", avail, reason)

	// hf_to_gguf: нужен torch И конвертер llama.cpp.
	switch {
	case !info.OK:
		targetRegistry.setAvailability("hf_to_gguf", false, "python-часть образа недоступна: "+info.Error)
	case info.TorchVersion == "":
		targetRegistry.setAvailability("hf_to_gguf", false,
			"образ собран без torch: полный ре-экспорт из HF-чекпойнта недоступен. "+
				"Метаданные и переупаковка GGUF работают и без него; для полного пути нужен образ, "+
				"собранный с WITH_TORCH=1 (см. docker/gguf-reexport/Dockerfile)")
	case !info.ConverterPresent:
		targetRegistry.setAvailability("hf_to_gguf", false,
			"в образе нет convert_hf_to_gguf.py (сабмодуль c/llama.cpp не попал в сборку)")
	default:
		targetRegistry.setAvailability("hf_to_gguf", true, "")
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// capabilitiesResponse — форма ответа /api/reexport/capabilities.
type capabilitiesResponse struct {
	Service       string                 `json:"service"`
	Available     bool                   `json:"available"`
	Version       map[string]interface{} `json:"version"`
	Converter     converterInfo          `json:"converter"`
	Architectures []archSpec             `json:"architectures"`
	Targets       []targetSpec           `json:"targets"`
	Limits        limitsInfo             `json:"limits"`
	Endpoints     map[string]string      `json:"endpoints"`
}

// converterInfo — версия конвертера, по которой оператор сверяет образ.
type converterInfo struct {
	Name        string `json:"name"`
	Revision    string `json:"revision,omitempty"`
	Date        string `json:"date,omitempty"`
	Driver      string `json:"driver,omitempty"`
	Python      string `json:"python,omitempty"`
	Torch       string `json:"torch,omitempty"`
	PatchScript string `json:"patch_script,omitempty"`
	TorchState  string `json:"torch_state"` // present | absent | unknown
}

// archSpec — поддерживаемые архитектуры/схемы.
type archSpec struct {
	ID      string `json:"id"`
	Scheme  string `json:"scheme"`
	Target  string `json:"target,omitempty"`
	Label   string `json:"label"`
	Note    string `json:"note,omitempty"`
	Builtin bool   `json:"builtin"`
}

type limitsInfo struct {
	MaxConcurrent int    `json:"max_concurrent"`
	ModelsDir     string `json:"models_dir"`
	// OutDir — legacy-каталог результата (REEXPORT_OUT_DIR), если он пригоден:
	// внутри тома моделей. Пусто — переменная не задана или не применяется
	// (тогда out_dir_ignored=true): писать за том моделей сервис не имеет права.
	OutDir string `json:"out_dir,omitempty"`
	// OutDirIgnored — переменная задана, но лежит вне тома моделей и потому
	// приглушена. Флаг нужен, чтобы панель/оператор видели это из ответа, а не
	// искали причину в логах контейнера.
	OutDirIgnored bool   `json:"out_dir_ignored,omitempty"`
	JobsDir       string `json:"jobs_dir"`
	ScanDepth     int    `json:"scan_depth"`
	MaxListed     int    `json:"max_listed_models"`
	Cancel        string `json:"cancel"`
}

// buildCapabilities собирает ответ. Версия конвертера берётся из ЗАПЕЧЁННОЙ
// идентичности образа (pkg/version → identity.json), а не из env и не из HEAD:
// вопрос «какой llama.cpp умеет этот сервис» относится к образу.
func buildCapabilities(info pythonInfo) capabilitiesResponse {
	id := buildIdentity()
	revision, _ := id["llama_commit"].(string)
	date, _ := id["llama_date"].(string)
	torchState := "unknown"
	switch {
	case !info.OK:
		torchState = "unknown"
	case info.TorchVersion != "":
		torchState = "present"
	default:
		torchState = "absent"
	}
	return capabilitiesResponse{
		Service:   "gguf-reexport",
		Available: true,
		Version:   id,
		Converter: converterInfo{
			Name:        "llama.cpp",
			Revision:    revision,
			Date:        date,
			Driver:      info.DriverPath,
			Python:      info.PythonVersion,
			Torch:       info.TorchVersion,
			PatchScript: info.PatchScript,
			TorchState:  torchState,
		},
		Architectures: []archSpec{
			{
				ID:      "inkling2",
				Scheme:  "inkling2_loop",
				Target:  "inkling2_loop",
				Label:   "Inkling-2 (loop-экспорт)",
				Note:    "block_count считает визиты, физических слоёв меньше; патч приводит файл к схеме inkling2_loop",
				Builtin: true,
			},
			{
				ID:      "inkling2_loop",
				Scheme:  "inkling2_loop",
				Label:   "Inkling-2 (уже в схеме загрузчика)",
				Note:    "ре-экспорт не требуется: файл уже в схеме inkling2_loop",
				Builtin: true,
			},
			{
				ID:     "*",
				Scheme: "plain",
				Label:  "прочие GGUF",
				Note:   "ре-экспорт метаданных не нужен: файл загружается как есть",
			},
		},
		Targets: targetRegistry.list(),
		Limits: limitsInfo{
			MaxConcurrent: 1,
			ModelsDir:     "", // заполняется обработчиком (нужен cfg)
			JobsDir:       "",
			ScanDepth:     0,
			MaxListed:     0,
			Cancel:        "DELETE /api/reexport/jobs/{id} — процесс конвертера убивается, частичный результат удаляется",
		},
		Endpoints: map[string]string{
			"capabilities": "GET /api/reexport/capabilities",
			"models":       "GET /api/reexport/models",
			"create":       "POST /api/reexport/jobs",
			"list":         "GET /api/reexport/jobs",
			"get":          "GET /api/reexport/jobs/{id}",
			"cancel":       "DELETE /api/reexport/jobs/{id}",
			"rescan":       "POST /api/models/refresh на cppworker — после успешной задачи воркер должен перечитать каталог",
		},
	}
}
