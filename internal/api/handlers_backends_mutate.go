package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
	"time"
)

func (s *Server) addBackend(w http.ResponseWriter, r *http.Request) {
	var req backendRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Валидация
	if req.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend ID is required",
		})
		return
	}
	if req.Host == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend host is required",
		})
		return
	}

	// R-MultiHost (2026-10-07): саморегистрация узла, который выдаёт себя за
	// бэкенд, уже обслуживаемый ДРУГОЙ машиной, отвергается здесь — до всех
	// веток обновления. Иначе вторая машина с тем же compose (те же
	// backendID/host/порт) молча переписывала бы чужую запись.
	if s.rejectForeignRegistration(w, r, req.ID) {
		return
	}

	// Проверка на дубликат.
	//
	// R70 (2026-09-24): саморегистрация cppworker'а (`POST /api/v1/backends` при
	// старте) должна быть ИДЕМПОТЕНТНОЙ. Раньше на существующий ID возвращался
	// 409, и после перезапуска cppworker'а с изменённой вместимостью (например
	// n_parallel=1 вместо старой константы 4) балансер продолжал держать старые
	// параметры: admission-очередь включалась на неверном пороге, а «лишние»
	// запросы блокировались внутри cppworker без позиции в очереди и keepalive.
	//
	// Обновляем ТОЛЬКО если это тот же бэкенд (совпадают host и cppWorkerPort) —
	// иначе 409, чтобы WebUI не перезаписал чужой бэкенд по ошибке.
	if s.proxy.BackendExists(req.ID) {
		if !s.isSameBackendRegistration(req) && !s.isReRegistrationOfAutoBackend(req) {
			s.writeJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Backend with ID %s already exists", req.ID),
			})
			return
		}
		// R-MultiHost (2026-10-07): обновляем и узел-владелец записи, чтобы
		// устаревший адрес не «защищал» ID от законного пересозданного
		// контейнера (и наоборот — чтобы takeover чужой живой записи уже был
		// отвергнут guard'ом выше).
		s.stampNodeAddr(req.ID, peerIP(r))
		s.refreshRegisteredBackend(w, req)
		return
	}

	// Нормализация типа бэкенда.
	// Приоритет: req.BackendType > req.BackendEngine > s.config.BackendEngine > ollama
	backendType := types.BackendType(req.BackendType)
	if backendType == "" {
		// Пытаемся определить из BackendEngine в запросе (клиент мог прислать)
		if req.BackendEngine != "" {
			backendType = types.BackendEngine(req.BackendEngine).ToBackendType()
		}
		// Если клиент не прислал — берём из глобального конфига сервера
		if backendType == "" && s.config.BackendEngine != "" {
			backendType = s.config.BackendEngine.ToBackendType()
		}
		// Последний fallback — ollama (обратная совместимость)
		if backendType == "" {
			backendType = types.BackendTypeOllama
		}
	}

	// Валидация типа бэкенда
	// R-Image (2026-09-27): добавлен image_cpp — отдельный бэкенд генерации
	// изображений (свой порт ImagePort, свой каталог моделей, OpenAI/A1111 API).
	if backendType != types.BackendTypeOllama && backendType != types.BackendTypeLlamaCpp &&
		backendType != types.BackendTypeImage {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid backend type: %s. Allowed: ollama, llama_cpp, image_cpp", req.BackendType),
		})
		return
	}

	// Round 51.2 (2026-08-20): валидация apiStyle на HTTP-границе.
	// Пустое значение допустимо (EffectiveAPIStyle() выведет из Type автоматически).
	// Явное невалидное значение — 400, чтобы оператор увидел ошибку сразу, а не
	// после тихого fallback'а. Внутренний helper остаётся tolerant для state.json,
	// который мог быть записан с опечаткой до R51.2.
	if req.ApiStyle != "" && !types.APIStyle(req.ApiStyle).IsValidAPIStyle() {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid apiStyle: '%s'. Allowed: ollama-native, openai-compatible (or empty for auto-inference from type)", req.ApiStyle),
		})
		return
	}

	// Проверка совместимости типа бэкенда с OperatingMode
	opMode := s.config.Balancing.OperatingMode
	if !types.IsModeCompatibleWithBackendType(opMode, backendType) {
		allowedTypes := types.ModeBackendTypes[opMode]
		allowedStr := ""
		for i, t := range allowedTypes {
			if i > 0 {
				allowedStr += ", "
			}
			allowedStr += string(t)
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", backendType, opMode, allowedStr),
		})
		return
	}

	// Парсинг GPU mode
	gpuMode := types.ModeAuto
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	}

	// Если CppWorkerPort не задан явно, используем актуальный default 18092
	// (18091 — legacy; современные llama.cpp CppWorker слушают на 18092).
	cppWorkerPort := req.CppWorkerPort
	if cppWorkerPort == 0 && backendType == types.BackendTypeLlamaCpp {
		cppWorkerPort = 18092
	}

	// R-Image: image-бэкенд слушает свой порт (default 18093). CppWorkerPort для
	// него не трогаем — EffectiveImagePort() даст ImagePort → CppWorkerPort → 18093.
	imagePort := req.ImagePort
	if imagePort == 0 && backendType == types.BackendTypeImage {
		imagePort = types.DefaultImageWorkerPort
	}

	backend := types.Backend{
		ID:                req.ID,
		Name:              req.Name,
		Host:              req.Host,
		OllamaPort:        req.OllamaPort,
		AgentPort:         req.AgentPort,
		CppWorkerPort:     cppWorkerPort,
		ImagePort:         imagePort,
		Weight:            req.Weight,
		MaxConcurrentReqs: req.MaxConcurrentReqs,
		MaxModels:         req.MaxModels,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
		GPUMode:           gpuMode,
		Type:              backendType,
		CppWorkerApiToken: req.CppWorkerApiToken,
		Engine:            types.ResolveEngine(types.BackendEngine(req.BackendEngine), backendType),
		// Round 51.2 (2026-08-20): если req.ApiStyle непусто и валидно — используем как есть.
		// Иначе Backend.EffectiveAPIStyle() выведет из Type автоматически.
		ApiStyle:  types.APIStyle(req.ApiStyle),
		BuildInfo: req.BuildInfo,
		// R486: релизная идентичность образа воркера — из запечённых значений.
		WorkerVersion:   req.WorkerVersion,
		WorkerCommit:    req.WorkerCommit,
		WorkerBuildDate: req.WorkerBuildDate,
		WorkerRPCProto:  req.WorkerRPCProto,
		// R493-12i: идентичность агента отдельно + источник worker*-полей.
		AgentVersion:         req.AgentVersion,
		AgentCommit:          req.AgentCommit,
		WorkerIdentitySource: req.WorkerIdentitySource,
		// R118 (2026-10-14): роль бэкенда в раскладке слоёв — операторское
		// решение, оно живёт в persistent-override (backend-roles.json). Именно
		// ЭТОТ путь создаёт запись для bundled-агента (POST /api/v1/backends),
		// поэтому роль надо подставлять здесь, иначе после перезапуска балансера
		// (записи пересоздаются) главный оставался без доноров — проверено живьём.
		Role: s.roleForBackend(req.ID, ""),
	}

	// GPUIndex: копируем ЗНАЧЕНИЕ из запроса, а не переиспользуем его указатель
	// (состояние бэкенда не должно зависеть от времени жизни тела запроса).
	// nil = «индекс не объявлен» → ключ лока сосуществования остаётся хостовым;
	// &0 = нода/оператор ЯВНО сказали «первая карта».
	if req.GPUIndex != nil {
		backend.GPUIndex = types.GPUIndexPtr(*req.GPUIndex)
	}

	// Добавление бэкенда в прокси
	if err := s.proxy.AddBackend(backend); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// R-MultiHost (2026-10-07): запоминаем узел, за которым закреплена запись.
	// Без этого следующая регистрация с ТОЙ ЖЕ строкой host (вторая машина с
	// тем же compose) снова считалась бы «своей»: протокол не несёт других
	// признаков, различающих узлы.
	s.stampNodeAddr(req.ID, peerIP(r))

	// Запуск health check для нового бэкенда
	go func() {
		time.Sleep(1 * time.Second)
		backend := s.proxy.GetBackend(req.ID)
		if backend != nil {
			var healthURLs []string
			switch backendType {
			case types.BackendTypeLlamaCpp:
				healthURLs = []string{fmt.Sprintf("http://%s:%d/health", backend.Host, cppWorkerPort)}
			case types.BackendTypeImage:
				// R-Image: на порту image-бэкенда может стоять либо наш воркер
				// `sdworker` (есть /health), либо голый sd-server (есть только
				// /sdcpp/v1/capabilities). Проверяем оба адреса по очереди.
				imgPort := backend.EffectiveImagePort()
				healthURLs = []string{
					fmt.Sprintf("http://%s:%d/health", backend.Host, imgPort),
					fmt.Sprintf("http://%s:%d/sdcpp/v1/capabilities", backend.Host, imgPort),
				}
			default:
				healthURLs = []string{fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)}
			}
			client := &http.Client{Timeout: 5 * time.Second}
			healthy := false
			var lastErr error
			var lastURL string
			for _, healthURL := range healthURLs {
				lastURL = healthURL
				resp, err := client.Get(healthURL)
				if err != nil {
					lastErr = err
					continue
				}
				status := resp.StatusCode
				resp.Body.Close()
				if status == http.StatusOK {
					healthy = true
					break
				}
				lastErr = fmt.Errorf("status code: %d", status)
			}
			if healthy {
				s.proxy.UpdateBackendStatus(req.ID, types.StatusHealthy)
			} else {
				if lastErr != nil {
					logger.Get().Errorw("backend health check unreachable",
						"backend", req.ID,
						"url", lastURL,
						"error", lastErr,
					)
				}
				s.proxy.UpdateBackendStatus(req.ID, types.StatusUnhealthy)
			}
		}
	}()

	s.writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"backend": backend,
		"message": "Backend added successfully. Health check will run automatically.",
	})
}

// cloneGPUIndex — копия указателя индекса GPU (nil-safe).
//
// Нужна там, где состояние бэкенда пересобирается из снимка (heartbeat
// агента): «неизвестно» (nil) и «явно GPU 0» (&0) обязаны сохраниться как есть,
// но новая запись не должна делить указатель со снимком, который уже отдан
// наружу (иначе запись по указателю в одном месте меняла бы индекс в другом).
func cloneGPUIndex(src *int) *int {
	if src == nil {
		return nil
	}
	return types.GPUIndexPtr(*src)
}

// applyGPUIndexUpdate — merge-семантика gpuIndex для PUT /api/v1/backends/{id}.
//
// ТРИ исхода (ровно этого требует R-Image follow-up: индекс GPU 0 должен быть
// объявляемым, а индекс — сбрасываемым):
//
//	ключа в теле нет   → current (не менять): частичный PUT из WebUI не должен
//	                     стирать настройку, от которой зависит ключ лока
//	                     сосуществования image/text;
//	"gpuIndex": null   → nil (СБРОС): индекс снова «неизвестен», лок хостовый;
//	"gpuIndex": <int>  → явный индекс; 0 — валидная ПЕРВАЯ карта.
//
// Отрицательный индекс — 400: «GPU -1» смысла не имеет, а молча проглотить его
// значило бы записать в state.json значение, которое EffectiveGPUIndex() потом
// трактует как «неизвестно» (и оператор не поймёт, почему лок не сузился).
func applyGPUIndexUpdate(raw json.RawMessage, current *int) (*int, error) {
	if len(raw) == 0 {
		return current, nil // ключ отсутствует — не менять
	}
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, nil // явный сброс в «неизвестно»
	}
	var idx int
	if err := json.Unmarshal(trimmed, &idx); err != nil {
		return current, fmt.Errorf("Invalid gpuIndex: expected an integer or null, got %s", string(trimmed))
	}
	if idx < 0 {
		return current, fmt.Errorf("Invalid gpuIndex: %d (must be >= 0, or null to reset)", idx)
	}
	return types.GPUIndexPtr(idx), nil
}

// updateBackend - обновление бэкенда
func (s *Server) updateBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		Name          string `json:"name"`
		Host          string `json:"host"`
		OllamaPort    int    `json:"ollamaPort"`
		AgentPort     int    `json:"agentPort"`
		CppWorkerPort int    `json:"cppWorkerPort"`
		// R-Image (2026-09-27): порт image-воркера для backendType=image_cpp
		// (default 18093). Fallback: CppWorkerPort → 18093 (EffectiveImagePort).
		ImagePort int `json:"imagePort"`
		// GPUIndex — индекс GPU. Здесь json.RawMessage, а не *int: у PUT
		// ТРИ исхода, и «ключ отсутствует» обязан отличаться от «null»:
		//   - ключа нет            → индекс не менять (частичный PUT из WebUI);
		//   - `"gpuIndex": null`   → СБРОСИТЬ индекс (снова «неизвестно»,
		//                            лок сосуществования — хостовый);
		//   - `"gpuIndex": 0`      → ЯВНО GPU 0 (валидная первая карта).
		// *int не различает первые два случая: json.Unmarshal кладёт nil и в
		// отсутствующий ключ, и в null (ровно из-за этого сбросить индекс через
		// PUT раньше было нельзя). См. applyGPUIndexUpdate.
		GPUIndex          json.RawMessage `json:"gpuIndex"`
		Weight            int             `json:"weight"`
		MaxConcurrentReqs int             `json:"maxConcurrentRequests"`
		MaxModels         int             `json:"maxModels"`
		GPUMode           string          `json:"gpuMode"`
		Labels            []string        `json:"labels"`
		BackendType       string          `json:"backendType"`
		BackendEngine     string          `json:"backendEngine"`
		// Round 51.2 (2026-08-20): явный API-стиль. Семантика:
		//   - непустое значение → устанавливается
		//   - пустая строка → сохраняется существующее значение (для сброса
		//     потребуется явный "manual reset" в state.json; см. R51.3+)
		ApiStyle string `json:"apiStyle"`
		// Round 7 (2026-07-09): см. addBackend — обновление тоже должно
		// принимать CppWorkerApiToken, иначе при re-registration (PUT update)
		// после первого запуска cppworker теряет свой токен в backend state.
		CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`
		// R101 (S1 плана 2026-10-07): версия сборки llama.cpp — приходит и при
		// регистрации, и при heartbeat; без неё в этом запросе проверка
		// совместимости сторон RPC стиралась бы на первом обновлении статуса.
		BuildInfo string `json:"buildInfo,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Проверка существования бэкенда
	existing := s.proxy.GetBackend(backendID)
	if existing == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// Парсинг GPU mode
	gpuMode := existing.GPUMode
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	case "auto":
		gpuMode = types.ModeAuto
	}

	// Нормализация типа бэкенда
	backendType := existing.Type
	if req.BackendType != "" {
		newType := types.BackendType(req.BackendType)
		if newType != existing.Type {
			// Проверка совместимости с OperatingMode
			if !types.IsModeCompatibleWithBackendType(s.config.Balancing.OperatingMode, newType) {
				allowedTypes := types.ModeBackendTypes[s.config.Balancing.OperatingMode]
				allowedStr := ""
				for i, t := range allowedTypes {
					if i > 0 {
						allowedStr += ", "
					}
					allowedStr += string(t)
				}
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", newType, s.config.Balancing.OperatingMode, allowedStr),
				})
				return
			}
			// Запрет смены типа при активных запросах
			if existing.ActiveRequests > 0 {
				s.writeJSON(w, http.StatusConflict, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Cannot change backend type from '%s' to '%s': backend has %d active requests. Wait for requests to complete or drain the backend first.", existing.Type, newType, existing.ActiveRequests),
				})
				return
			}
			logger.Get().Warnw("backend type changed via API",
				"backend", backendID,
				"oldType", existing.Type,
				"newType", newType,
			)
		}
		backendType = newType
	}

	// Применяем CppWorkerPort если передан, иначе сохраняем существующий
	cppWorkerPort := existing.CppWorkerPort
	if req.CppWorkerPort > 0 {
		cppWorkerPort = req.CppWorkerPort
	}

	// R-Image: порт image-воркера — та же merge-семантика, что у CppWorkerPort.
	imagePort := existing.ImagePort
	if req.ImagePort > 0 {
		imagePort = req.ImagePort
	}

	// GPUIndex — merge-семантика с ВОЗМОЖНОСТЬЮ СБРОСА (см. applyGPUIndexUpdate):
	// ключа нет → не менять; null → сбросить в nil («индекс неизвестен», лок
	// сосуществования снова хостовый); 0 → явная первая карта.
	gpuIndex, gpuIndexErr := applyGPUIndexUpdate(req.GPUIndex, existing.GPUIndex)
	if gpuIndexErr != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   gpuIndexErr.Error(),
		})
		return
	}

	// Round 51.2 (2026-08-20): применяем apiStyle если передан, иначе сохраняем.
	// Если оператор явно прислал невалидное значение — 400 (тот же контракт, что
	// в addBackend, для консистентности).
	if req.ApiStyle != "" && !types.APIStyle(req.ApiStyle).IsValidAPIStyle() {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid apiStyle: '%s'. Allowed: ollama-native, openai-compatible (or empty to preserve existing)", req.ApiStyle),
		})
		return
	}
	apiStyle := existing.ApiStyle
	if req.ApiStyle != "" {
		apiStyle = types.APIStyle(req.ApiStyle)
	}

	// R65d (2026-09-20): fallback для cppWorkerApiToken — как у cppWorkerPort
	// и apiStyle выше.
	//
	// Найденный дефект: поле присваивалось напрямую из запроса
	// (`CppWorkerApiToken: req.CppWorkerApiToken`), а WebUI его НЕ отправляет
	// (app-modals.js формирует payload без токена — оператор не должен видеть
	// секрет). Поэтому любое редактирование бэкенда через UI стирало токен, и
	// последующие вызовы балансер→cppworker, требующие авторизации
	// (POST /api/models/reload из per-model profiles,
	// internal/api/gguf_backend_proxy.go) начинали получать 401.
	//
	// Защита от случайного стирания: пустая строка = «не менять».
	// Осознанно очистить токен можно, прислав явный маркер "-" (сервер не сможет
	// отличить "поле не прислано" от "прислано пустым" иначе).
	cppWorkerAPIToken := existing.CppWorkerApiToken
	if req.CppWorkerApiToken != "" {
		if req.CppWorkerApiToken == "-" {
			cppWorkerAPIToken = ""
		} else {
			cppWorkerAPIToken = req.CppWorkerApiToken
		}
	}

	// R65d (2026-09-20): MERGE-семантика вместо «поле-в-поле».
	//
	// Было: структура собиралась из req целиком, поэтому ЛЮБОЙ частичный PUT
	// уничтожал не переданные поля. Практический сценарий поломки — кнопка
	// «Auto-detect cppworker port» в WebUI (app-modals.js): она отправляет
	// `{cppWorkerPort: <detected>}` без остальных полей, и бэкенд получал
	// host="", name="", weight=0, labels=nil, maxConcurrentReqs=0 — то есть
	// становился нероутируемым (пустой host).
	//
	// Теперь: база — existingConfig, поверх применяются только реально
	// переданные значения. Для строк/слайсов признак «передано» — непустое
	// значение (очистить их через эту ручку нельзя, что и правильно: host и name
	// обязательны для работающего бэкенда).
	name := existing.Name
	if req.Name != "" {
		name = req.Name
	}
	host := existing.Host
	if req.Host != "" {
		host = req.Host
	}
	ollamaPort := existing.OllamaPort
	if req.OllamaPort > 0 {
		ollamaPort = req.OllamaPort
	}
	agentPort := existing.AgentPort
	if req.AgentPort > 0 {
		agentPort = req.AgentPort
	}
	// weight/maxConcurrentReqs/maxModels: 0 = «не задано» (значение 0 смысла не
	// имеет — это отсутствие лимита, которое выражается -1).
	weight := existing.Weight
	if req.Weight > 0 {
		weight = req.Weight
	}
	maxConcurrent := existing.MaxConcurrentReqs
	if req.MaxConcurrentReqs != 0 {
		maxConcurrent = req.MaxConcurrentReqs
	}
	maxModels := existing.MaxModels
	if req.MaxModels != 0 {
		maxModels = req.MaxModels
	}
	labels := existing.Labels
	if req.Labels != nil {
		labels = req.Labels
	}

	updated := types.Backend{
		ID:                backendID,
		Name:              name,
		Host:              host,
		OllamaPort:        ollamaPort,
		AgentPort:         agentPort,
		CppWorkerPort:     cppWorkerPort,
		ImagePort:         imagePort,
		GPUIndex:          gpuIndex,
		Weight:            weight,
		MaxConcurrentReqs: maxConcurrent,
		MaxModels:         maxModels,
		Labels:            labels,
		Status:            existing.Status,
		CppWorkerApiToken: cppWorkerAPIToken,
		// R101: версия сборки llama.cpp едет вместе с бэкендом — иначе heartbeat
		// (этот путь пересобирает структуру целиком) стирал бы её, и проверка
		// совместимости сторон RPC снова стала бы «неизвестно».
		BuildInfo:                    buildInfoOrKeepR101(req.BuildInfo, existing.BuildInfo),
		HasAgent:                     existing.HasAgent,
		LastAgentContact:             existing.LastAgentContact,
		LastHealthCheck:              existing.LastHealthCheck,
		ConsecutiveFailures:          existing.ConsecutiveFailures,
		ActiveRequests:               existing.ActiveRequests,
		RuntimeMaxModels:             existing.RuntimeMaxModels,
		RuntimeMaxConcurrentRequests: existing.RuntimeMaxConcurrentRequests,
		// R70: оператор меняет бэкенд из WebUI — снимаем признак «вместимость
		// от ноды», чтобы heartbeat агента снова мог её обновлять.
		RuntimeCapacityFromNode: false,
		GPUMode:                 gpuMode,
		Type:                    backendType,
		ApiStyle:                apiStyle,
		// Engine тоже не отправляется из WebUI (в request-структуре его нет).
		// Без этого он обнулялся при каждом сохранении, и бэкенд терял признак
		// движка (используется в /api/v1/backends и при выборе стратегии запуска).
		Engine: existing.Engine,
		// R-MultiHost (2026-10-07): узел-владелец — часть записи, а не то, что
		// оператор задаёт в WebUI. Реконструкция по полям его затирала, и
		// сохранение бэкенда из WebUI снимало защиту от захвата чужой машиной.
		NodeAddr: existing.NodeAddr,
		// R118 (2026-10-14): роль в раскладке слоёв — операторское решение; этот
		// путь (PUT/правка бэкенда) пересобирает запись целиком, поэтому роль
		// переносим (как Runtime*-поля и Engine выше).
		Role: s.roleForBackend(backendID, existing.Role),
		// R250 (2026-10-28): «не использовать бэкенд» — тоже операторское решение,
		// и этот путь пересобирает запись ЦЕЛИКОМ. Без переноса обычное
		// сохранение бэкенда из WebUI (например «Auto-detect cppworker port»)
		// молча возвращало выключенный узел в работу.
		Disabled:       existing.Disabled,
		DisabledReason: existing.DisabledReason,
		DisabledAt:     cloneDisabledSince(existing.DisabledAt),
	}

	// Обновление бэкенда в прокси
	if err := s.proxy.UpdateBackend(backendID, updated); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"backend": updated,
		"message": "Backend updated successfully",
	})
}

// deleteBackend - удаление бэкенда
func (s *Server) deleteBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	// Проверка существования бэкенда
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// R118.10 (2026-10-14): УЗЕЛ С ЯВНОЙ РОЛЬЮ НЕ УДАЛЯЕМ — ОСТАВЛЯЕМ В СПИСКЕ.
	//
	// Требование оператора: «оставлять узел в списке, так как может происходить
	// настройка». Штатную остановку узла сопровождает best-effort
	// `DELETE /api/v1/backends/{id}` от самого cppworker'а
	// (cmd/cppworker/balancer_register.go) — и раньше это стирало запись вместе с
	// ролью в раскладке: остановил узел, чтобы настроить, — потерял и роль, и
	// раскладку (живая проверка: запись донора исчезала из /api/v1/backends через
	// ~30 с после docker stop).
	//
	// Теперь запись с ролью не удаляется, а «отцепляется» от агента: статус
	// offline, роль и раскладка целы, панель продолжает показывать узел и
	// позволяет его настраивать. Вернувшийся агент прикрепится к той же записи.
	// Явное удаление оператором по-прежнему возможно — сначала снимите роль
	// (PUT .../role с пустым значением), затем DELETE.
	if backend := s.proxy.GetBackend(backendID); backend != nil && strings.TrimSpace(backend.Role) != "" {
		role := strings.TrimSpace(backend.Role)
		detached := s.proxy.DetachAgentFromBackend(backendID)
		logger.Get().Warnw(i18n.T("api.handlers_backends.delete_backend_role_kept"),
			"backend", backendID, "role", role, "detached", detached,
			"hint", i18n.T("api.handlers_backends.delete_backend_role_hint"))
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":  true,
			"id":       backendID,
			"kept":     true,
			"detached": detached,
			"role":     role,
			"message":  i18n.T("api.handlers_backends.delete_backend_role_kept_message"),
		})
		return
	}

	// Удаление бэкенда из прокси
	if err := s.proxy.RemoveBackend(backendID); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// R398 (2026-11-04): удаление обязано доехать до config.json.
	//
	// ЖИВОЙ ФАКТ. RemoveBackend чистит runtime и state.json, но не статический
	// конфиг. После рестарта балансер снова читает config.Backends — удалённый
	// оператором узел ВОЗВРАЩАЛСЯ и его начинали дёргать apply/перезагрузки
	// («затрагивается другой бэкенд»: в ответе apply появлялись cppworker-donor /
	// fake-donor с connection refused, хотя выбран совсем другой бэкенд).
	removedFromConfig := false
	if s.config != nil {
		kept := make([]types.Backend, 0, len(s.config.Backends))
		for _, cb := range s.config.Backends {
			if cb.ID == backendID {
				removedFromConfig = true
				continue
			}
			kept = append(kept, cb)
		}
		if removedFromConfig {
			s.config.Backends = kept
			if s.configSaver != nil {
				if err := s.configSaver(); err != nil {
					logger.Get().Warnw("deleteBackend: не удалось сохранить config.json после удаления",
						"backend", backendID, "error", err)
				}
			}
		}
	}
	logger.Get().Infow("deleteBackend: бэкенд удалён",
		"backend", backendID, "removed_from_config", removedFromConfig)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":           true,
		"id":                backendID,
		"message":           "Backend removed successfully",
		"removedFromConfig": removedFromConfig,
	})
}
