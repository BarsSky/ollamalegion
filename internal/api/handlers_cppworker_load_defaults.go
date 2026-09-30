// handlers_cppworker_load_defaults.go — R83 (2026-09-30).
//
// ЗАДАЧА ОПЕРАТОРА: «необходимо чётко в WebUI видеть, какие настройки есть и
// сейчас доступны для загрузки; кроме пресетов надо уметь назначать настройки
// моделям, что лежат в папке, и связывать их с дефолтным конфигом — тот конфиг,
// что выступает в роли инициализации при старте балансера, сделать редактируемым
// со стороны WebUI: если модель добавлена/выкачана новая, это позволит прописать
// в конфиг новые параметры под новую модель».
//
// Два эндпоинта:
//
//	GET /api/v1/cppworker/load-defaults  — что применится к модели БЕЗ своего
//	    профиля: значение каждого поля + ИСТОЧНИК (defaultProfile / cppworker-env)
//	    + сырые дефолты контейнера, чтобы оператор видел полную картину.
//	PUT /api/v1/cppworker/load-defaults  — записать эти значения в конфиг
//	    балансера (config.defaultModelProfile) с персистом.
//
//	GET /api/v1/cppworker/model-catalog   — модели, ЛЕЖАЩИЕ В ПАПКЕ: файл, размер,
//	    загружена ли, есть ли свой профиль и какие настройки будут применены
//	    (профиль → дефолт → env cppworker). Отсюда оператор назначает настройки
//	    файлу (создавая per-model профиль существующим PUT model-profiles).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// cppworkerLoadDefaultsPath — эндпоинты cppworker, которые читает балансер.
const (
	cppworkerConfigPath = "/api/v1/cppworker/config"
	cppworkerFilesPath  = "/api/models/files"
)

// cppworkerSnapshot — срез контейнерных дефолтов cppworker: то, что реально
// доступно для загрузки, если в балансере ничего не задано.
type cppworkerSnapshot struct {
	BackendID string                 `json:"backendId,omitempty"`
	Available bool                   `json:"available"`
	Error     string                 `json:"error,omitempty"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

// loadDefaultsResponse — ответ GET /api/v1/cppworker/load-defaults.
type loadDefaultsResponse struct {
	// DefaultProfile — то, что оператор задал в WebUI (config.defaultModelProfile).
	DefaultProfile *types.LlamaCppModelProfile `json:"defaultModelProfile,omitempty"`
	// Effective — что применится к новой модели без своего профиля.
	Effective map[string]interface{} `json:"effective"`
	// Source — откуда взято каждое значение: "defaultProfile" | "cppworker-env" | "none".
	Source map[string]string `json:"source"`
	// CppWorker — сырые дефолты контейнера (полная картина «что сейчас доступно»).
	CppWorker cppworkerSnapshot `json:"cppworker"`
	// ProfilesCount — сколько per-model профилей уже назначено.
	ProfilesCount int `json:"profilesCount"`
}

// handleLoadDefaults — GET/PUT /api/v1/cppworker/load-defaults.
func (s *Server) handleLoadDefaults(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getLoadDefaults(w, r)
	case http.MethodPut, http.MethodPost:
		s.putLoadDefaults(w, r)
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// getLoadDefaults собирает «доступные настройки загрузки»: значение + источник.
func (s *Server) getLoadDefaults(w http.ResponseWriter, _ *http.Request) {
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	cw := s.fetchCppWorkerSnapshot()
	prof, hasProfile := s.proxy.GetDefaultModelProfile()

	effective := map[string]interface{}{}
	source := map[string]string{}

	// pickInt/pickStr: значение из defaultProfile, иначе — из cppworker-конфига.
	pickInt := func(key string, fromProfile int, fromCpp string) {
		if hasProfile && fromProfile > 0 {
			effective[key] = fromProfile
			source[key] = "defaultProfile"
			return
		}
		if v, ok := cw.intValue(fromCpp); ok && v > 0 {
			effective[key] = v
			source[key] = "cppworker-env"
			return
		}
		source[key] = "none"
	}

	profileCtx, profileBatch, profileLayers, profileParallel := 0, 0, 0, 0
	profileKV := ""
	profileFlash := -1
	profileMmap := true
	if hasProfile {
		profileCtx = prof.ContextLength
		profileBatch = prof.BatchSize
		profileLayers = prof.NumGPULayers
		profileParallel = prof.Parallel
		profileKV = prof.KVCacheType
		if prof.FlashAttn != nil {
			if *prof.FlashAttn {
				profileFlash = 1
			} else {
				profileFlash = 0
			}
		}
		if prof.UseMmap != nil {
			profileMmap = *prof.UseMmap
		}
	}

	pickInt("contextLength", profileCtx, "defaultCtxSize")
	pickInt("batchSize", profileBatch, "defaultBatchSize")
	// gpuLayers: -2 = auto, -1 = все слои, 0 = CPU-only — поэтому «> 0» здесь не
	// критерий, значение берём при наличии профиля и любом отличии от нуля.
	if hasProfile && profileLayers != 0 {
		effective["numGpuLayers"] = profileLayers
		source["numGpuLayers"] = "defaultProfile"
	} else if v, ok := cw.intValue("defaultGpuLayers"); ok {
		effective["numGpuLayers"] = v
		source["numGpuLayers"] = "cppworker-env"
	} else {
		source["numGpuLayers"] = "none"
	}

	pickInt("parallel", profileParallel, "defaultNParallel")

	// R83 (2026-09-30): окно НА КЛИЕНТА. contextLength выше — СУММАРНОЕ окно,
	// которое слоты ДЕЛЯТ (llama.cpp: n_ctx_seq = n_ctx / n_seq_max). Показываем
	// обе цифры: иначе «16K + 2 слота» читается как 16K на клиента.
	if effCtx, ok := intFromEffective(effective, "contextLength"); ok && effCtx > 0 {
		slots, _ := intFromEffective(effective, "parallel")
		effective["contextPerSlot"] = cppbackend.ContextPerSlot(effCtx, slots)
		source["contextPerSlot"] = source["contextLength"]
	}
	if hasProfile && prof.ContextPerSeq > 0 {
		effective["contextPerSeq"] = prof.ContextPerSeq
		source["contextPerSeq"] = "defaultProfile"
	}

	if hasProfile && profileKV != "" {
		effective["kvCacheType"] = profileKV
		source["kvCacheType"] = "defaultProfile"
	} else if v, ok := cw.strValue("defaultKvCacheType"); ok && v != "" {
		effective["kvCacheType"] = v
		source["kvCacheType"] = "cppworker-env"
	} else {
		source["kvCacheType"] = "none"
	}

	if hasProfile && prof.FlashAttn != nil {
		effective["flashAttn"] = profileFlash
		source["flashAttn"] = "defaultProfile"
	} else if v, ok := cw.intValue("defaultFlashAttnType"); ok {
		effective["flashAttn"] = v
		source["flashAttn"] = "cppworker-env"
	} else {
		source["flashAttn"] = "none"
	}

	// useMmap и reasoning в cppworker-конфиге — bool. В профиле модели (и в
	// defaultModelProfile) поля EnableReasoning нет: reasoning задаётся env
	// контейнера или per-model запросом, поэтому источник здесь только env.
	if hasProfile && prof.UseMmap != nil {
		effective["useMmap"] = profileMmap
		source["useMmap"] = "defaultProfile"
	} else if v, ok := cw.boolValue("defaultUseMmap"); ok {
		effective["useMmap"] = v
		source["useMmap"] = "cppworker-env"
	} else {
		source["useMmap"] = "none"
	}

	if v, ok := cw.boolValue("defaultEnableReasoning"); ok {
		effective["enableReasoning"] = v
		source["enableReasoning"] = "cppworker-env"
	} else {
		source["enableReasoning"] = "none"
	}

	profilesCount := 0
	if n, err := s.proxy.CountModelProfiles(); err == nil {
		profilesCount = n
	}

	resp := loadDefaultsResponse{
		Effective:     effective,
		Source:        source,
		CppWorker:     cw,
		ProfilesCount: profilesCount,
	}
	if hasProfile {
		p := prof
		resp.DefaultProfile = &p
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// putLoadDefaults — записать «настройки по умолчанию» в конфиг балансера.
//
// Мерж с существующим defaultModelProfile (как в PUT model-profiles): форма
// WebUI может прислать только часть полей, и это не должно обнулять остальные.
func (s *Server) putLoadDefaults(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body", "message": err.Error()})
		return
	}

	var upd types.LlamaCppModelProfile
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&upd); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid JSON body",
			"message": err.Error(),
		})
		return
	}
	var presence profilePresence
	_ = json.Unmarshal(bodyBytes, &presence)

	existing, hasExisting := s.proxy.GetDefaultModelProfile()
	incoming := mergeProfileUpdate(existing, upd, presence)
	if !hasExisting {
		// Для НОВОГО дефолтного профиля contextLength обязателен — иначе
		// «настройки по умолчанию» ничего не задают.
		incoming = mergeProfileUpdate(types.LlamaCppModelProfile{}, upd, presence)
	}
	if err := validateModelProfile(incoming); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid defaults",
			"message": err.Error(),
		})
		return
	}

	s.proxy.SetDefaultModelProfile(incoming)

	persistWarning := ""
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			logger.Get().Warnw("putLoadDefaults: configSaver failed, trying profiles fallback", "error", err)
			if err := s.proxy.SaveProfilesToFile(); err != nil {
				logger.Get().Errorw("putLoadDefaults: profiles fallback also failed", "error", err)
				persistWarning = "defaults saved in memory, but config.json AND profiles.json write failed: " + err.Error()
			}
		}
	} else if err := s.proxy.SaveProfilesToFile(); err != nil {
		persistWarning = "defaults saved in memory, but profiles.json write failed: " + err.Error()
	}

	logger.Get().Infow("load defaults upserted",
		"contextLength", incoming.ContextLength, "parallel", incoming.Parallel,
		"kvCacheType", incoming.KVCacheType, "warning", persistWarning)

	resp := map[string]interface{}{
		"status":  "ok",
		"profile": incoming,
	}
	if persistWarning != "" {
		resp["warning"] = persistWarning
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleModelCatalog — GET /api/v1/cppworker/model-catalog.
//
// Модели, лежащие в папке (включая ещё не загруженные), с настройками, которые
// к ним применятся. Позволяет «назначить настройки файлу» до первой загрузки.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	cw := s.fetchCppWorkerSnapshot()
	backendID, baseURL := s.firstCppWorkerBackend()
	defaults, hasDefaults := s.proxy.GetDefaultModelProfile()

	type fileEntry struct {
		Name         string `json:"name"`
		SizeBytes    int64  `json:"sizeBytes"`
		Quantization string `json:"quantization,omitempty"`
		ModifiedAt   string `json:"modifiedAt,omitempty"`
		Loaded       bool   `json:"loaded"`
		LoadedCtx    int    `json:"loadedContextSize,omitempty"`
		HasProfile   bool   `json:"hasProfile"`
		// ProfileIgnored — профиль есть, но ignoreDefaults=true: его параметры
		// НЕ применяются (оператор выбрал «значения из env контейнера»).
		ProfileIgnored bool                        `json:"profileIgnored,omitempty"`
		Profile        *types.LlamaCppModelProfile `json:"profile,omitempty"`
		Effective      map[string]interface{}      `json:"effective"`
		Source         map[string]string           `json:"source"`
	}

	files := []fileEntry{}
	errMsg := ""
	if baseURL != "" {
		var payload struct {
			Files []struct {
				Name         string `json:"name"`
				Size         int64  `json:"size"`
				SizeBytes    int64  `json:"sizeBytes"`
				Quantization string `json:"quantization"`
				ModifiedAt   string `json:"modifiedAt"`
			} `json:"files"`
		}
		if err := s.cppworkerGetJSON(baseURL+cppworkerFilesPath, &payload); err != nil {
			errMsg = err.Error()
		} else {
			loaded := s.loadedModelsByBackend(backendID)
			for _, f := range payload.Files {
				entry := fileEntry{
					Name:         f.Name,
					SizeBytes:    f.SizeBytes,
					Quantization: f.Quantization,
					ModifiedAt:   f.ModifiedAt,
					Effective:    map[string]interface{}{},
					Source:       map[string]string{},
				}
				if entry.SizeBytes == 0 {
					entry.SizeBytes = f.Size
				}
				if lm, ok := loaded[strings.ToLower(f.Name)]; ok {
					entry.Loaded = true
					entry.LoadedCtx = lm
				}
				// Настройки: профиль модели → дефолтный профиль → env cppworker.
				// Ровно та же цепочка и то же правило ignoreDefaults, что в
				// executeLlamaCppLoad (applyProfileLoadParams) — иначе WebUI
				// показывал бы одно, а грузилось другое.
				fillFrom := func(p types.LlamaCppModelProfile, src string) {
					if entry.Effective["contextLength"] == nil && p.ContextLength > 0 {
						entry.Effective["contextLength"] = p.ContextLength
						entry.Source["contextLength"] = src
					}
					if entry.Effective["batchSize"] == nil && p.BatchSize > 0 {
						entry.Effective["batchSize"] = p.BatchSize
						entry.Source["batchSize"] = src
					}
					if entry.Effective["numGpuLayers"] == nil && p.NumGPULayers != 0 {
						entry.Effective["numGpuLayers"] = p.NumGPULayers
						entry.Source["numGpuLayers"] = src
					}
					if entry.Effective["parallel"] == nil && p.Parallel > 0 {
						entry.Effective["parallel"] = p.Parallel
						entry.Source["parallel"] = src
					}
					if entry.Effective["kvCacheType"] == nil && p.KVCacheType != "" {
						entry.Effective["kvCacheType"] = p.KVCacheType
						entry.Source["kvCacheType"] = src
					}
					// R83 (2026-09-30): «окно на клиента» из профиля/дефолта.
					if entry.Effective["contextPerSeq"] == nil && p.ContextPerSeq > 0 {
						entry.Effective["contextPerSeq"] = p.ContextPerSeq
						entry.Source["contextPerSeq"] = src
					}
				}
				prof, hasProf := s.proxy.GetModelProfile(f.Name)
				if !hasProf {
					prof, hasProf = s.proxy.GetModelProfile(strings.TrimSuffix(f.Name, ".gguf"))
				}
				if hasProf {
					cp := prof
					entry.Profile = &cp
					entry.HasProfile = true
					if prof.IgnoreDefaults {
						// Профиль есть, но его параметры отключены: оператор явно
						// просил «пусть решает окружение». Показываем это честно,
						// иначе в UI выглядело бы, что настройки применяются.
						entry.ProfileIgnored = true
					} else {
						fillFrom(prof, "profile")
					}
				}
				if hasDefaults && !defaults.IgnoreDefaults {
					fillFrom(defaults, "defaultProfile")
				}
				// Последний уровень — env контейнера cppworker.
				fillFromCppWorker(entry.Effective, entry.Source, cw)
				// R83 (2026-09-30): окно на клиента и итоговое суммарное окно.
				// Если задан contextPerSeq — он и есть окно на клиента, а суммарное
				// растёт до per × слотов (ту же арифметику применяет cppworker).
				ctxTotal, _ := intFromEffective(entry.Effective, "contextLength")
				slots, _ := intFromEffective(entry.Effective, "parallel")
				perSeq, _ := intFromEffective(entry.Effective, "contextPerSeq")
				totalEff, perEff := cppbackend.EffectiveContextSize(ctxTotal, perSeq, slots)
				if perEff > 0 {
					entry.Effective["contextPerSlot"] = perEff
					entry.Source["contextPerSlot"] = entry.Source["contextPerSeq"]
					if perSeq > 0 {
						// При явном «на клиента» суммарное окно другое — показываем,
						// сколько реально запросим у llama.cpp.
						entry.Effective["contextLengthEffective"] = totalEff
					}
				}
				files = append(files, entry)
			}
		}
	} else {
		errMsg = "нет ни одного llama.cpp бэкенда"
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	resp := map[string]interface{}{
		"files":     files,
		"total":     len(files),
		"cppworker": cw,
	}
	if hasDefaults {
		d := defaults
		resp["defaultModelProfile"] = d
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// fillFromCppWorker — добивает пустые поля значениями из env контейнера.
func fillFromCppWorker(effective map[string]interface{}, source map[string]string, cw cppworkerSnapshot) {
	setInt := func(key, cppKey string) {
		if effective[key] != nil {
			return
		}
		if v, ok := cw.intValue(cppKey); ok {
			effective[key] = v
			source[key] = "cppworker-env"
		}
	}
	setStr := func(key, cppKey string) {
		if effective[key] != nil {
			return
		}
		if v, ok := cw.strValue(cppKey); ok && v != "" {
			effective[key] = v
			source[key] = "cppworker-env"
		}
	}
	setInt("contextLength", "defaultCtxSize")
	setInt("batchSize", "defaultBatchSize")
	setInt("numGpuLayers", "defaultGpuLayers")
	setInt("parallel", "defaultNParallel")
	setStr("kvCacheType", "defaultKvCacheType")
}

// intFromEffective — целое значение из карты effective (там лежат int/float64).
func intFromEffective(m map[string]interface{}, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// fetchCppWorkerSnapshot — читает дефолты контейнера cppworker (полная картина
// «какие настройки сейчас доступны»). Недоступность бэкенда — не ошибка запроса.
func (s *Server) fetchCppWorkerSnapshot() cppworkerSnapshot {
	backendID, baseURL := s.firstCppWorkerBackend()
	snap := cppworkerSnapshot{BackendID: backendID}
	if baseURL == "" {
		snap.Error = "нет ни одного llama.cpp бэкенда"
		return snap
	}
	var payload struct {
		Config map[string]interface{} `json:"config"`
	}
	if err := s.cppworkerGetJSON(baseURL+cppworkerConfigPath, &payload); err != nil {
		snap.Error = err.Error()
		return snap
	}
	snap.Available = true
	snap.Config = payload.Config
	return snap
}

// cppworkerGetJSON — GET+decode с таймаутом (cppworker доступен внутри сети).
func (s *Server) cppworkerGetJSON(url string, out interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cppworker %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// firstCppWorkerBackend — первый пригодный llama.cpp бэкенд: (id, baseURL).
func (s *Server) firstCppWorkerBackend() (string, string) {
	st := s.proxy.GetClusterState()
	if st == nil {
		return "", ""
	}
	best := ""
	bestURL := ""
	for _, bm := range st.Backends {
		if bm.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		if bm.Status != types.StatusHealthy && best != "" {
			continue
		}
		url := fmt.Sprintf("http://%s:%d", bm.Host, bm.CppWorkerPort)
		if bm.Status == types.StatusHealthy {
			return bm.ID, url
		}
		if best == "" {
			best, bestURL = bm.ID, url
		}
	}
	return best, bestURL
}

// loadedModelsByBackend — имя модели (lowercase, с .gguf и без) → n_ctx.
func (s *Server) loadedModelsByBackend(backendID string) map[string]int {
	out := map[string]int{}
	st := s.proxy.GetClusterState()
	if st == nil {
		return out
	}
	for _, bm := range st.Backends {
		if backendID != "" && bm.ID != backendID {
			continue
		}
		for _, m := range bm.LlamaCpp.LoadedModels {
			out[strings.ToLower(m.Name)] = m.ContextLength
			out[strings.ToLower(m.Name+".gguf")] = m.ContextLength
		}
	}
	return out
}

// intValue / strValue / boolValue — безопасное чтение из map[string]interface{}
// (cppworker отдаёт конфиг как JSON-объект).
func (c cppworkerSnapshot) intValue(key string) (int, bool) {
	if c.Config == nil {
		return 0, false
	}
	v, ok := c.Config[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

func (c cppworkerSnapshot) strValue(key string) (string, bool) {
	if c.Config == nil {
		return "", false
	}
	v, ok := c.Config[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (c cppworkerSnapshot) boolValue(key string) (bool, bool) {
	if c.Config == nil {
		return false, false
	}
	v, ok := c.Config[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}
