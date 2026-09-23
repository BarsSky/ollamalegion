package balancer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ---------- Helpers для /api/ps и /api/tags (R65d, 2026-09-20) ----------

// defaultPSExpiresWindow — окно, которое /api/ps показывает как время жизни
// загруженной модели, если точное значение неизвестно.
//
// Точный keep_alive живёт в cppworker (см. applyKeepAlive,
// handlers_generate.go:267), балансер его не знает. Раньше в это поле
// подставлялось нулевое time.Time, которое сериализовалось как
// "0001-01-01T00:00:00Z" — клиенты (OpenWebUI, `ollama ps`) считали модель
// протухшей. 30 минут совпадает с defaultKeepAliveDuration у cppworker.
const defaultPSExpiresWindow = 30 * time.Minute

// digestForModel строит стабильный content-addressed digest модели.
//
// Раньше /api/ps отдавал Digest="" (а cppworker в /api/tags —
// fmt.Sprintf("sha256:%x", sizeBytes), то есть байтовый размер под видом хеша,
// из-за чего две модели одинакового размера получали одинаковый «digest»).
//
// Полный SHA256 файла считать нельзя: модели по 5-20 GB, а endpoint
// read-only и вызывается часто. Для идентичности модели в списке достаточно
// хеша от (путь + имя + размер) — он стабилен между запросами и различает
// модели одинакового размера.
func digestForModel(m types.LlamaCppModel) string {
	if m.Name == "" && m.Path == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(m.Path))
	h.Write([]byte{0})
	h.Write([]byte(m.Name))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatUint(m.Size, 10)))
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

// estimateParameterSize оценивает размер модели в параметрах для поля
// details.parameter_size ("7.6B").
// Раньше в /api/ps это поле всегда было пустой строкой (details["parameter_size"]="").
// Для llama-архитектур число параметров ≈ n_layers × n_embd² × k, где k зависит
// от наличия gate/up/down и MoE. Точное значение требует чтения GGUF-метаданных,
// поэтому даём грубую оценку, помечая её как приблизительную — этого достаточно
// для отображения в UI (OpenWebUI показывает parameter_size в карточке модели).
//
// Если данных нет — возвращаем "unknown", а не пустую строку: пустое значение
// UI рендерит как «-», а "unknown" честно сообщает, что размер неизвестен.
func estimateParameterSize(m types.LlamaCppModel) string {
	if m.NLayers <= 0 || m.NEmbd <= 0 {
		return "unknown"
	}
	// Эмпирический коэффициент: для типичного трансформера с SwiGLU
	// params ≈ 12 × n_layers × n_embd² (attention + MLP с 3 матрицами).
	// Значение внутри порядка величины, поэтому округляем до 0.1B.
	const coeff = 12.0
	params := coeff * float64(m.NLayers) * float64(m.NEmbd) * float64(m.NEmbd)
	billions := params / 1e9
	if billions < 0.05 {
		return "unknown"
	}
	return strconv.FormatFloat(billions, 'f', 1, 64) + "B"
}

// detailsForModel строит Ollama-совместимый блок details для /api/tags.
//
// Раньше handleTags создавал записи вообще без Details (nil), поэтому клиенты,
// читающие details.family / quantization_level (OpenWebUI показывает их в
// карточке модели; ollama list использует parameter_size), видели пустоту.
// Набор полей соответствует Ollama API: parent_model, format, family, families,
// parameter_size, quantization_level.
func detailsForModel(m types.LlamaCppModel) map[string]interface{} {
	family := m.Architecture
	if family == "" {
		family = "unknown"
	}
	d := map[string]interface{}{
		"parent_model":       "",
		"format":             "gguf",
		"family":             family,
		"families":           []string{family},
		"parameter_size":     estimateParameterSize(m),
		"quantization_level": quantizationOrUnknown(m.Quantization),
	}
	return d
}

// quantizationOrUnknown — Ollama отдаёт "unknown" вместо пустой строки.
func quantizationOrUnknown(q string) string {
	if strings.TrimSpace(q) == "" {
		return "unknown"
	}
	return q
}

// digestForOnDiskModel — digest для модели, известной только по имени и размеру
// (ответ /api/models/files не содержит путь).
//
// Раньше в этих записях digest отсутствовал вовсе. Полный SHA256 файла считать
// нельзя (модели по 5-20 GB, endpoint вызывается часто), поэтому используем
// стабильный хеш от имени+размера: он различает модели и не меняется между
// запросами. Префикс "sha256:" сохранён для совместимости с форматом Ollama.
func digestForOnDiskModel(name string, size int64) string {
	if name == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(size, 10)))
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

// ---------- Read-only endpoints ----------

// handleTags — агрегирует список моделей со всех llama.cpp бэкендов.
//
// Возвращает ОБЪЕДИНЕНИЕ (дедуплицированное по имени):
//  1. Loaded models из метрик (быстро, in-memory)
//  2. Available models (cppworker's /api/models/files, все .gguf на диске)
//
// Round 19 hotfix (2026-08-03): раньше fallbacks (cppworker /api/tags) запускались
// ТОЛЬКО когда loaded=0. В результате если 1+ модель загружена — клиенту возвращался
// только loaded список, БЕЗ on-disk моделей. OpenWebUI не видел остальные .gguf
// файлы, которые можно подгрузить lazy-load'ом.
//
// Теперь fallbacks запускаются ВСЕГДА (best-effort, ранний выход если уже нашли
// что-то). Источники объединяются, дедуп по name.
func (lr *LlamaCppRouter) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
		return
	}

	// 1) Loaded models из метрик всех llama.cpp бэкендов (быстро, in-memory).
	//
	// Round 19 hotfix (2026-08-03): читаем из `llamaMetrics[id]` (куда пишет
	// llamaCppMetricsPoller), а не из `metrics[id].LlamaCpp` (куда пишет
	// Ollama-agent). Без этого fix'а — `LoadedModels` всегда пустой для бэкендов
	// без agent, и loaded модели пропадают из /api/tags.
	lr.proxy.metricsMgr.mu.RLock()
	uniqueModels := make(map[string]OllamaTag)
	for _, b := range backends {
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.id]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			// R65d (2026-09-20): заполняем Size/Digest/Details/ModifiedAt.
			//
			// Было: {Name, Model, Size: 0} без Digest/ModifiedAt/Details, и
			// благодаря «первый победил» ниже (шаг 2) более богатая on-disk
			// запись НЕ перезаписывала бедную. Клиенты, читающие
			// details.family / parameter_size / digest (OpenWebUI, ollama list),
			// получали пустоту для всех загруженных моделей.
			entry := OllamaTag{
				Name:  m.Name,
				Model: m.Name,
				Size:  int64(m.Size),
			}
			entry.Digest = digestForModel(m)
			entry.Details = detailsForModel(m)
			if m.LoadedAt != "" {
				if t, perr := time.Parse(time.RFC3339, m.LoadedAt); perr == nil {
					entry.ModifiedAt = t
				}
			}
			// Не перезаписываем уже собранную запись (дедуп по имени).
			if _, exists := uniqueModels[m.Name]; !exists {
				uniqueModels[m.Name] = entry
			}
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// 2) ВСЕГДА опрашиваем /api/models/files на каждом бэкенде чтобы получить
	//    полный список .gguf на диске (включая выгруженные). Round 19 hotfix:
	//    убрал условие `if len(uniqueModels) == 0` — раньше on-disk список
	//    возвращался ТОЛЬКО когда loaded=0, скрывая остальные модели от клиента.
	//    Дедупликация по имени сохраняется (map).
	for _, b := range backends {
		files, aliases, err := lr.fetchLlamaCppFilesWithAliases(b.host, b.port)
		if err != nil {
			logger.Get().Debugw("handleTags: /api/models/files fetch failed (non-fatal)",
				"backend", b.id, "host", b.host, "port", b.port, "error", err)
			continue
		}
		// R66d (2026-09-23): алиасы моделей (POST /api/create) — отдельные записи,
		// указывающие на существующий .gguf. Клиент должен видеть модель под именем
		// алиаса (details.parent_model = исходная модель), иначе «создали — не видно».
		for _, a := range aliases {
			if a.Name == "" {
				continue
			}
			if _, exists := uniqueModels[a.Name]; exists {
				continue // загруженная/файловая запись с тем же именем точнее
			}
			uniqueModels[a.Name] = OllamaTag{
				Name:       a.Name,
				Model:      a.Name,
				Size:       a.SourceSizeBytes,
				ModifiedAt: a.CreatedAt,
				Digest:     digestForOnDiskModel(a.Name, a.SourceSizeBytes),
				Details: map[string]interface{}{
					"format":         "gguf",
					"parent_model":   a.ParentModel,
					"parameter_size": "unknown",
					"families":       []string{},
				},
			}
		}
		for _, f := range files {
			// f.Name includes .gguf extension, strip for Ollama convention
			name := f.Name
			if strings.HasSuffix(strings.ToLower(name), ".gguf") {
				name = name[:len(name)-5]
			}
			// R65d (2026-09-20): «богатая запись побеждает».
			//
			// Раньше on-disk данные НЕ применялись к уже существующей записи
			// (loaded) из-за «первый победил». В результате загруженная модель
			// показывалась с Size=0 и без ModifiedAt, хотя cppworker знает и
			// размер файла, и mtime. Теперь дополняем запись недостающими
			// полями, сохраняя уже известные (digest/details от loaded).
			if existing, exists := uniqueModels[name]; exists {
				if existing.Size == 0 && f.SizeBytes > 0 {
					existing.Size = f.SizeBytes
				}
				if existing.ModifiedAt.IsZero() && !f.ModifiedAt.IsZero() {
					existing.ModifiedAt = f.ModifiedAt
				}
				if existing.Model == "" {
					existing.Model = name
				}
				uniqueModels[name] = existing
				continue
			}
			uniqueModels[name] = OllamaTag{
				Name:       name,
				Model:      name,
				Size:       f.SizeBytes,
				ModifiedAt: f.ModifiedAt,
				// On-disk запись тоже должна иметь digest: клиенты используют его
				// для идентификации модели. Считаем от имени+размера (файла под
				// рукой нет — путь не отдан в files-ответе).
				Digest: digestForOnDiskModel(name, f.SizeBytes),
			}
		}
	}

	// 3) Final fallback: если по-прежнему пусто (все бэкенды недоступны), пробуем
	//    cppworker's /api/tags — он делает то же что и files, но с другим форматом.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			tags, err := lr.fetchLlamaCppTags(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleTags: final fallback cppworker /api/tags failed",
					"backend", b.id, "host", b.host, "port", b.port, "error", err)
				continue
			}
			for _, t := range tags {
				if _, exists := uniqueModels[t.Name]; !exists {
					uniqueModels[t.Name] = t
				}
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	// 4) R66c (2026-09-22): последний резерв — OpenAI-совместимый /v1/models.
	//
	// fetchLlamaCppModels был написан, но НИКОГДА не вызывался: если бэкенд не
	// отдаёт ни /api/models/files, ни /api/tags, а умеет только /v1/models
	// (голый llama.cpp server, vLLM, любой OpenAI-совместимый upstream),
	// список моделей у клиента оказывался пустым — при том что /api/tags
	// обязан быть эквивалентом `ollama list`. Теперь это честный fallback.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			tags, err := lr.fetchLlamaCppModels(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleTags: /v1/models fallback failed",
					"backend", b.id, "host", b.host, "port", b.port, "error", err)
				continue
			}
			for _, t := range tags {
				if _, exists := uniqueModels[t.Name]; !exists {
					uniqueModels[t.Name] = t
				}
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	models := make([]OllamaTag, 0, len(uniqueModels))
	for _, m := range uniqueModels {
		models = append(models, m)
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})

	logger.Get().Infow("handleTags: returning models",
		"count", len(models),
	)

	writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: models})
}

// fetchLlamaCppModels — запрашивает /v1/models у llama.cpp бэкенда
// и парсит OpenAI-совместимый ответ в список OllamaTag.
func (lr *LlamaCppRouter) fetchLlamaCppModels(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Data))
	for _, d := range result.Data {
		tags = append(tags, OllamaTag{
			Name:  d.ID,
			Model: d.ID,
			Size:  0,
		})
	}
	return tags, nil
}

// fetchLlamaCppFiles — запрашивает /api/models/files у cppworker.
// Возвращает список ВСЕХ .gguf файлов на диске (включая выгруженные).
// Round 19 hotfix: используется как primary source для on-disk моделей.
//
// Преимущества перед /api/tags:
//   - быстрее (только fs.ReadDir, без загрузки метаданных модели)
//   - всегда возвращает актуальный список (не зависит от того, загружена модель или нет)
//   - не зависает если какая-то модель в состоянии loading
type llamaCppFileEntry struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}

// llamaCppAliasEntry — алиас модели (<name>.gguf.json, POST /api/create).
//
// R66d (2026-09-23): cppworker отдаёт их в том же ответе /api/models/files
// (поле aliases), и /api/tags балансера собирается именно из этого ответа —
// без слияния алиасов созданная модель не доезжала до клиентов (Cline/OpenWebUI
// её просто не видели в списке моделей).
type llamaCppAliasEntry struct {
	// Порядок полей — по требованию govet fieldalignment: сначала time.Time,
	// затем строки, затем скаляры.
	CreatedAt       time.Time
	Name            string
	Source          string
	ParentModel     string
	SourceSizeBytes int64
}

// fetchLlamaCppFilesWithAliases — файлы + алиасы с бэкенда одним запросом.
func (lr *LlamaCppRouter) fetchLlamaCppFilesWithAliases(host string, port int) ([]llamaCppFileEntry, []llamaCppAliasEntry, error) {
	url := fmt.Sprintf("http://%s:%d/api/models/files", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Files []struct {
			ModifiedAt string `json:"modifiedAt"`
			Name       string `json:"name"`
			SizeBytes  int64  `json:"sizeBytes"`
		} `json:"files"`
		Aliases []struct {
			CreatedAt       string `json:"createdAt"`
			Name            string `json:"name"`
			Source          string `json:"source"`
			ParentModel     string `json:"parentModel"`
			SourceSizeBytes int64  `json:"sourceSizeBytes"`
		} `json:"aliases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, err
	}

	entries := make([]llamaCppFileEntry, 0, len(result.Files))
	for _, f := range result.Files {
		var modTime time.Time
		if f.ModifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, f.ModifiedAt); err == nil {
				modTime = t
			}
		}
		entries = append(entries, llamaCppFileEntry{
			Name:       f.Name,
			SizeBytes:  f.SizeBytes,
			ModifiedAt: modTime,
		})
	}

	aliases := make([]llamaCppAliasEntry, 0, len(result.Aliases))
	for _, a := range result.Aliases {
		var createdAt time.Time
		if a.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
				createdAt = t
			}
		}
		parent := a.ParentModel
		if parent == "" {
			parent = strings.TrimSuffix(a.Source, ".gguf")
		}
		aliases = append(aliases, llamaCppAliasEntry{
			Name:            a.Name,
			Source:          a.Source,
			ParentModel:     parent,
			SourceSizeBytes: a.SourceSizeBytes,
			CreatedAt:       createdAt,
		})
	}
	return entries, aliases, nil
}

func (lr *LlamaCppRouter) fetchLlamaCppFiles(host string, port int) ([]llamaCppFileEntry, error) {
	entries, _, err := lr.fetchLlamaCppFilesWithAliases(host, port)
	return entries, err
}

// fetchLlamaCppTags — запрашивает Ollama-совместимый /api/tags у cppworker.
// В отличие от /v1/models, отдаёт ВСЕ .gguf файлы на диске (включая выгруженные),
// с полным Ollama-форматом: name, model, size, digest, modified_at, details.
func (lr *LlamaCppRouter) fetchLlamaCppTags(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model,omitempty"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest,omitempty"`
			ModifiedAt string `json:"modified_at,omitempty"`
			Details    struct {
				Format          string `json:"format,omitempty"`
				Family          string `json:"family,omitempty"`
				ParameterSize   string `json:"parameter_size,omitempty"`
				QuantizationLvl string `json:"quantization_level,omitempty"`
			} `json:"details,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Models))
	for _, m := range result.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		// Убираем расширение .gguf для консистентности с Ollama-форматом.
		displayName := strings.TrimSuffix(name, ".gguf")

		details := map[string]interface{}{}
		if m.Details.Format != "" {
			details["format"] = m.Details.Format
		}
		if m.Details.Family != "" {
			details["family"] = m.Details.Family
		}
		if m.Details.ParameterSize != "" {
			details["parameter_size"] = m.Details.ParameterSize
		}
		if m.Details.QuantizationLvl != "" {
			details["quantization_level"] = m.Details.QuantizationLvl
		}

		var modTime time.Time
		if m.ModifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, m.ModifiedAt); err == nil {
				modTime = t
			} else {
				logger.Get().Debugw("fetchLlamaCppTags: failed to parse modified_at",
					"value", m.ModifiedAt, "error", err)
			}
		}

		tags = append(tags, OllamaTag{
			Name:       displayName,
			Model:      displayName,
			Size:       m.Size,
			Digest:     m.Digest,
			ModifiedAt: modTime,
			Details:    details,
		})
	}
	return tags, nil
}

// handleModels — возвращает список моделей в нативном формате cppworker'а.
// Агрегируем по всем llama.cpp бэкендам (с дедупликацией по path).
func (lr *LlamaCppRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count":  0,
			"models": []interface{}{},
		})
		return
	}

	type cppModel struct {
		Name          string `json:"name"`
		Path          string `json:"path,omitempty"`
		State         string `json:"state,omitempty"`
		SizeBytes     int64  `json:"sizeBytes,omitempty"`
		NLayers       int    `json:"nLayers,omitempty"`
		NHeads        int    `json:"nHeads,omitempty"`
		NEmbd         int    `json:"nEmbd,omitempty"`
		NVocab        int    `json:"nVocab,omitempty"`
		ContextSize   int    `json:"contextSize,omitempty"`
		GPULayers     int    `json:"gpuLayers,omitempty"`
		ActiveQueries int    `json:"activeQueries,omitempty"`
		TotalQueries  int    `json:"totalQueries,omitempty"`
		LoadedAt      string `json:"loadedAt,omitempty"`
		Backend       string `json:"backend,omitempty"`
	}
	type cppResponse struct {
		Count  int        `json:"count"`
		Models []cppModel `json:"models"`
	}

	merged := cppResponse{Models: []cppModel{}}
	seen := make(map[string]bool)

	for _, b := range backends {
		resp, err := lr.fetchLlamaCppModelsNative(b.host, b.port)
		if err != nil {
			logger.Get().Warnw("handleModels: fetch failed",
				"backend", b.id, "host", b.host, "port", b.port, "error", err)
			continue
		}
		for _, m := range resp.Models {
			key := m.Path
			if key == "" {
				key = m.Name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged.Models = append(merged.Models, cppModel{
				Name:          m.Name,
				Path:          m.Path,
				State:         m.State,
				SizeBytes:     m.SizeBytes,
				NLayers:       m.NLayers,
				NHeads:        m.NHeads,
				NEmbd:         m.NEmbd,
				NVocab:        m.NVocab,
				ContextSize:   m.ContextSize,
				GPULayers:     m.GPULayers,
				ActiveQueries: m.ActiveQueries,
				TotalQueries:  m.TotalQueries,
				LoadedAt:      m.LoadedAt,
				Backend:       b.id,
			})
		}
	}
	merged.Count = len(merged.Models)

	logger.Get().Infow("handleModels: returning models",
		"count", merged.Count,
		"backends_queried", len(backends),
	)

	writeJSON(w, http.StatusOK, merged)
}

// fetchLlamaCppModelsNative — запрашивает нативный /api/models у cppworker.
func (lr *LlamaCppRouter) fetchLlamaCppModelsNative(host string, port int) (*cppWorkerModelsNative, error) {
	url := fmt.Sprintf("http://%s:%d/api/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result cppWorkerModelsNative
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// handlePS — возвращает список запущенных моделей (Ollama /api/ps).
//
// R66c (2026-09-22): агрегируем ВСЕ типы бэкендов, а не только llama.cpp.
// Раньше метод смотрел лишь в getLlamaCppBackends(), поэтому в кластере с
// Ollama-бэкендами (и в смешанном) уже загруженные на них модели в ответ не
// попадали: `ollama ps`, OpenWebUI и WebUI-страница моделей показывали пустой
// список, а клиент, проверяя «загружена ли модель», считал её незагруженной и
// уходил в auto-load. Для llama.cpp источник — llamaMetrics (cppworker-poller),
// для Ollama — metrics[id].Ollama.RunningModels (данные agent'а).
func (lr *LlamaCppRouter) handlePS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allBackends := lr.proxy.GetAllBackends()
	if len(allBackends) == 0 {
		writeJSON(w, http.StatusOK, OllamaPSResponse{Models: []OllamaProcess{}})
		return
	}

	lr.proxy.metricsMgr.mu.RLock()
	// R66b (2026-09-22): инициализируем НЕ nil-слайсом. С nil json.Marshal даёт
	// `"models":null`, тогда как Ollama всегда отдаёт `"models":[]`. Клиенты,
	// которые сразу итерируются по списку (ollama-python: `for m in resp["models"]`,
	// OpenWebUI), на null падают с TypeError — «пустой список процессов» ломал
	// страницу моделей вместо того, чтобы просто ничего не показать.
	allProcesses := make([]OllamaProcess, 0, 4)
	for _, b := range allBackends {
		if b.Status != types.StatusHealthy && string(b.Status) != "degraded" {
			continue
		}

		// Ollama-бэкенды: загруженные модели приходят от agent'а.
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			bm, ok := lr.proxy.metricsMgr.metrics[b.ID]
			if !ok || bm == nil {
				continue
			}
			for _, m := range bm.Ollama.RunningModels {
				if m.Name == "" {
					continue
				}
				details := map[string]interface{}{}
				if m.Family != "" {
					details["family"] = m.Family
					details["families"] = []string{m.Family}
				}
				if m.Quantization != "" {
					details["quantization_level"] = m.Quantization
				}
				if m.ParameterSize != "" {
					details["parameter_size"] = m.ParameterSize
				} else {
					details["parameter_size"] = "unknown"
				}
				format := m.Format
				if format == "" {
					format = "gguf"
				}
				details["format"] = format

				// expires_at: НЕ отдаём нулевое время (см. комментарий ниже).
				expiresAt := m.ExpiresAt
				if expiresAt.IsZero() {
					expiresAt = time.Now().Add(defaultPSExpiresWindow)
				}

				allProcesses = append(allProcesses, OllamaProcess{
					Name:      m.Name,
					Model:     m.Name,
					Size:      int64(m.Size),
					Digest:    m.Digest,
					Details:   details,
					ExpiresAt: expiresAt,
					SizeVRAM:  int64(m.VRAMUsage) * 1024 * 1024,
				})
			}
			continue
		}

		// llama.cpp-бэкенды: источник — llamaMetrics (cppworker-poller).
		//
		// Round 19 hotfix: читаем из llamaMetrics (cppworker-poller), не metrics[id].LlamaCpp (Ollama-agent).
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.ID]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			// R60.58 fix (2026-09-14): было hardcoded Size=0/SizeVRAM=0 — OpenWebUI
			// видел пустой /api/ps, считал модель не загруженной и зависал на auto-load.
			// LlamaCppModel уже имеет Size (bytes) и VRAMUsage (MB).
			// R65d (2026-09-20): заполняем поля, которые раньше были заглушками.
			//
			// Было: Digest="" и ExpiresAt=time.Time{} (нулевое время → JSON
			// "0001-01-01T00:00:00Z"). Клиенты, читающие expires_at (OpenWebUI,
			// `ollama ps`), видели модель «протухшей» с 1-го января года 1, а
			// digest отсутствовал.
			details := map[string]interface{}{}
			if m.Architecture != "" {
				details["family"] = m.Architecture
				details["families"] = []string{m.Architecture}
			}
			if m.Quantization != "" {
				details["quantization_level"] = m.Quantization
			}
			details["parameter_size"] = estimateParameterSize(m)
			details["format"] = "gguf"

			// expires_at: Ollama показывает время, когда модель будет выгружена.
			// Точного значения балансер не знает (keep_alive обрабатывает
			// cppworker), поэтому отдаём консервативную оценку «сейчас + дефолтное
			// окно жизни», а при наличии LoadedAt — от него. Главное — НЕ отдавать
			// нулевое время.
			expiresAt := time.Now().Add(defaultPSExpiresWindow)
			if m.LoadedAt != "" {
				if loadedAt, perr := time.Parse(time.RFC3339, m.LoadedAt); perr == nil {
					expiresAt = loadedAt.Add(defaultPSExpiresWindow)
				}
			}

			allProcesses = append(allProcesses, OllamaProcess{
				Name:      m.Name,
				Model:     m.Name,
				Size:      int64(m.Size),
				Digest:    digestForModel(m),
				Details:   details,
				ExpiresAt: expiresAt,
				SizeVRAM:  int64(m.VRAMUsage) * 1024 * 1024,
			})
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	writeJSON(w, http.StatusOK, OllamaPSResponse{Models: allProcesses})
}

// handleVersion — возвращает версию балансировщика.
func (lr *LlamaCppRouter) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"version":       "ollamalegion-1.0.0",
		"llamaVersions": make(map[string]string),
	}

	writeJSON(w, http.StatusOK, response)
}

// handleOpenAIModels — возвращает список моделей в OpenAI-формате
// {"object":"list","data":[{"id":"...","object":"model","created":...,"owned_by":"ollamalegion"}]}
// Используется OpenWebUI при обращении к /openai/v1/models.
//
// Round 19 hotfix (2026-08-03): баг был в том, что fallback на /v1/models и
// /api/models/files срабатывал ТОЛЬКО когда loaded=0. Если хоть 1 модель загружена —
// клиент видел только её. Теперь объединяем loaded + on-disk ВСЕГДА, как в handleTags.
func (lr *LlamaCppRouter) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	uniqueModels := make(map[string]bool)

	// 1) Loaded models из метрик всех llama.cpp бэкендов.
	//    Round 19 hotfix: читаем из llamaMetrics (cppworker-poller), не metrics[id].LlamaCpp (Ollama-agent).
	lr.proxy.metricsMgr.mu.RLock()
	for _, b := range backends {
		lm, ok := lr.proxy.metricsMgr.llamaMetrics[b.id]
		if !ok || lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			uniqueModels[m.Name] = true
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// 2) ВСЕГДА опрашиваем /api/models/files чтобы получить .gguf на диске.
	//    Round 19 hotfix: убрал `if len(uniqueModels) == 0` guard.
	for _, b := range backends {
		files, err := lr.fetchLlamaCppFiles(b.host, b.port)
		if err != nil {
			logger.Get().Debugw("handleOpenAIModels: /api/models/files fetch failed (non-fatal)",
				"backend", b.id, "error", err)
			continue
		}
		for _, f := range files {
			name := f.Name
			if strings.HasSuffix(strings.ToLower(name), ".gguf") {
				name = name[:len(name)-5]
			}
			uniqueModels[name] = true
		}
	}

	// 3) Final fallback: cppworker /v1/models (только loaded), затем /api/tags.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			models, err := lr.fetchLlamaCppModels(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleOpenAIModels: final fallback /v1/models failed",
					"backend", b.id, "error", err)
				continue
			}
			for _, m := range models {
				uniqueModels[m.Name] = true
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(uniqueModels))
	for name := range uniqueModels {
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  now,
			"owned_by": "ollamalegion",
		})
	}
	sort.Slice(data, func(i, j int) bool {
		idI, _ := data[i]["id"].(string)
		idJ, _ := data[j]["id"].(string)
		return idI < idJ
	})

	logger.Get().Infow("handleOpenAIModels: returning models",
		"count", len(data),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}
