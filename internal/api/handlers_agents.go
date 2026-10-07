package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Agents API handlers
// ============================================================

// agentRegisterHandler - регистрация агента (самостоятельная регистрация бэкенда)
func (s *Server) agentRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentID       string   `json:"agentId"`
		Hostname      string   `json:"hostname"`
		Host          string   `json:"host"`
		OllamaPort    int      `json:"ollamaPort"`
		AgentPort     int      `json:"agentPort"`
		CppWorkerPort int      `json:"cppWorkerPort"`
		GPUCount      int      `json:"gpuCount"`
		Name          string   `json:"name"`
		Labels        []string `json:"labels"`
		Weight        int      `json:"weight"`
		BackendType   string   `json:"backendType"`
		// R83 (2026-09-28): реальная вместимость воркера (n_parallel).
		// <= 0 = «не задано явно» → AddBackend применит дефолт по типу
		// (llama_cpp = 1). Раньше поле не принималось, а MaxConcurrentReqs
		// задавался константой 10 — см. комментарий у создания бэкенда ниже.
		MaxConcurrentRequests int `json:"maxConcurrentRequests"`
		// R83 (2026-09-28): токен, который ждёт сам cppworker.
		//
		// Агент — единственный, кто его знает в bundled-стеке (Go-side
		// регистрация cppworker там выключена). Без этого поля
		// Backend.CppWorkerApiToken остаётся пустым, и балансер, проксируя
		// запрос к cppworker защищённого эндпоинта, удаляет заголовок
		// авторизации (R65d) → 401 «invalid or missing API token».
		CppWorkerApiToken string `json:"cppWorkerApiToken"`
		// R-Image (2026-10-07): порт image-воркера (sdworker/sd-server).
		//
		// Нужен встроенному агенту image-бэкенда: он регистрируется ПОД ТЕМ ЖЕ
		// ID, что и саморегистрация sdworker, и обязан подтвердить тот же
		// imagePort. Без поля балансер в ветке «backend exists» не мог бы
		// отличить «агент не знает порт» от «порт равен нулю» и стирал бы
		// ImagePort, уводя EffectiveImagePort() в fallback 18093.
		//
		// <= 0 = «не задано» → сохраняем прежнее значение записи.
		ImagePort int `json:"imagePort"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Валидация
	if req.AgentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "agentId is required",
		})
		return
	}

	// R-MultiHost (2026-10-07): агент, пришедший с ДРУГОГО узла под ID уже
	// живого бэкенда, отвергается здесь — ДО ветки dedup и ДО ветки «backend
	// exists». На двух машинах с одинаковым compose совпадают буквально все
	// поля запроса (agentId, host, cppWorkerPort), поэтому раньше вторая машина
	// молча переписывала AgentID/AgentPort/метрики чужой записи: на странице
	// модели показывались CPU/GPU второй машины под URL первой, а сама вторая
	// машина не обслуживала запросов. Различить узлы позволяет только адрес
	// источника (см. api/registration_guard.go).
	if s.rejectForeignRegistration(w, r, req.AgentID) {
		return
	}

	// Если host не указан, используем hostname или RemoteAddr
	host := req.Host
	if host == "" {
		if req.Hostname != "" {
			host = req.Hostname
		} else {
			// Извлекаем IP из RemoteAddr
			host, _, _ = net.SplitHostPort(r.RemoteAddr)
			if host == "" {
				host = "localhost"
			}
		}
	}

	// Установка портов по умолчанию
	ollamaPort := req.OllamaPort
	if ollamaPort == 0 {
		ollamaPort = 11434
	}
	agentPort := req.AgentPort
	if agentPort == 0 {
		agentPort = 18032
	}

	// Определяем вес (по умолчанию 1)
	weight := req.Weight
	if weight <= 0 {
		weight = 1
	}

	// Нормализация типа бэкенда с эвристикой
	// Если тип не задан, но указан CppWorkerPort > 0 — считаем llama.cpp
	backendType := types.BackendType(req.BackendType)
	if backendType == "" {
		if req.CppWorkerPort > 0 {
			backendType = types.BackendTypeLlamaCpp
		} else {
			backendType = types.BackendTypeOllama
		}
	}

	// Round 12 (2026-07-10): agent attach to existing cppworker backend.
	// Bundled-режим: cppworker первично регистрирует inference endpoint,
	// agent при старте должен прикрепиться к этому же бэкенду (как metrics
	// provider), а не создавать второй бэкенд с тем же host:cppWorkerPort.
	// Lookup по (host, CppWorkerPort) → если найден cppworker-бэкенд →
	// attach через AttachAgentToBackend, не создаём новый.
	//
	// Round 30 (2026-08-09):
	//   1. Используем FindBackendByHostPortExcluding(req.AgentID) — иначе
	//      FindBackendByHostPort может вернуть stale standalone (с тем же
	//      ID что и req.AgentID, Go maps iteration order is random) →
	//      existingID == req.AgentID → dedup не сработает.
	//   2. После успешного attach — удаляем stale standalone бэкенд с
	//      ID == req.AgentID, если он был создан до введения dedup-логики
	//      (например, на 2026-08-06 когда cppworker ещё не был зарегистрирован).
	//      Иначе в /api/v1/backends висят 2 записи на один физический endpoint.
	if req.CppWorkerPort > 0 {
		if existingID := s.proxy.FindBackendByHostPortExcluding(host, req.CppWorkerPort, req.AgentID); existingID != "" {
			logger.Get().Infow("agentRegisterHandler: attaching to existing cppworker backend (dedup)",
				"agentId", req.AgentID,
				"existingBackendId", existingID,
				"host", host,
				"cppWorkerPort", req.CppWorkerPort)
			// R83 (2026-09-28): до attach принимаем вместимость узла.
			//
			// Дефект на живой стойке (A10): агент пересоздан с
			// AGENT_MAX_CONCURRENT_REQUESTS=1, а балансер в трёх запусках
			// подряд отдавал maxConcurrentRequests=10. Значение пришло из
			// state.json, записанного старой сборкой (создание бэкенда тогда
			// ставило константу 10), и дальше поддерживалось петлёй
			// heartbeat → агент → регистрация. Ветка dedup меняла только
			// HasAgent/AgentPort, поэтому починка создания бэкенда на
			// существующих стендах не срабатывала вовсе.
			s.proxy.AdoptCapacityFromNode(existingID, req.MaxConcurrentRequests)

			// R83 (2026-09-28): тем же шагом принимаем токен cppworker.
			//
			// Раньше attach менял только HasAgent/AgentPort, поэтому на
			// bundled-стенде (бэкенд создаёт агент, Go-side регистрация
			// cppworker выключена) токен не попадал в запись бэкенда вообще:
			// балансер удалял заголовок авторизации при проксировании на
			// cppworker, и правка параметров модели из WebUI получала
			// 401 «invalid or missing API token» — при одинаковом токене во
			// всех .env.
			s.proxy.AdoptCppWorkerToken(existingID, req.CppWorkerApiToken)

			s.proxy.AttachAgentToBackend(existingID, req.AgentID, agentPort)

			// R-MultiHost (2026-10-07): закрепляем запись за узлом источника.
			// Только так следующая регистрация с чужой машины (та же строка
			// host, тот же backendID) будет распознана как чужая.
			s.stampNodeAddr(existingID, peerIP(r))

			// Удаляем stale standalone бэкенд с тем же agentId (если есть).
			// Это нужно когда агент ранее был зарегистрирован как standalone
			// (когда dedup ещё не было), а потом cppworker зарегистрировался
			// отдельно. Теперь агент attach'ится к правильному бэкенду и
			// старый standalone должен быть удалён, чтобы избежать дублей.
			if s.proxy.BackendExists(req.AgentID) {
				logger.Get().Infow("agentRegisterHandler: removing stale standalone backend with same agentId",
					"staleBackendId", req.AgentID,
					"attachedTo", existingID)
				if err := s.proxy.RemoveBackend(req.AgentID); err != nil {
					logger.Get().Warnw("agentRegisterHandler: failed to remove stale standalone backend",
						"staleBackendId", req.AgentID, "error", err)
					// Не блокируем attach — просто warning.
				}
			}

			existing := s.proxy.GetBackend(existingID)
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"action":    "attached",
				"agentId":   req.AgentID,
				"backendId": existingID,
				"backend":   existing,
				"hasAgent":  true,
				"message":   "Agent attached to existing cppworker backend (dedup).",
			})
			return
		}
	}

	// Проверка, существует ли уже бэкенд (legacy: ID-based lookup)
	if s.proxy.BackendExists(req.AgentID) {
		// Обновляем существующий
		existing := s.proxy.GetBackend(req.AgentID)
		// R71 (2026-09-24): сохраняем всё, чего нет в payload регистрации.
		// Раньше структура собиралась с нуля, и повторная регистрация агента
		// обнуляла runtime-лимиты (операторский PUT /limits) и признак
		// «вместимость от ноды» — после чего heartbeat-«эхо» агента снова
		// перекрывало реальный n_parallel (наблюдалось на живой стойке:
		// runtime 1 → 0 при рестарте агента).
		// R83 (2026-09-28): повторная регистрация ОБЯЗАНА обновлять
		// вместимость, иначе исправление создания теряется на живом стенде.
		//
		// Наблюдение на реальной стойке: агент пересоздан с
		// AGENT_MAX_CONCURRENT_REQUESTS=1, балансер по-прежнему отдавал
		// maxConcurrentRequests=10. Причина — здесь: значение бралось ИЗ
		// existing и payload регистрации игнорировался. Дополнительно
		// завышенное 10 успевало осесть в state.json и восстанавливалось при
		// рестарте балансера, так что «починить и перезапустить» не помогало.
		//
		// Правило: агент — источник истины о своём n_parallel, но только когда
		// он его реально сообщил (>0). <=0 означает «не задано» → сохраняем
		// прежнее значение и не ломаем существующие стенды.
		//
		// Операторский лимит (PUT /limits → RuntimeMaxConcurrentRequests)
		// по-прежнему переносится из existing ниже.
		maxReqs := existing.MaxConcurrentReqs
		if req.MaxConcurrentRequests > 0 {
			maxReqs = req.MaxConcurrentRequests
		}

		// R83 (2026-09-28): токен cppworker — та же логика, что у вместимости.
		// Агент сообщил непустое значение → принимаем; иначе сохраняем прежнее
		// (сборки агента без этого поля не должны стирать токен).
		cppToken := existing.CppWorkerApiToken
		if req.CppWorkerApiToken != "" {
			cppToken = req.CppWorkerApiToken
		}

		// ПОРТЫ: берём из запроса, а при отсутствии — СОХРАНЯЕМ прежние.
		//
		// R-Image (2026-10-07): раньше ImagePort вообще не переносился, поэтому
		// регистрация агента СТИРАЛА порт image-воркера у записи image_cpp:
		// EffectiveImagePort() уходил в fallback 18093, и адрес воркера в WebUI
		// разъезжался с реальным (видно на нестандартном SDWORKER_PORT).
		// Тот же принцип для CppWorkerPort: агент Ollama-стенда его не шлёт и не
		// должен обнулять координаты чужого cppworker-бэкенда.
		imagePort := req.ImagePort
		if imagePort <= 0 {
			imagePort = existing.ImagePort
		}
		cppWorkerPort := req.CppWorkerPort
		if cppWorkerPort <= 0 {
			cppWorkerPort = existing.CppWorkerPort
		}

		updated := types.Backend{
			ID:                           req.AgentID,
			Name:                         req.Name,
			Host:                         host,
			OllamaPort:                   ollamaPort,
			AgentPort:                    agentPort,
			ImagePort:                    imagePort,
			CppWorkerPort:                cppWorkerPort,
			Weight:                       weight,
			MaxConcurrentReqs:            maxReqs,
			Labels:                       req.Labels,
			Status:                       types.StatusHealthy,
			Type:                         backendType,
			RuntimeMaxModels:             existing.RuntimeMaxModels,
			RuntimeMaxConcurrentRequests: existing.RuntimeMaxConcurrentRequests,
			RuntimeCapacityFromNode:      existing.RuntimeCapacityFromNode,
			RuntimeRequestTimeout:        existing.RuntimeRequestTimeout,
			CppWorkerApiToken:            cppToken,
			Engine:                       existing.Engine,
			ApiStyle:                     existing.ApiStyle,
			GPUMode:                      existing.GPUMode,
			// R-Image (2026-10-07): агент, зарегистрировавшийся под ID уже
			// существующего бэкенда, ОБЯЗАН включать признак HasAgent.
			//
			// Было `AgentID/HasAgent: existing.*` — то есть повторная регистрация
			// не могла ничего изменить. Следствие на живой стойке: image-бэкенд
			// (sdworker) регистрирует себя сам и создаёт запись с HasAgent=false;
			// когда под тем же ID регистрировался агент, ветка «backend exists»
			// оставляла HasAgent=false, attach не происходил, и метрики
			// GPU/VRAM/system вообще не приезжали. В WebUI это выглядело как
			// «странное отображение» страницы модели: hasAgent=false и нули в
			// gpuMemory/vramUsagePercent при живом image-воркере.
			//
			// Семантика поля: HasAgent = «у бэкенда есть агент-сборщик метрик».
			// Сам факт регистрации агента и есть доказательство — поэтому
			// выставляем true, а AgentID берём из запроса (он же req.AgentID).
			AgentID:         req.AgentID,
			HasAgent:        true,
			CppWorkerConfig: existing.CppWorkerConfig,
			OllamaConfig:    existing.OllamaConfig,
			// GPUIndex — операторское/нодовое поле: heartbeat его НЕ трогает,
			// но обязан перенести. Иначе ближайший же (каждые 30 с) heartbeat
			// молча стирал бы индекс, и лок сосуществования возвращался бы к
			// хостовому (R-Image follow-up: ключ лока = host + индекс, когда
			// индекс ИЗВЕСТЕН у обеих сторон). Копируем ЗНАЧЕНИЕ указателя:
			// nil («неизвестно») и &0 («явно GPU 0») обязаны сохраниться как
			// есть, но запись бэкенда не должна делить указатель со снимком
			// existing, который мог быть отдан наружу.
			GPUIndex: cloneGPUIndex(existing.GPUIndex),
		}
		s.proxy.UpdateBackend(req.AgentID, updated)

		// R-MultiHost (2026-10-07): запоминаем узел, за которым закреплена
		// запись (см. api/registration_guard.go).
		s.stampNodeAddr(req.AgentID, peerIP(r))

		// R83 (2026-09-28): не возвращаем секрет клиенту.
		// `updated` содержит CppWorkerApiToken (его прислал агент), а ответ
		// уходит в тело JSON. Обнуляем только копию для ответа — запись
		// бэкенда уже сохранена с токеном (UpdateBackend выше).
		safeResp := updated
		safeResp.CppWorkerApiToken = ""

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"action":  "updated",
			"agentId": req.AgentID,
			"backend": safeResp,
		})
		return
	}

	// Создание нового бэкенда
	//
	// R83 (2026-09-28): MaxConcurrentReqs НЕ задаём здесь константой.
	//
	// Было `MaxConcurrentReqs: 10` — жёстко, для ЛЮБОГО типа бэкенда. Это
	// обходило типо-зависимый дефолт в AddBackend (для llama_cpp он равен 1,
	// потому что n_parallel=1 в C-bridge) и давало cppworker'у 10 «свободных
	// слотов». Следствие на живом стенде: balance показывал
	// maxConcurrentRequests=10, admission-очередь считала, что можно слать
	// десять запросов параллельно, и клиент (Cline) получал отказы/зависания
	// там, где cppworker физически обслуживает один запрос за раз.
	//
	// Теперь: 0 = «не задано явно» → AddBackend подставит дефолт по типу.
	// Явное значение оператора (агент прислал maxConcurrentRequests > 0)
	// уважается: оно означает реальный n_parallel воркера.
	maxReqs := 0
	if req.MaxConcurrentRequests > 0 {
		maxReqs = req.MaxConcurrentRequests
	}
	backend := types.Backend{
		ID:                req.AgentID,
		Name:              req.Name,
		Host:              host,
		OllamaPort:        ollamaPort,
		AgentPort:         agentPort,
		ImagePort:         req.ImagePort,
		CppWorkerPort:     req.CppWorkerPort,
		Weight:            weight,
		MaxConcurrentReqs: maxReqs,
		// R83 (2026-09-28): токен cppworker, иначе балансер не сможет
		// авторизоваться на его защищённых эндпоинтах (см. комментарий к полю
		// в структуре запроса выше).
		CppWorkerApiToken: req.CppWorkerApiToken,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
		Type:              backendType,
	}

	// Добавление бэкенда в прокси
	if err := s.proxy.AddBackend(backend); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// R-MultiHost (2026-10-07): закрепляем запись за узлом-источником.
	s.stampNodeAddr(req.AgentID, peerIP(r))

	// Обновление статуса после health check
	go func() {
		time.Sleep(1 * time.Second)
		backend := s.proxy.GetBackend(req.AgentID)
		if backend == nil {
			return
		}

		// 2026-06-30: для llama_cpp-бэкенда проверяем cppworker /health,
		// а не Ollama /api/tags. Иначе agent-бэкенд, указывающий на
		// физический cppworker (host=cppworker-gpu:18092) и не имеющий
		// Ollama API на :11434, помечается как unhealthy сразу после
		// регистрации и "мигает" (пропадает/появляется) на странице GGUF.
		var healthURL string
		if backend.Type == types.BackendTypeLlamaCpp && backend.CppWorkerPort > 0 {
			healthURL = fmt.Sprintf("http://%s:%d/health", backend.Host, backend.CppWorkerPort)
		} else {
			healthURL = fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			s.proxy.UpdateBackendStatus(req.AgentID, types.StatusHealthy)
			if resp != nil {
				resp.Body.Close()
			}
		} else {
			if err != nil {
				logger.Get().Errorw("agent health check unreachable",
					"agent", req.AgentID,
					"url", healthURL,
					"error", err,
				)
			} else if resp != nil {
				resp.Body.Close()
			}
			s.proxy.UpdateBackendStatus(req.AgentID, types.StatusUnhealthy)
		}
	}()

	// R83 (2026-09-28): не возвращаем секрет клиенту — `backend` содержит
	// CppWorkerApiToken (его прислал агент). В записи пула токен уже сохранён
	// (AddBackend выше), поэтому обнуляем только копию для ответа.
	safeCreated := backend
	safeCreated.CppWorkerApiToken = ""

	s.writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"action":  "created",
		"agentId": req.AgentID,
		"backend": safeCreated,
		"message": "Agent registered successfully. Backend added to the pool.",
	})
}

// agentMetricsHandler - получение метрик от агента
func (s *Server) agentMetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Сначала валидируем JSON (тест ожидает 400 для невалидного JSON)
	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Игнорируем метрики, если бэкенд не зарегистрирован
	if !s.proxy.BackendExists(agentID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"status": "error",
			"error":  "Backend not found. Please register agent first.",
		})
		return
	}

	// Обновление метрик в прокси
	s.proxy.UpdateMetrics(agentID, &metrics)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(agentID, true)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "received",
	})
}

// agentHeartbeatHandler - получение heartbeat от агента (расширенный)
func (s *Server) agentHeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Читаем weight из heartbeat payload
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var hbPayload struct {
		Weight                int `json:"weight"`
		MaxConcurrentRequests int `json:"maxConcurrentRequests"`
		MaxModels             int `json:"maxModels"`
	}
	_ = json.Unmarshal(body, &hbPayload)

	// R81: heartbeat агента НЕ трогает health-статус бэкенда.
	//
	// Раньше здесь безусловно стояло UpdateBackendStatus(agentID, StatusHealthy),
	// и агент «воскрешал» бэкенд, чей cppworker уже мёртв: агент — отдельный
	// контейнер, он продолжает слать heartbeat, а health-check балансера в это же
	// время помечает бэкенд unhealthy. Итог (замер P4, R80): статус мигал
	// healthy/unhealthy, часть запросов уходила на мёртвую копию и висела до
	// клиентского таймаута (1 из 3 запросов, 120 с).
	//
	// Статусом владеет health-check (internal/balancer/health.go): он ставит
	// healthy только после успешной проверки cppworker'а и unhealthy после
	// провала. Агент отмечает лишь факт контакта (HasAgent/LastAgentContact) —
	// это делает UpdateBackendAgentStatus ниже.
	s.proxy.UpdateBackendAgentStatus(agentID, true)

	// Применяем лимиты из heartbeat агента (если > 0 — агент явно задал лимит)
	//
	// R70: бэкенд перечитываем ПРЯМО ЗДЕСЬ, а не используем снимок, взятый выше
	// по функции: иначе локальная копия могла быть снята до саморегистрации
	// cppworker'а (которая помечает вместимость как «от ноды») и heartbeat
	// перекрывал её старым «эхом» (4 вместо реального n_parallel=1).
	backend := s.proxy.GetBackend(agentID)
	if backend != nil {
		needUpdate := false
		updated := *backend

		if hbPayload.Weight > 0 && backend.Weight != hbPayload.Weight {
			updated.Weight = hbPayload.Weight
			needUpdate = true
		}
		// R71 (2026-09-24): heartbeat БОЛЬШЕ НЕ пишет вместимость.
		//
		// Найденный дефект (живая стойка): балансер отдаёт агенту
		// config.maxConcurrentRequests = runtimeMaxConcurrentRequests, агент
		// применяет его локально и в следующем heartbeat возвращает то же число
		// обратно — балансер записывал «эхо» как новую вместимость. Узел с
		// n_parallel=1 (cppworker присылает maxConcurrentRequests=1 при
		// саморегистрации) жил с порогом admission-очереди 4: устаревшая
		// константа агента (4) возвращалась быстрее, чем нода успевала её
		// исправить (heartbeat раз в ~3 с, PUT /limits откатывался за 3 с).
		//
		// Источники вместимости теперь ровно два, и оба не эхо:
		//   - нода: саморегистрация cppworker'а (MaxConcurrentReqs = n_parallel,
		//     RuntimeCapacityFromNode=true);
		//   - оператор: PUT /limits или правка бэкенда в WebUI.
		// Ниже (config) мы по-прежнему отдаём агенту эффективную вместимость,
		// чтобы его локальное значение сходилось к реальному n_parallel.
		if hbPayload.MaxModels > 0 && updated.RuntimeMaxModels != hbPayload.MaxModels {
			updated.RuntimeMaxModels = hbPayload.MaxModels
			needUpdate = true
		}

		if needUpdate {
			s.proxy.UpdateBackend(agentID, updated)
		}
	}

	now := time.Now().UTC()

	// Получаем runtime-конфигурацию бэкенда для передачи агенту
	backend = s.proxy.GetBackend(agentID)
	config := map[string]interface{}{
		"maxModels":             -1,
		"maxConcurrentRequests": -1,
	}
	if backend != nil {
		// Приоритет: Runtime-значение > статический MaxConcurrentReqs из конфига
		if backend.RuntimeMaxModels != 0 {
			config["maxModels"] = backend.RuntimeMaxModels
		} else if backend.MaxModels != 0 {
			config["maxModels"] = backend.MaxModels
		}
		// R71 (2026-09-24): вместимость отдаём по единому правилу
		// (EffectiveMaxConcurrentRequests): для саморегистрировавшейся ноды —
		// её n_parallel, иначе операторский runtime-лимит, иначе статический
		// max из конфига. Так локальное значение агента сходится к реальной
		// вместимости узла вместо того, чтобы жить собственной жизнью.
		// <= 0 означает «не менять локальное значение» на стороне агента.
		if effective := backend.EffectiveMaxConcurrentRequests(); effective > 0 {
			config["maxConcurrentRequests"] = effective
		}
		// RequestTimeout: приоритет — runtime-значение (адаптивный), затем статический
		if backend.RuntimeRequestTimeout != 0 {
			config["requestTimeout"] = backend.RuntimeRequestTimeout
		} else if backend.RequestTimeout != 0 {
			config["requestTimeout"] = backend.RequestTimeout
		}
	}

	// Формируем ответ
	response := map[string]interface{}{
		"status":       "ok",
		"serverTime":   now,
		"acknowledged": now,
		"config":       config,
	}

	// Добавляем ollamaConfig, если заданы желаемые runtime-флаги Ollama
	if backend != nil && backend.OllamaConfig != nil {
		response["ollamaConfig"] = map[string]interface{}{
			"numGpuLayers":    backend.OllamaConfig.NumGPULayers,
			"contextLength":   backend.OllamaConfig.ContextLength,
			"numParallel":     backend.OllamaConfig.NumParallel,
			"numThreads":      backend.OllamaConfig.NumThreads,
			"batchSize":       backend.OllamaConfig.BatchSize,
			"maxLoadedModels": backend.OllamaConfig.MaxLoadedModels,
			"flashAttention":  backend.OllamaConfig.FlashAttention,
			"kvCacheQuant":    backend.OllamaConfig.KVCacheQuant,
			"source":          "balancer",
		}
	}

	s.writeJSON(w, http.StatusOK, response)
}

// agentStatsHandler - получение статистики агентов
func (s *Server) agentStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := s.proxy.GetAllBackends()

	// 2026-06-30: de-dup по (host, CppWorkerPort). Без этого на bundled-стеке
	// (cppworker + agent) в /api/v1/agents/stats видно 2 записи на один
	// физический контейнер: cppworker-gpu-bundled (без агента) и cppworker-gpu
	// (с агентом). В WebUI на вкладке Agents это выглядит как дубль.
	// preferAgent=true: оставляем запись с hasAgent=true, чтобы карточка
	// показывала реальные GPU/VRAM метрики.
	backends = dedupBackendsByHostPort(
		backends,
		func(b types.Backend) string { return b.Host },
		func(b types.Backend) int { return backendEffectivePort(b.OllamaPort, b.CppWorkerPort) },
		func(b types.Backend) bool { return b.HasAgent },
		true,
	)

	agents := make([]map[string]interface{}, 0, len(backends))
	healthyCount := 0

	for _, backend := range backends {
		if backend.Status == types.StatusHealthy {
			healthyCount++
		}
		agents = append(agents, map[string]interface{}{
			"id":            backend.ID,
			"hostname":      backend.Name,
			"status":        backend.Status,
			"lastHeartbeat": backend.LastHealthCheck,
			"host":          backend.Host,
			"agentPort":     backend.AgentPort,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"totalAgents":   len(agents),
		"healthyAgents": healthyCount,
		"agents":        agents,
	})
}

// agentInfoHandler - получение информации о конкретном агенте и управление им
func (s *Server) agentInfoHandler(w http.ResponseWriter, r *http.Request) {
	// Извлечение ID из пути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Agent ID required", http.StatusBadRequest)
		return
	}

	agentID := parts[0]

	backend := s.proxy.GetBackend(agentID)
	if backend == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Agent %s not found", agentID),
		})
		return
	}

	// Подпуть /restart — прокси к агенту
	if len(parts) > 1 && parts[1] == "restart" {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Прокси-запрос к агенту
		agentURL := fmt.Sprintf("http://%s:%d/api/restart", backend.Host, backend.AgentPort)
		s.proxyAgentRequest(w, r, agentURL)
		return
	}

	// Подпуть /logs — прокси к агенту
	if len(parts) > 1 && parts[1] == "logs" {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agentURL := fmt.Sprintf("http://%s:%d/api/logs", backend.Host, backend.AgentPort)
		s.proxyAgentRequest(w, r, agentURL)
		return
	}

	// Без подпути — возвращаем информацию о бэкенде/агенте
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.writeJSON(w, http.StatusOK, backend)
}

// proxyAgentRequest — проксирует HTTP-запрос к агенту и возвращает ответ клиенту
func (s *Server) proxyAgentRequest(w http.ResponseWriter, r *http.Request, targetURL string) {
	// Создаем новый запрос к агенту
	body := &bytes.Buffer{}
	if r.Body != nil {
		io.Copy(body, r.Body)
		r.Body.Close()
	}

	proxyReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Failed to create proxy request: %v", err),
		})
		return
	}

	// Копируем заголовки
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxyReq.Header.Set("X-Agent-ID", r.Header.Get("X-Agent-ID"))

	// Выполняем запрос к агенту
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(proxyReq)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Agent unreachable: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	// Копируем ответ агента клиенту
	respBody, _ := io.ReadAll(resp.Body)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// ============================================================
// Agent V2 handlers — metrics-only agent, no duplicate backend
// ============================================================

// agentV2RegisterHandler — регистрация агента v2 (без создания бэкенда).
// POST /api/v1/agents/v2/register
// Прикрепляет агента к существующему cppworker-бэкенду по backendID
// или находит бэкенд по (host, cppWorkerPort).
func (s *Server) agentV2RegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentID       string   `json:"agentId"`
		BackendID     string   `json:"backendId"`     // ID существующего cppworker-бэкенда
		Host          string   `json:"host"`          // host cppworker (если backendID не указан)
		CppWorkerPort int      `json:"cppWorkerPort"` // порт cppworker
		AgentPort     int      `json:"agentPort"`     // порт агента
		Name          string   `json:"name"`
		Labels        []string `json:"labels"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	if req.AgentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "agentId is required",
		})
		return
	}

	host := req.Host
	if host == "" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		}
	}

	// Находим целевой бэкенд
	backendID := req.BackendID
	if backendID == "" && req.CppWorkerPort > 0 {
		// Ищем бэкенд по (host, cppWorkerPort)
		backendID = s.proxy.FindBackendByHostPort(host, req.CppWorkerPort)
	}

	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "No cppworker backend found. Register a cppworker backend first.",
		})
		return
	}

	// R-MultiHost (2026-10-07): тот же запрет захвата, что и в v1 — запись,
	// закреплённая за живым узлом, не принимает агента с другого узла.
	if s.rejectForeignRegistration(w, r, backendID) {
		return
	}

	// Прикрепляем агента к бэкенду (не создавая новый)
	s.proxy.AttachAgentToBackend(backendID, req.AgentID, req.AgentPort)
	s.stampNodeAddr(backendID, peerIP(r))

	logger.Get().Infow("agent v2 attached to backend",
		"agentID", req.AgentID,
		"backendID", backendID,
	)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"action":    "attached",
		"agentId":   req.AgentID,
		"backendId": backendID,
		"message":   "Agent v2 attached to existing backend.",
	})
}

// agentV2MetricsHandler — приём метрик от агента v2.
// POST /api/v1/agents/v2/metrics
func (s *Server) agentV2MetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "X-Agent-ID header is required",
		})
		return
	}

	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Ищем бэкенд, к которому прикреплён этот агент
	backendID := s.proxy.FindBackendByAgentID(agentID)
	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Agent not attached to any backend. Register first.",
		})
		return
	}

	s.proxy.UpdateMetrics(backendID, &metrics)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

// agentV2HeartbeatHandler — heartbeat от агента v2.
// POST /api/v1/agents/v2/heartbeat
func (s *Server) agentV2HeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "X-Agent-ID header is required",
		})
		return
	}

	// Обновляем LastAgentContact для бэкенда агента
	backendID := s.proxy.FindBackendByAgentID(agentID)
	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Agent not attached to any backend.",
		})
		return
	}

	s.proxy.TouchAgentContact(backendID)

	// Возвращаем конфигурационные параметры
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":               true,
		"maxConcurrentRequests": -1,
		"maxModels":             -1,
	})
}

// agentBackendHeartbeatHandler — heartbeat agent для конкретного backendID.
// В отличие от старого /agents/heartbeat (который использовал agentID как
// backendID, что было неправильно при attached режиме), здесь backendID
// приходит из path — это и есть canonical ID бэкенда.
//
// Валидация: X-Agent-ID должен совпадать с AgentID бэкенда (защита от
// подмены heartbeat от чужого агента).
func (s *Server) agentBackendHeartbeatHandler(w http.ResponseWriter, r *http.Request, backendID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Проверяем, что бэкенд существует
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	// Валидация: X-Agent-ID должен совпадать с прикреплённым AgentID бэкенда.
	// Если бэкенд был создан cppworker'ом (а не agent'ом), AgentID может быть пуст —
	// разрешаем attach от любого agent (первый attach становится владельцем).
	if backend.AgentID != "" && backend.AgentID != agentID {
		http.Error(w, "agent ID mismatch", http.StatusForbidden)
		return
	}

	// Читаем payload
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var hbPayload struct {
		Weight                int    `json:"weight"`
		MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
		MaxModels             int    `json:"maxModels"`
		Status                string `json:"status"`
		BackendType           string `json:"backendType"`
		// Round 14 (2026-07-10): agent шлёт свой agentPort чтобы cppworker-бэкенд
		// (который не знал про agent при создании) получил правильный port.
		// Раньше AttachAgentToBackend использовал backend.AgentPort=0, что было
		// бесполезно для UI/API.
		AgentPort int `json:"agentPort"`
	}
	_ = json.Unmarshal(body, &hbPayload)

	// R81: heartbeat агента НЕ трогает health-статус (см. подробный комментарий в
	// agentHeartbeatHandler). Раньше безусловный UpdateBackendStatus(..., Healthy)
	// возвращал в пул бэкенд с мёртвым cppworker'ом. Контакт агента отмечают
	// AttachAgentToBackend / MarkAgentContact ниже.

	// Определяем agentPort: из payload (новый способ, agent шлёт свой порт)
	// или fallback на текущий backend.AgentPort. Это решает проблему "agentPort=0"
	// когда cppworker создал бэкенд без знания про agent port.
	effectiveAgentPort := hbPayload.AgentPort
	if effectiveAgentPort == 0 {
		effectiveAgentPort = backend.AgentPort
	}

	// Если X-Agent-ID не был прикреплён к бэкенду ранее — attach'им сейчас
	if backend.AgentID == "" {
		s.proxy.AttachAgentToBackend(backendID, agentID, effectiveAgentPort)
	} else {
		// Уже прикреплён — обновляем только LastAgentContact
		s.proxy.MarkAgentContact(backendID, agentID)
		// Если agentPort ещё 0 (cppworker не знал), обновляем с effective
		if effectiveAgentPort > 0 && backend.AgentPort == 0 {
			s.proxy.UpdateAgentPort(backendID, effectiveAgentPort)
		}
	}

	// Применяем runtime-лимиты из heartbeat.
	// R70: перечитываем бэкенд (снимок выше мог быть снят до саморегистрации,
	// которая помечает вместимость как «от ноды» — см. первый heartbeat-обработчик).
	if fresh := s.proxy.GetBackend(backendID); fresh != nil {
		backend = fresh
	}
	updated := *backend
	needUpdate := false
	if hbPayload.Weight > 0 && updated.Weight != hbPayload.Weight {
		updated.Weight = hbPayload.Weight
		needUpdate = true
	}
	// R71 (2026-09-24): см. комментарий в первом heartbeat-обработчике —
	// heartbeat не пишет вместимость (это было «эхо» собственного ответа
	// балансера и держало порог очереди на устаревшем 4 при n_parallel=1).
	if hbPayload.MaxModels > 0 && updated.RuntimeMaxModels != hbPayload.MaxModels {
		updated.RuntimeMaxModels = hbPayload.MaxModels
		needUpdate = true
	}
	if needUpdate {
		s.proxy.UpdateBackend(backendID, updated)
	}

	logger.Get().Debugw("agent heartbeat via backends/{id}/agent/heartbeat",
		"backend", backendID, "agent", agentID,
		"weight", hbPayload.Weight, "maxConcurrent", hbPayload.MaxConcurrentRequests)

	// R71: отдаём агенту эффективную вместимость (нода-саморегистрация →
	// её n_parallel, иначе операторский лимит), чтобы его локальное значение
	// сходилось с реальной вместимостью узла. Раньше здесь всегда было -1
	// («не меняем»), и агент мог держать локальный лимит, разошедшийся с нодой.
	capacity := -1
	if effective := backend.EffectiveMaxConcurrentRequests(); effective > 0 {
		capacity = effective
	}

	// Возвращаем runtime-лимиты от балансера
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"config": map[string]interface{}{
			"maxConcurrentRequests": capacity,
			"maxModels":             -1,
		},
	})
}

// agentBackendMetricsHandler — приём метрик от прикреплённого агента по правильному backendID.
// Round 15 (2026-07-10): в Round 12 dedup сделал 1 бэкенд на физический inference
// endpoint (cppworker-gpu-bundled). Agent ID стал отличаться от backend ID
// (cppworker-gpu-bundled-agent). Старый endpoint /api/v1/agents/metrics использовал
// agentID как ключ для BackendExists() → 404 "Backend not found" → метрики
// не доходили до балансера и WebUI показывал пустые GPU/VRAM/CPU/RAM.
//
// Новый endpoint /api/v1/backends/{backendID}/agent/metrics принимает backendID
// из path (тот самый что был получен при register через response.backendId).
func (s *Server) agentBackendMetricsHandler(w http.ResponseWriter, r *http.Request, backendID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Парсим метрики
	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Проверяем что backend существует
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	// Валидация: X-Agent-ID должен совпадать с прикреплённым AgentID бэкенда
	// (защита от подмены — чужой агент не может слать метрики от имени чужого бэкенда).
	// При первом attach (backend.AgentID пуст) — allow, потом attach ниже.
	if backend.AgentID != "" && backend.AgentID != agentID {
		http.Error(w, "agent ID mismatch", http.StatusForbidden)
		return
	}

	// Auto-attach если ещё не прикреплён
	if backend.AgentID == "" {
		s.proxy.AttachAgentToBackend(backendID, agentID, backend.AgentPort)
	}

	// Обновляем метрики для правильного backendID
	s.proxy.UpdateMetrics(backendID, &metrics)

	// R83 (2026-09-25): провал загрузки → уведомление в WebUI (bell-меню).
	//
	// Цепочка: cppworker /api/models → agent (уже опрашивает) → этот push →
	// EventBus → SSE → notifications.js. Балансер здесь ничего не опрашивает и не
	// агрегирует: только одно сравнение на полученный push. Дедупликация внутри
	// PublishLoadFailureTransition обязательна — agent шлёт метрики каждые ~10 с,
	// а запись о провале живёт 10 минут (SSE-буфер вмещает всего 100 событий).
	s.proxy.PublishLoadFailureTransition(backendID, metrics.LoadFailure)

	// R83 §9.4 шаг 1б (2026-09-26), вариант D: «загружено, но без GPU» —
	// отдельное уведомление (severity=warning). Дедупликация своя: cpu_only не
	// должен ни глушить, ни глушиться провалом загрузки (см.
	// PublishLoadDegradedTransition).
	s.proxy.PublishLoadDegradedTransition(backendID, metrics.LoadDegraded)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(backendID, true)

	// Обновляем LastAgentContact (лёгкий путь — без race-condition)
	s.proxy.MarkAgentContact(backendID, agentID)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "received",
	})
}
