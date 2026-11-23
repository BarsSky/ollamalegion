package cppbackend

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScanModels сканирует директорию на предмет .gguf файлов
func (m *ModelManager) ScanModels() ([]GGUFModelMeta, error) {
	start := time.Now()
	log := logger.Get()

	m.mu.Lock()
	m.totalScans++
	m.mu.Unlock()

	// Проверяем существование директории.
	//
	// R93: недоступный каталог (удалённый bind-mount, отвалившийся сетевой том)
	// больше НЕ оставляет старый кэш — инвентарь обнуляется, причина пишется в
	// dirError и уезжает в API. Иначе WebUI показывал модели, которых нет.
	info, err := os.Stat(m.modelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infow("models directory does not exist, creating",
				"dir", m.modelsDir)
			if mkErr := os.MkdirAll(m.modelsDir, 0755); mkErr != nil {
				// Живой случай: каталог был bind-mount'ом, том удалили —
				// mkdir отвечает «File exists», а ls/ReadDir дают ENOENT.
				return m.recordDirFailure(fmt.Errorf("create models dir: %w", mkErr))
			}
		} else {
			return m.recordDirFailure(fmt.Errorf("stat models dir: %w", err))
		}
	} else if !info.IsDir() {
		return m.recordDirFailure(fmt.Errorf("models path is not a directory: %s", m.modelsDir))
	}

	// Сканируем .gguf файлы
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return m.recordDirFailure(fmt.Errorf("read models dir: %w", err))
	}

	newFiles := make(map[string]*GGUFModelMeta)
	var found int

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}

		fi, err := entry.Info()
		if err != nil {
			continue
		}

		meta := &GGUFModelMeta{
			Filename:   name,
			Path:       filepath.Join(m.modelsDir, name),
			SizeBytes:  fi.Size(),
			ModifiedAt: fi.ModTime(),
		}

		// Пытаемся извлечь архитектуру из GGUF header (лениво)
		// Полные метаданные будут извлечены при загрузке модели

		newFiles[name] = meta
		found++
	}

	// R66d (2026-09-23): сканируем алиасы моделей (<name>.gguf.json, создаются
	// через Ollama-совместимый POST /api/create). Раньше они не попадали ни в
	// ListModels, ни в /api/tags, ни в resolveModelPath — то есть «создали
	// модель, а её нигде нет и загрузить нельзя».
	newAliases := make(map[string]GGUFAlias)
	aliasFound := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf.json") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(m.modelsDir, name))
		if readErr != nil {
			continue
		}
		var rec struct {
			Name      string `json:"name"`
			Source    string `json:"source"`
			Modelfile string `json:"modelfile"`
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		aliasName := rec.Name
		if aliasName == "" {
			aliasName = strings.TrimSuffix(name, ".gguf.json")
		}
		src := rec.Source
		if src == "" {
			continue
		}
		srcPath := src
		if !filepath.IsAbs(srcPath) {
			if !strings.HasSuffix(strings.ToLower(srcPath), ".gguf") {
				srcPath += ".gguf"
			}
			srcPath = filepath.Join(m.modelsDir, filepath.Base(srcPath))
		}
		a := GGUFAlias{
			Name:       aliasName,
			Source:     src,
			SourcePath: srcPath,
			Modelfile:  rec.Modelfile,
		}
		if fi, statErr := os.Stat(srcPath); statErr == nil {
			a.SourceSizeBytes = fi.Size()
			a.SourceExists = true
		}
		if ts, tsErr := time.Parse(time.RFC3339, rec.CreatedAt); tsErr == nil {
			a.CreatedAt = ts
		}
		a.AliasPath = filepath.Join(m.modelsDir, name)
		newAliases[aliasName] = a
		aliasFound++
	}

	// R66d: после сбора всех алиасов считаем эффективный источник с учётом
	// цепочек (alias → alias → .gguf) — SourceExists/SourceSizeBytes нужны
	// /api/tags, чтобы не показывать клиенту незагружаемые модели.
	for name, a := range newAliases {
		resolved, exists := resolveAliasChain(newAliases, newFiles, name)
		if resolved != "" {
			a.SourcePath = resolved
		}
		a.SourceExists = exists && a.SourcePath != ""
		if a.SourceExists {
			if fi, statErr := os.Stat(a.SourcePath); statErr == nil {
				a.SourceSizeBytes = fi.Size()
			}
		}
		newAliases[name] = a
	}

	// Обновляем кэш + фиксируем ИЗМЕНЕНИЯ инвентаря (R93).
	//
	// Диф нужен, чтобы «модель исчезла/появилась» было видно в логе и в API, а не
	// только по факту «список стал другим»: оператор должен понимать, что именно
	// изменилось, без перезапуска контейнера.
	m.mu.Lock()
	added, removed, resized := diffInventory(m.ggufFiles, newFiles)
	m.ggufFiles = newFiles
	m.aliases = newAliases
	m.lastScanTime = time.Now()
	m.scanDuration = time.Since(start)
	m.dirError = ""
	m.dirErrorAt = time.Time{}
	if len(added) > 0 || len(removed) > 0 || len(resized) > 0 {
		m.lastChangeAt = time.Now()
		m.lastAdded, m.lastRemoved, m.lastResized = added, removed, resized
		m.changeCount++
	}
	changes := len(added) + len(removed) + len(resized)
	m.mu.Unlock()

	if changes > 0 {
		log.Infow("model inventory changed",
			"added", added, "removed", removed, "resized", resized,
			"dir", m.modelsDir, "found", found)
	}
	log.Infow("model scan complete",
		"found", found,
		"aliases", aliasFound,
		"changes", changes,
		"dir", m.modelsDir,
		"duration", m.scanDuration)

	return m.ListModels(), nil
}

// diffInventory — что изменилось между двумя снимками каталога: добавленные,
// удалённые и изменившие размер файлы (отсортированы, чтобы лог был стабильным).
func diffInventory(oldFiles, newFiles map[string]*GGUFModelMeta) (added, removed, resized []string) {
	for name, meta := range newFiles {
		prev, ok := oldFiles[name]
		if !ok {
			added = append(added, name)
			continue
		}
		if prev != nil && meta != nil && prev.SizeBytes != meta.SizeBytes {
			resized = append(resized, name)
		}
	}
	for name := range oldFiles {
		if _, ok := newFiles[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(resized)
	return added, removed, resized
}

// recordDirFailure — каталог моделей недоступен: инвентарь ОБНУЛЯЕТСЯ.
//
// R93 (2026-10-09): раньше ошибка чтения каталога возвращалась наружу, а кэш
// оставался прежним — WebUI продолжал показывать модели, которых на диске уже нет
// (живой случай: bind-mount D:\ollama-legion-models удалили, /app/models даёт
// «File exists» на mkdir и «No such file or directory» на ls). Теперь список
// пуст, а причина лежит в dirError и уезжает в /api/models/files.
func (m *ModelManager) recordDirFailure(cause error) ([]GGUFModelMeta, error) {
	m.mu.Lock()
	prev := make([]string, 0, len(m.ggufFiles))
	for name := range m.ggufFiles {
		prev = append(prev, name)
	}
	sort.Strings(prev)
	m.ggufFiles = make(map[string]*GGUFModelMeta)
	m.aliases = make(map[string]GGUFAlias)
	m.dirError = cause.Error()
	m.dirErrorAt = time.Now()
	m.lastScanTime = time.Now()
	if len(prev) > 0 {
		m.lastChangeAt = time.Now()
		m.lastAdded, m.lastRemoved, m.lastResized = nil, prev, nil
		m.changeCount++
	}
	m.mu.Unlock()

	logger.Get().Warnw("model inventory: models dir unavailable, inventory cleared",
		"dir", m.modelsDir, "error", cause, "removed", prev)
	// Возвращаем пустой список БЕЗ ошибки: вызывающий код (HTTP-хендлеры) должен
	// отдать «моделей нет + причина», а не упасть. Операции импорта/удаления
	// сообщают о своих ошибках сами.
	return []GGUFModelMeta{}, nil
}

// ScanModelsIfStale — пересканировать каталог, если с прошлого скана прошло больше
// rescanTTL (R93). Возвращает true, если скан выполнялся.
//
// Вызывается из /api/models/files и из фонового цикла воркера: так удаление или
// добавление файла на диске становится видно без перезапуска контейнера.
func (m *ModelManager) ScanModelsIfStale() bool {
	m.mu.RLock()
	ttl := m.rescanTTL
	last := m.lastScanTime
	m.mu.RUnlock()
	if ttl <= 0 {
		return false
	}
	if !last.IsZero() && time.Since(last) < ttl {
		return false
	}
	_, _ = m.ScanModels()
	return true
}

// SetRescanTTL — период фоновой проверки каталога (0 = выключить). Для тестов и
// для оператора через env CPPWORKER_MODELS_RESCAN_SEC.
func (m *ModelManager) SetRescanTTL(d time.Duration) {
	m.mu.Lock()
	m.rescanTTL = d
	m.mu.Unlock()
}

// RescanTTL — текущий период фоновой проверки (0 = выключена).
func (m *ModelManager) RescanTTL() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rescanTTL
}

// InventoryStatus — снимок состояния инвентаря для API/WebUI.
func (m *ModelManager) InventoryStatus() ModelInventoryStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := ModelInventoryStatus{
		Dir:          m.modelsDir,
		Count:        len(m.ggufFiles),
		ScannedAt:    m.lastScanTime,
		ChangedAt:    m.lastChangeAt,
		Added:        append([]string(nil), m.lastAdded...),
		Removed:      append([]string(nil), m.lastRemoved...),
		Resized:      append([]string(nil), m.lastResized...),
		DirError:     m.dirError,
		DirErrorAt:   m.dirErrorAt,
		Scans:        m.totalScans,
		Changes:      m.changeCount,
		RescanTTLSec: int(m.rescanTTL / time.Second),
	}
	return st
}

// ListAliases возвращает все алиасы моделей (отсортированы по имени).
//
// R66d (2026-09-23): алиасы создаются Ollama-совместимым POST /api/create и
// лежат как <name>.gguf.json. Используется /api/tags, /api/models/files и
// resolveModelPath, чтобы созданная модель была видна и загружаема.
func (m *ModelManager) ListAliases() []GGUFAlias {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]GGUFAlias, 0, len(m.aliases))
	for _, a := range m.aliases {
		result = append(result, a)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// AliasSourcePath резолвит имя модели как алиас и возвращает путь к .gguf.
//
// Поддерживает цепочки алиас → алиас (например "short" → "gemma-alias" →
// "gemma-4-E4B-it-Q4_K_M.gguf") с ограничением глубины, чтобы цикл не повесил
// resolveModelPath. Возвращает false, если это не алиас или файл-источник
// отсутствует (тогда клиент получит честную ошибку, а не «модель не найдена»).
func (m *ModelManager) AliasSourcePath(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	srcPath, ok := resolveAliasChain(m.aliases, m.ggufFiles, name)
	if ok {
		return srcPath, true
	}
	// R83: если имя НЕ является алиасом, пробуем его варианты (Ollama-тег):
	// клиент просит "qwen3.8:latest", алиас создан как "qwen3.8".
	//
	// ВАЖНО: когда алиас ЕСТЬ, но файл-источник пропал, путь сохраняем как раньше
	// (R66d): вызывающий покажет понятную ошибку загрузки, а не «модель не
	// найдена». Регресс-тест: TestScanModels_Aliases_R66d («битый алиас»).
	if _, isAlias := aliasKey(m.aliases, name); !isAlias {
		for _, v := range ModelNameVariants(name) {
			if p, ok := resolveAliasChain(m.aliases, m.ggufFiles, v); ok {
				return p, true
			}
		}
	}
	return srcPath, false
}

// aliasKey — ключ алиаса для имени (та же нормализация, что в resolveAliasChain:
// trim + снятие .gguf).
func aliasKey(aliases map[string]GGUFAlias, name string) (string, bool) {
	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}
	_, ok := aliases[current]
	return current, ok
}

// resolveAliasChain — общая логика резолва алиасов (используется и в ScanModels,
// чтобы заранее посчитать SourcePath/SourceExists для цепочек).
//
// Возвращает (путь-источника, существует-ли-файл). Путь возвращается даже если
// файла нет — вызывающий покажет его в логах/ошибке.
func resolveAliasChain(
	aliases map[string]GGUFAlias,
	files map[string]*GGUFModelMeta,
	name string,
) (string, bool) {
	const maxDepth = 4
	seen := make(map[string]bool, maxDepth)

	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}

	var lastPath string
	for depth := 0; depth < maxDepth; depth++ {
		if current == "" || seen[current] {
			return lastPath, false
		}
		seen[current] = true

		a, ok := aliases[current]
		if !ok {
			return lastPath, false
		}
		srcBase := filepath.Base(a.Source)
		lastPath = a.SourcePath

		// 1. Источник — обычный .gguf, который видит скан.
		if meta, has := files[srcBase]; has {
			return meta.Path, true
		}
		// 2. Файл есть на диске (скан мог его ещё не увидеть).
		if _, err := os.Stat(a.SourcePath); err == nil {
			return a.SourcePath, true
		}
		// 3. Источник сам является алиасом — идём по цепочке.
		next := strings.TrimSuffix(srcBase, ".gguf")
		if _, isAlias := aliases[next]; isAlias {
			current = next
			continue
		}
		// 4. Ни файла, ни алиаса — битый алиас.
		return a.SourcePath, false
	}
	return lastPath, false
}

// AliasByName возвращает запись алиаса по имени модели (без .gguf).
func (m *ModelManager) AliasByName(name string) (GGUFAlias, bool) {
	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.aliases[current]
	return a, ok
}

// ListModels возвращает список всех .gguf файлов в директории
func (m *ModelManager) ListModels() []GGUFModelMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]GGUFModelMeta, 0, len(m.ggufFiles))
	for _, meta := range m.ggufFiles {
		result = append(result, *meta)
	}

	// Сортируем по имени
	sort.Slice(result, func(i, j int) bool {
		return result[i].Filename < result[j].Filename
	})

	return result
}

// GetModelMeta возвращает метаданные конкретного GGUF файла.
//
// Если архитектурные параметры (NLayers/NEmbd/NHeads/NKvHeads) ещё не
// прочитаны из GGUF header — читает их лениво через ReadGGUFHeader
// и обновляет кэш. Это даёт ensureModelLoaded возможность рассчитать
// max viable n_ctx ДО llama.cpp.LoadModel.
func (m *ModelManager) GetModelMeta(filename string) (*GGUFModelMeta, error) {
	m.mu.RLock()
	meta, exists := m.ggufFiles[filename]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found in models directory", filename)
	}

	// Lazy-load архитектурных параметров из GGUF header.
	// R83: условие включает KeyLength — параметры реального KV-кэша
	// (key_length/nextn/recurrent) обязаны добираться и для моделей, у которых
	// уже заполнены NLayers/NEmbd/NHeads/ContextLength.
	if meta.NLayers == 0 || meta.NEmbd == 0 || meta.NHeads == 0 || meta.ContextLength == 0 || meta.KeyLength == 0 {
		if hdr, err := ReadGGUFHeader(meta.Path); err == nil && hdr != nil {
			m.mu.Lock()
			if meta.NLayers == 0 {
				meta.NLayers = hdr.NLayers
			}
			if meta.NEmbd == 0 {
				meta.NEmbd = hdr.NEmbd
			}
			if meta.NHeads == 0 {
				meta.NHeads = hdr.NHeads
			}
			if meta.NKvHeads == 0 && hdr.NKvHeads > 0 {
				meta.NKvHeads = hdr.NKvHeads
			}
			// Round 37 (2026-08-18): ContextLength lazy-load.
			// ReadGGUFHeader парсит *.context_length (см. backend.go:ggufSetField case 4).
			// Qwen3.6-35B-A3B-UD-Q4_K_M → 262144 (262K токенов).
			if meta.ContextLength == 0 && hdr.ContextLength > 0 {
				meta.ContextLength = hdr.ContextLength
			}
			if meta.Architecture == "" {
				meta.Architecture = hdr.Architecture
			}
			// R83 (2026-09-25): параметры реального KV-кэша — иначе он считался
			// по block_count и n_embd/n_heads (завышение в разы для гибридных
			// моделей). См. kv_layers.go.
			if meta.KeyLength == 0 && hdr.KeyLength > 0 {
				meta.KeyLength = hdr.KeyLength
			}
			if meta.ValueLength == 0 && hdr.ValueLength > 0 {
				meta.ValueLength = hdr.ValueLength
			}
			if meta.NextNPredictLayers == 0 && hdr.NextNPredictLayers > 0 {
				meta.NextNPredictLayers = hdr.NextNPredictLayers
			}
			if meta.FullAttentionInterval == 0 && hdr.FullAttentionInterval > 0 {
				meta.FullAttentionInterval = hdr.FullAttentionInterval
			}
			if hdr.HasRecurrentLayersKey {
				meta.HasRecurrentLayersKey = true
			}
			// R83 (2026-10-01): KV-sharing + скользящее окно (gemma4/gemma3n).
			if meta.SharedKVLayers == 0 && hdr.SharedKVLayers > 0 {
				meta.SharedKVLayers = hdr.SharedKVLayers
			}
			if meta.SlidingWindow == 0 && hdr.SlidingWindow > 0 {
				meta.SlidingWindow = hdr.SlidingWindow
			}
			if meta.SWAKeyLength == 0 && hdr.SWAKeyLength > 0 {
				meta.SWAKeyLength = hdr.SWAKeyLength
			}
			if meta.SWAPattern == "" && hdr.SWAPattern != "" {
				meta.SWAPattern = hdr.SWAPattern
			}
			// R707 (2026-11-25): послойная раскладка KV — вторая ширина V у
			// оконного класса, число KV-голов по слоям, визиты loop-схемы.
			// Без них gemma-4 считался шириной глобальных слоёв, а Inkling-2 —
			// без разделения на окно. См. KVPlan в kv_layers.go.
			if meta.SWAValueLength == 0 && hdr.SWAValueLength > 0 {
				meta.SWAValueLength = hdr.SWAValueLength
			}
			if len(meta.KVHeadsPerLayer) == 0 && len(hdr.KVHeadsPerLayer) > 0 {
				meta.KVHeadsPerLayer = hdr.KVHeadsPerLayer
			}
			if meta.LoopCount == 0 && hdr.LoopCount > 0 {
				meta.LoopCount = hdr.LoopCount
			}
			if meta.LoopCoreStart == 0 && hdr.LoopCoreStart > 0 {
				meta.LoopCoreStart = hdr.LoopCoreStart
			}
			if meta.LoopCoreEnd == 0 && hdr.LoopCoreEnd > 0 {
				meta.LoopCoreEnd = hdr.LoopCoreEnd
			}
			// R712 (2026-11-25): канонические loop.start/end/count — признак
			// файла, уже переведённого в loop-схему. Ответ 409 (R704) обязан
			// его видеть, иначе советует повторный ре-экспорт. См.
			// GGUFHeaderInfo.HasLoopSchemaKeys.
			if hdr.HasLoopSchemaKeys {
				meta.HasLoopSchemaKeys = true
			}
			m.mu.Unlock()
		}
		// Если ReadGGUFHeader упал — caller использует fallback на
		// estimateLayersFromFileSize (см. Backend.checkVRAMForModel).
	}

	return meta, nil
}
