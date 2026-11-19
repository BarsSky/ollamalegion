package api

import (
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"sort"
	"strings"
	"time"
)

// listBackends - получение списка бэкендов
func (s *Server) listBackends(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]interface{}, 0)

	// По умолчанию скрываем нерабочие бэкенды, чтобы в WebUI не отображались
	// заглушки/недоступные ноды. Параметр includeUnhealthy=true возвращает всё.
	includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"
	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	// Получение всех бэкендов из прокси
	allBackends := s.proxy.GetAllBackends()

	// Сортировка по ID для стабильного порядка
	sort.Slice(allBackends, func(i, j int) bool {
		return allBackends[i].ID < allBackends[j].ID
	})

	// 2026-06-30: de-dup по (host, CppWorkerPort). Без этого на bundled-стеке
	// (cppworker + agent) в /api/v1/backends видно 2-3 записи на один
	// физический контейнер (cppworker-gpu-bundled + cppworker-gpu от агента).
	// На вкладке Backends WebUI это выглядит как «один и тот же бэкенд дважды»,
	// а в selectBackend вызывает race. См. internal/api/dedup.go.
	allBackends = dedupBackendsByEndpoint(
		allBackends,
		func(b types.Backend) endpointKey {
			return endpointKey{Host: b.Host, Port: backendDedupPort(b), NodeAddr: b.NodeAddr}
		},
		func(b types.Backend) bool { return b.HasAgent },
		true, // preferAgent: бэкенд с агентом даёт реальные GPU/VRAM метрики
	)

	// Получение метрик из состояния кластера
	state := s.proxy.GetClusterState()

	// Создание карты метрик для быстрого доступа
	metricsMap := make(map[string]*types.BackendMetrics)
	for i := range state.Backends {
		metricsMap[state.Backends[i].ID] = &state.Backends[i]
	}

	// Формирование ответа для каждого бэкенда
	for _, backend := range allBackends {
		// Фильтруем нерабочие бэкенды, если не запрошено явное включение.
		//
		// R118.10 (2026-10-14): узел с ЯВНОЙ ролью в раскладке показываем ВСЕГДА,
		// даже offline/unhealthy. Требование оператора — «оставлять узел в списке,
		// так как может происходить настройка»: остановленный узел (или узел,
		// который настраивают) должен быть виден в панели вместе со своей ролью,
		// иначе настраивать нечего. Обычные нерабочие записи фильтруются как раньше.
		if !includeUnhealthy && unhealthyStatuses[backend.Status] && strings.TrimSpace(backend.Role) == "" {
			continue
		}
		// R304: пометка донора считается один раз на запись списка. Источник —
		// снимок ролей в балансере и снапшот поллера, без походов в сеть.
		donorMark := s.proxy.DonorMarkR304(backend.ID)
		// R71: вместимость по единому правилу (нода-саморегистрация → n_parallel,
		// иначе операторский runtime-лимит, иначе статический max). Отчёт должен
		// совпадать с тем, на что реально встаёт admission-очередь.
		// R117: вместе с числом отдаём источник (ResolveCapacity) — см. ниже.
		maxConcurrent, capacitySource := backend.ResolveCapacity()
		// Приоритет: RuntimeMaxModels > MaxModels
		maxModels := backend.MaxModels
		if backend.RuntimeMaxModels != 0 {
			maxModels = backend.RuntimeMaxModels
		}

		backendData := map[string]interface{}{
			"id":            backend.ID,
			"name":          backend.Name,
			"host":          backend.Host,
			"ollamaPort":    backend.OllamaPort,
			"agentPort":     backend.AgentPort,
			"cppWorkerPort": backend.CppWorkerPort,
			// R-Image (2026-10-02): порт image-воркера. Найдено живым E2E:
			// POST отдавал imagePort, а GET /api/v1/backends — нет, поэтому
			// потребитель, берущий порт из списка, молча падал на дефолт 18093
			// и на нестандартном порту уходил бы не туда.
			"imagePort":             backend.ImagePort,
			"weight":                backend.Weight,
			"maxConcurrentRequests": maxConcurrent,
			// R508 / §6 аудита S3: то же число под тем же именем, что в
			// /api/v1/cluster (`effectiveMaxConcurrentRequests`) и в WebUI.
			//
			// ЗАЧЕМ ДУБЛЬ. Проверка §6 сравнивает три поверхности, и на
			// /api/v1/backends этого поля не было: `maxConcurrentRequests` здесь
			// УЖЕ эффективная вместимость (ResolveCapacity), но проверяющий скрипт
			// и панель искали привычное имя и получали null — «числа не совпали».
			// Одно значение под одним именем на всех поверхностях дешевле, чем
			// объяснение, почему на одной поверхности имя другое.
			"effectiveMaxConcurrentRequests": maxConcurrent,
			"maxModels":                      maxModels,
			"runtimeMaxModels":               backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests":   backend.RuntimeMaxConcurrentRequests,
			// R117 (2026-10-14): ПОЧЕМУ вместимость такая. До этого отчёт давал
			// только число, и «2» одинаково выглядело как «у модели два слота», как
			// «нода заявила n_parallel=2» и как «модели нет вовсе». Живой случай:
			// count=0 на узле, а отчёт показывал 2 — разобраться без чтения кода
			// было нельзя. Источник считает types.ResolveCapacity.
			"maxConcurrentRequestsSource": capacitySource,
			"runtimeModelSlots":           backend.RuntimeModelSlots,
			// R118: роль бэкенда в раскладке слоёв (standalone|main|donor) —
			// её выставляет оператор из WebUI (PUT .../role).
			"role": backend.Role,
			// R304 (2026-10-30): донор держит ТОЛЬКО память мастера и своих
			// копий моделей не поднимает. Панель не должна выводить это из строки
			// роли: нужно ещё ответить «чей это донор», «можно ли ему грузить» и
			// «держит ли он копию сейчас вопреки роли». Последнее — нарушение
			// инварианта, и оно обязано быть видно числами, а не догадкой по
			// соседним полям.
			"donor":            donorMark.Donor,
			"canLoadOwnModels": donorMark.CanHostOwnModels,
			"ownCopies":        donorMark.OwnCopies,
			"donorNote":        donorMark.Note,
			// R229 (2026-10-22): тумблер раскладки оператора: auto|on|off. В панели это
			// состояние тумблера, а не догадка по роли — иначе после «выкл» он выглядел
			// бы включённым, потому что роль главного осталась прежней.
			"rpcEnabledOverride": s.proxy.RPCEnabledOverrideMode(backend.ID),
			// R250 (2026-10-28): «не использовать бэкенд». Флаг отдаётся в СПИСКЕ,
			// а не только в ответе тумблера: панель рисует состояние из списка, и
			// без этих полей выключенный узел выглядел бы рабочим. disabledSince —
			// как давно выключен; пусто = включён.
			"disabled":       backend.Disabled,
			"disabledReason": backend.DisabledReason,
			"disabledSince":  disabledSinceRFC3339(backend.DisabledAt),
			// R118 (2026-10-14): замер пропускной способности канала до узлов
			// раскладки — из окружения балансера (LB_RPC_LINK_*), потому что
			// измерить его из процесса нельзя: число даёт оператор scripts/
			// measure-network.ps1. WebUI показывает его в пометках раскладки.
			// R118.15: rpcLink — замер ОПЕРАТОРА (LB_RPC_LINK_*, как было; смысл
			// не подменяем), rpcLinkAuto — автозамер балансера до узла (см. тип
			// rpcLinkAuto: почему это ДВА разных числа, а не одно).
			"rpcLink":     rpcLinkInfo(),
			"rpcLinkAuto": s.rpcLinkAutoInfo(backend.ID),
			// R118.2 (2026-10-14): состояние раскладки у ЭТОГО бэкенда: включена
			// ли она сейчас и не работает ли в неполном составе. Оператор видит
			// причину («узлы раскладки недоступны (…): раскладка выключена,
			// загрузка идёт локально») прямо в панели, а не только в логе.
			"rpcLayout": s.proxy.LayoutGuardStatus(backend.ID),
			// R133 (2026-10-16): состояние СОБСТВЕННОГО узла раскладки бэкенда —
			// то, что раньше было не видно вообще. Донор поднимает rpc-server по
			// приказу балансера (роль donor) и отчитывается из /api/models:
			// enabled (желание), running (процесс), listening (порт), reachable
			// (проверка с балансера). Без этого роль в панели выглядела как
			// команда без ответа, а «поднят» путалось с «доступен».
			"rpcNode": s.proxy.RPCNodeReport(backend.ID),
			// R132: откуда вклад узлов в бюджет у справочных путей
			// (none|cache|probed) — «0 МБ узлов» и «узлы не измерялись» в панели
			// выглядели одинаково.
			"vramRemoteSource": s.proxy.VramRemoteSourceFor(backend.ID),
			"labels":           backend.Labels,
			"status":           backend.Status,
			"type":             backend.Type,
			"engine":           backend.Engine,
			// Round 51.2 (2026-08-20): явный API-стиль бэкенда. Если пусто — клиент
			// (WebUI) видит только effective-значение из Type. Возвращаем ОБА:
			// apiStyle (raw, оператор-установленный) + effectiveApiStyle (resolved).
			"apiStyle":          backend.ApiStyle,
			"effectiveApiStyle": backend.EffectiveAPIStyle(),
			// Round 13 (2026-07-10): expose agent attachment info для WebUI.
			"hasAgent":         backend.HasAgent,
			"agentId":          backend.AgentID,
			"lastAgentContact": backend.LastAgentContact,
			// R101 (S1 плана 2026-10-07): версия сборки llama.cpp, которую
			// сообщил узел. Оператор видит, из чего собраны узлы раскладки:
			// расхождение — причина таймаута загрузки, а не «медленная сеть».
			// Поле отдаётся всегда (пустая строка = узел версию не сообщает).
			"buildInfo": backend.BuildInfo,
			// R486 (2026-11-10): с какого РЕЛИЗА собран узел. Раньше балансер
			// знал только коммит llama.cpp; «узел на другом образе» не видел
			// никто, и рассинхрон стенда искали глазами оператора.
			"workerVersion":         backend.WorkerVersion,
			"workerCommit":          backend.WorkerCommit,
			"workerBuildDate":       backend.WorkerBuildDate,
			"workerRpcProto":        backend.WorkerRPCProto,
			"workerReleaseMismatch": workerReleaseMismatchR486(&backend),
			// R493-12i (2026-11-12): идентичность АГЕНТА отдельно и источник
			// worker*-полей. Раньше агент заполнял worker*-поля своей версией, и
			// в bundled-стеке «workerCommit» показывал коммит агента — при
			// проверке выкладки это читалось как «раскатали не тот образ».
			"agentVersion":         backend.AgentVersion,
			"agentCommit":          backend.AgentCommit,
			"workerIdentitySource": backend.WorkerIdentitySource,
			// R269 (2026-10-29): свежесть сборки относительно стабильного
			// upstream llama.cpp. Раньше оператор видел только строку версии и
			// не знал, есть ли новее; relation=unknown — это ЧЕСТНОЕ «сравнить
			// не с чем», а не «устарело».
			"llamaBuild": s.proxy.LlamaBuildStatusFor(backend.ID),
		}

		// R-Image follow-up (2026-10-02): gpuIndex — ЧИСЛО, когда индекс задан
		// явно (0 — валидная ПЕРВАЯ карта), и поля НЕТ, когда индекс неизвестен
		// (nil). Разницу обязан видеть потребитель: от неё зависит ключ лока
		// сосуществования image/text (см. types.Backend.GPUIndex).
		if backend.GPUIndex != nil {
			backendData["gpuIndex"] = *backend.GPUIndex
		}

		// R304: у донора всегда называем мастера — и когда он назначен, и когда
		// нет (тогда поля пустые). Панель не должна выводить это поиском по
		// списку: список уже отфильтрован (offline-узлы с ролью показываются, а
		// обычные — нет), и «мастер не в списке» и «мастера нет» стали бы
		// неотличимы.
		if donorMark.Donor {
			backendData["donorFor"] = map[string]interface{}{
				"backendId": donorMark.MasterBackendID,
				"host":      donorMark.MasterHost,
			}
		}

		// Добавление метрик если они доступны
		if metrics, ok := metricsMap[backend.ID]; ok {
			backendData["timestamp"] = metrics.Timestamp
			backendData["gpu"] = metrics.GPU
			backendData["system"] = metrics.System
			backendData["prediction"] = metrics.Prediction
			// Phase 2 (2026-07-06): fallback для llama_cpp бэкендов. Без NVML в
			// agent metrics.GPU пуст (MemoryTotal=0, MemoryUsed=0).
			//
			// R346 (2026-10-30): источник — TotalVRAMMB/AvailableVRAMMB поллера.
			// Раньше здесь читался LlamaCppMetrics.GPUInfo, а его НЕ ЗАПОЛНЯЕТ ни один
			// путь балансера: поле `gpuInfo` в JSON не появлялось никогда, поэтому
			// фолбэк был мёртвым кодом. Симптом: в bundled-режиме (воркер без агентской
			// NVML) монитор показывал 0 VRAM, хотя поллер каждый тик писал
			// total_vram_mb/available_vram_mb — числа были видны только в сыром JSON.
			//
			// Semantics: available_vram_mb у воркера = свободная VRAM (VRAMFreeMB),
			// total_vram_mb = вся VRAM карты (VRAMTotalMB), обе в мегабайтах — как и
			// GPUMetrics.MemoryTotal/MemoryUsed/MemoryFree (см. mapLlamaGPUMetrics).
			if backend.Type == types.BackendTypeLlamaCpp && metrics.GPU.MemoryTotal == 0 {
				// R245: снапшот под RLock — поллер переписывает поля под Lock.
				lm, _ := s.proxy.GetMetricsManager().SnapshotLlamaCppMetrics(backend.ID)
				if lm != nil && lm.TotalVRAMMB > 0 {
					freeMB := lm.AvailableVRAMMB
					if freeMB > lm.TotalVRAMMB {
						freeMB = lm.TotalVRAMMB
					}
					usedMB := lm.TotalVRAMMB - freeMB
					util := float64(usedMB) * 100.0 / float64(lm.TotalVRAMMB)
					backendData["gpu"] = map[string]interface{}{
						"usagePercent": util,
						"memoryTotal":  lm.TotalVRAMMB,
						"memoryUsed":   usedMB,
						"memoryFree":   freeMB,
						"temperature":  0,
						"powerUsage":   0,
						"powerLimit":   0,
						"gpuClock":     0,
						"memClock":     0,
					}
				}
			}

			// === Шаг «отображение загрузки в мониторе и вкладке бэкендов» ===
			// Если бэкенд — llama_cpp, передаём loadingModels и loadedModels напрямую
			// из LlamaCppMetrics (UI использует это для отображения спиннера и
			// elapsed-времени loading, а loadedModels — для отображения текущего
			// состояния загруженных моделей в WebUI/Monitor).
			//
			// Round 51.6.1 (2026-08-20): добавлен loadedModels + loadedModelCount.
			// До этого фикса API возвращал ТОЛЬКО loadingModels, и UI никогда
			// не видел загруженные модели (хотя callback UpdateLlamaCppModelLoaded
			// обновлял lm.LoadedModels в памяти при успешной загрузке).
			// Симптом для пользователя: "гонка за отображение состояния загруженной
			// модели — информация не обновляется даже через 30s polling".
			// Реальная причина: loadedModels в response отсутствовало.
			if backend.Type == types.BackendTypeLlamaCpp {
				// R119 (пункт 4): берём КОПИЮ метрик с приписанным происхождением
				// параметров загрузки — WebUI показывает источник рядом со
				// значением (запрос / профиль / настройки по умолчанию / cppworker).
				// GetLlamaCppMetrics отдаёт живой указатель, дописывать в него нельзя.
				if lm := s.proxy.LlamaCppMetricsWithProvenance(backend.ID); lm != nil {
					loading := lm.LoadingModels
					if loading == nil {
						loading = []types.LlamaCppModel{}
					}
					backendData["loadingModels"] = loading
					backendData["loadingModelCount"] = len(loading)

					loaded := lm.LoadedModels
					if loaded == nil {
						loaded = []types.LlamaCppModel{}
					}
					backendData["loadedModels"] = loaded
					backendData["loadedModelCount"] = len(loaded)

					// Round 54.1 (2026-08-24): AutoTune report — рекомендации
					// по оптимизации загруженных моделей (KV cache, num_ctx, layers).
					// WebUI может показать badge "sub-optimal" и предложить fix.
					// R230: метрики GPU/System приходят в МБ, AnalyzeBackend ждёт
					// байты — конвертируем (иначе freeVRAM=20000 → 0.0 GB).
					var freeVRAM, freeRAM, totalVRAM uint64
					if m := metricsMap[backend.ID]; m != nil {
						totalVRAM = balancer.MBToBytes(m.GPU.MemoryTotal)
						if m.GPU.MemoryFree > 0 {
							freeVRAM = balancer.MBToBytes(m.GPU.MemoryFree)
						} else if m.GPU.MemoryTotal > 0 {
							// estimate: total - used
							freeVRAM = balancer.MBToBytes(m.GPU.MemoryTotal)
							if used := balancer.MBToBytes(m.GPU.MemoryUsed); used < freeVRAM {
								freeVRAM -= used
							}
						}
						if m.System.MemoryFree > 0 {
							freeRAM = balancer.MBToBytes(m.System.MemoryFree)
						}
					}
					autoTuneReport := balancer.AnalyzeBackend(
						s.proxy, backend.ID, string(backend.Type), loaded, freeVRAM, freeRAM, totalVRAM)
					backendData["autoTune"] = autoTuneReport

					// R594 (2026-11-19): «тень» резерва VRAM в карточке бэкенда.
					//
					// ЗАЧЕМ. Оператор видел в ошибке декода одно число (max_vram_n_ctx
					// без резерва — его считает мост), а балансер принимал решения по
					// другому (с резервом CPPWORKER_VRAM_OVERHEAD_MB). Живой факт
					// 27B/A10 (2026-11-19): окно 201216 при безрезервном потолке 251254
					// оставило 414 MB свободной VRAM из 22 502, и декод промпта 33 812
					// токенов упал с `code 1` при `compute buffer = 316.75 MiB`.
					//
					// Решение по окну НЕ меняется (решение владельца: сначала показать):
					// оба числа уходят в ответ /api/v1/backends, рисует их карточка.
					// Отдаём только когда хоть одно измерено — нули как «факт» не выдаём.
					if lm.MaxVRAMNCtx > 0 || lm.MaxVRAMNCtxUnreserved > 0 || lm.VRAMReserveMB > 0 {
						backendData["vramWindow"] = map[string]interface{}{
							"maxVramNCtx":           lm.MaxVRAMNCtx,
							"maxVramNCtxUnreserved": lm.MaxVRAMNCtxUnreserved,
							"reserveMB":             lm.VRAMReserveMB,
						}
					}
				} else {
					backendData["loadingModels"] = []types.LlamaCppModel{}
					backendData["loadingModelCount"] = 0
					backendData["loadedModels"] = []types.LlamaCppModel{}
					backendData["loadedModelCount"] = 0
				}
			}

			ollamaMetrics := metrics.Ollama

			// FreeSlots вычисляем на основе лимитов
			if ollamaMetrics.MaxConcurrentRequests > 0 {
				freeSlots := ollamaMetrics.MaxConcurrentRequests - ollamaMetrics.ActiveRequests
				if freeSlots < 0 {
					freeSlots = 0
				}
				ollamaMetrics.FreeSlots = freeSlots
			}

			backendData["ollama"] = ollamaMetrics
		} else {
			// Пустые метрики если данные недоступны
			backendData["timestamp"] = time.Time{}
			backendData["gpu"] = types.GPUMetrics{}
			backendData["system"] = types.SystemMetrics{}
			backendData["ollama"] = types.OllamaMetrics{}
		}

		// Флаг наличия активного агента
		backendData["hasAgent"] = backend.HasAgent
		backendData["lastAgentContact"] = backend.LastAgentContact

		backends = append(backends, backendData)
	}

	resp := map[string]interface{}{
		"backends": backends,
		"total":    len(backends),
	}
	// R92 (S2 плана 2026-10-07): логические бэкенды отдаются отдельным списком.
	// Без него оператор видит два физических узла и не понимает, что они
	// обслуживают ОДНУ модель как один бэкенд (слоты, health и VRAM — общие).
	// Состояние считает internal/backendgroup, см. /api/v1/placement → groups.
	if groups := s.proxy.BackendGroupStatuses(); len(groups) > 0 {
		resp["groups"] = groups
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// getBackend - получение информации о бэкенде
func (s *Server) getBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()

	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, metrics)
			return
		}
	}

	// Если нет метрик, возвращаем конфигурацию бэкенда
	backend := s.proxy.GetBackend(backendID)
	if backend != nil {
		// R65e: НЕ отдаём секрет наружу. GetBackend возвращает живой указатель,
		// поэтому сначала делаем копию — мутировать оригинал нельзя (сломает
		// авторизацию балансера на cppworker). Раньше клиент получал
		// cppWorkerApiToken открытым текстом в теле ответа.
		safe := *backend
		safe.CppWorkerApiToken = ""
		s.writeJSON(w, http.StatusOK, &safe)
		return
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// addBackend - добавление бэкенда
// isSameBackendRegistration — R70: это повторная САМОРЕГИСТРАЦИЯ того же бэкенда?
//
// Признаки (оба обязательны):
//   - метка "auto-registered" (её ставит cppworker/agent при саморегистрации;
//     ручное создание бэкенда из WebUI такой метки не имеет — для него остаётся
//     409, чтобы случайный POST не перезаписал чужую запись);
//   - совпадение host и порта с уже зарегистрированным бэкендом.
//
// R-Image (2026-09-27): учитываем imagePort. У image-бэкенда (sdworker) НЕТ
// ни Ollama-, ни llama.cpp-поверхности, поэтому он физически не может заполнить
// CppWorkerPort/OllamaPort — без этой ветки повторная саморегистрация (после
// смены host/порта или рестарта) получала 409 и запись не обновлялась.
func (s *Server) isSameBackendRegistration(req backendRequest) bool {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		return false
	}
	if !hasLabelFold(req.Labels, "auto-registered") {
		return false
	}
	host := req.Host
	port := req.CppWorkerPort
	if port == 0 {
		port = req.OllamaPort
	}
	if port == 0 {
		port = req.ImagePort
	}
	if host == "" || port == 0 {
		return false
	}
	sameHost := host == existing.Host
	samePort := port == existing.CppWorkerPort || port == existing.OllamaPort ||
		port == existing.ImagePort || port == existing.EffectiveImagePort()
	return sameHost && samePort
}

// isReRegistrationOfAutoBackend — R-Image Phase 8 (2026-10-03): повторная
// саморегистрация с ИЗМЕНИВШИМСЯ адресом.
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ isSameBackendRegistration. Тот требует совпадения host+порта,
// поэтому пересозданный контейнер (новый host или порт — типичный случай в
// едином docker-стенде после `up -d --build`) залипал в 409 НАВСЕГДА: воркер жив
// и здоров, а в балансере остаётся старая запись с чужим адресом. Ровно это
// выглядит как «image-бэкенд не активен в докере».
//
// Условия — все обязательны, иначе POST из WebUI мог бы перезаписать чужую
// запись по совпадению ID:
//   - у СУЩЕСТВУЮЩЕЙ записи есть метка "auto-registered" (её ставит только нода,
//     ручное создание из WebUI — нет);
//   - в запросе тоже есть "auto-registered" (это саморегистрация, а не оператор);
//   - тип бэкенда в запросе непустой и совпадает с типом существующей записи.
func (s *Server) isReRegistrationOfAutoBackend(req backendRequest) bool {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		return false
	}
	if !hasLabelFold(existing.Labels, "auto-registered") || !hasLabelFold(req.Labels, "auto-registered") {
		return false
	}
	want := types.BackendType(req.BackendType)
	if want == "" {
		return false
	}
	return want == existing.Type
}

// hasLabelFold — есть ли метка (без учёта регистра).
func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// refreshRegisteredBackend — R70: обновить параметры повторно зарегистрированного
// бэкенда (идемпотентная саморегистрация cppworker'а после перезапуска).
//
// Обновляем только то, что сообщает сама нода: вместимость (maxConcurrentReqs —
// она равна реальному n_parallel), лимит моделей, тип движка, GPU-режим, вес,
// метки и токен для внутренних вызовов. Хост/порт — из той же проверки
// isSameBackendRegistration (иначе сюда бы не попали).
func (s *Server) refreshRegisteredBackend(w http.ResponseWriter, req backendRequest) {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		s.writeJSON(w, http.StatusConflict, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s disappeared during update", req.ID),
		})
		return
	}
	updated := *existing
	// Phase 8: АДРЕС тоже обновляем. Для isSameBackendRegistration он совпадает по
	// определению, а для повторной регистрации авто-бэкенда с новым адресом
	// (пересозданный контейнер) это единственный способ не залипнуть в 409:
	// иначе воркер жив, а балансер держит запись с чужим host/портом и зовёт
	// несуществующий адрес.
	if req.Host != "" {
		updated.Host = req.Host
	}
	if req.Name != "" {
		updated.Name = req.Name
	}
	if req.OllamaPort > 0 {
		updated.OllamaPort = req.OllamaPort
	}
	if req.CppWorkerPort > 0 {
		updated.CppWorkerPort = req.CppWorkerPort
	}
	if req.ImagePort > 0 {
		updated.ImagePort = req.ImagePort
	}
	if req.AgentPort > 0 {
		updated.AgentPort = req.AgentPort
	}
	if req.MaxConcurrentReqs > 0 {
		updated.MaxConcurrentReqs = req.MaxConcurrentReqs
		// R70: сбрасываем runtime-override — иначе он (значение, которое агент
		// получает обратно в ответе на heartbeat) продолжит перекрывать реальную
		// вместимость ноды, и admission-очередь останется на старом пороге.
		updated.RuntimeMaxConcurrentRequests = req.MaxConcurrentReqs
		// R70: помечаем, что вместимость пришла от ноды — heartbeat агента её
		// больше не перекрывает (см. handlers_agents.go).
		updated.RuntimeCapacityFromNode = true
	}
	if req.MaxModels > 0 {
		updated.MaxModels = req.MaxModels
	}
	if req.Weight > 0 {
		updated.Weight = req.Weight
	}
	if req.BackendType != "" {
		updated.Type = types.BackendType(req.BackendType)
	}
	if req.GPUMode != "" {
		updated.GPUMode = types.PlatformMode(req.GPUMode)
	}
	if len(req.Labels) > 0 {
		updated.Labels = req.Labels
	}
	if req.CppWorkerApiToken != "" {
		updated.CppWorkerApiToken = req.CppWorkerApiToken
	}
	// R101: версия сборки — только если узел её сообщил; пустое значение не стирает
	// уже известную (сборки, которые поля не знают, не должны обнулять проверку).
	if req.BuildInfo != "" {
		updated.BuildInfo = req.BuildInfo
	}
	// R486: релизная идентичность воркера — тем же правилом «не стирать
	// известное», иначе она исчезала бы на первом обновлении статуса.
	applyWorkerIdentityR486(&updated, req.WorkerVersion, req.WorkerCommit, req.WorkerBuildDate, req.WorkerRPCProto)
	// R493-12i: идентичность агента и источник worker*-полей.
	applyAgentIdentityR493(&updated, req.AgentVersion, req.AgentCommit, req.WorkerIdentitySource)
	// GPUIndex: нода сообщила индекс (в т.ч. 0 — явная первая карта) → принимаем;
	// ключа в payload нет → сохраняем операторское значение. Сборки, которые поля
	// не знают, не должны стирать настройку, иначе лок сосуществования молча
	// вернулся бы к хостовому.
	if req.GPUIndex != nil {
		updated.GPUIndex = types.GPUIndexPtr(*req.GPUIndex)
	}
	if err := s.proxy.UpdateBackend(req.ID, updated); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	logger.Get().Infow("backend re-registered: parameters refreshed (R70)",
		"backend", req.ID, "max_concurrent_requests", updated.MaxConcurrentReqs,
		"max_models", updated.MaxModels, "host", updated.Host)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"updated": true,
		"backend": updated,
	})
}

// buildInfoOrKeepR101 — версия сборки из запроса, иначе уже известная.
//
// Отдельная функция, потому что путь heartbeat пересобирает Backend целиком: без
// этого правила версия, полученная при регистрации, исчезала бы на первом же
// обновлении статуса — а вместе с ней и проверка совместимости сторон RPC.
func buildInfoOrKeepR101(fromRequest, existing string) string {
	if strings.TrimSpace(fromRequest) != "" {
		return fromRequest
	}
	return existing
}

// backendRequest — тело POST /api/v1/backends (создание/саморегистрация бэкенда).
// R70: вынесено из addBackend в именованный тип, чтобы логика идемпотентной
// повторной регистрации (isSameBackendRegistration / refreshRegisteredBackend)
// работала с тем же типом.
type backendRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	OllamaPort    int    `json:"ollamaPort"`
	AgentPort     int    `json:"agentPort"`
	CppWorkerPort int    `json:"cppWorkerPort"`
	// R-Image (2026-09-27): порт image-воркера для backendType=image_cpp
	// (default 18093). Fallback: CppWorkerPort → 18093 (EffectiveImagePort).
	ImagePort int `json:"imagePort"`
	// GPUIndex — индекс GPU бэкенда (nil = не задан/неизвестно; см.
	// types.Backend.GPUIndex). Влияет только на ключ лока сосуществования
	// image/text: при известном индексе у ОБЕИХ сторон лок берётся по
	// (host, gpu), иначе по хосту. УКАЗАТЕЛЬ, а не int: 0 — валидная первая
	// карта, и в int «явно GPU 0» неотличимо от «не прислано» (тогда POST от
	// cppworker'а на GPU 0 молча оставлял бы хостовый лок).
	GPUIndex          *int     `json:"gpuIndex"`
	Weight            int      `json:"weight"`
	MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
	MaxModels         int      `json:"maxModels"`
	GPUMode           string   `json:"gpuMode"`
	Labels            []string `json:"labels"`
	BackendType       string   `json:"backendType"`
	BackendEngine     string   `json:"backendEngine"`
	// Round 51.2 (2026-08-20): явный API-стиль бэкенда (ollama-native/openai-compatible).
	// Если пусто — Backend.EffectiveAPIStyle() выводит из Type.
	ApiStyle string `json:"apiStyle"`
	// BuildInfo — R101 (S1 плана 2026-10-07): версия/коммит сборки llama.cpp,
	// которую узел сообщает при регистрации. Балансер сравнивает её у узлов одной
	// раскладки (протокол RPC версионно-чувствителен) — см. types.Backend.BuildInfo.
	BuildInfo string `json:"buildInfo"`
	// R486: релизная идентичность образа воркера (см. types.Backend.WorkerCommit).
	// Пустые поля не стирают уже известные значения.
	WorkerVersion   string `json:"workerVersion"`
	WorkerCommit    string `json:"workerCommit"`
	WorkerBuildDate string `json:"workerBuildDate"`
	WorkerRPCProto  string `json:"workerRpcProto"`
	// R493-12i (2026-11-12): идентичность АГЕНТА отдельно от идентичности
	// ВОРКЕРА, плюс источник worker*-полей. Раньше агент клал в worker-поля свою
	// версию, и на стенде это читалось как «раскатали не тот образ».
	AgentVersion         string `json:"agentVersion"`
	AgentCommit          string `json:"agentCommit"`
	WorkerIdentitySource string `json:"workerIdentitySource"`
	// Round 7 (2026-07-09): cppworker пробрасывает свой API_TOKEN при
	// регистрации, чтобы balancer мог авторизоваться на /api/models/reload
	// (authMiddleware). В bundled-режиме устраняет необходимость ручной
	// настройки CppWorkerApiToken в конфиге.
	CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`
}
