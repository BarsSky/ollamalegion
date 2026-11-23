// proxy_services.go — R712: служебные методы Proxy вынесены из proxy.go.
//
// ЗАЧЕМ. proxy.go перешагнул порог «гиганта» (1500 непустых строк) и покраснел
// страж tests/structure_ratchet_r628_test.go: он рос в R679-R700. В файле жили три
// вещи: описание состояния (struct Proxy), путь запроса (ServeHTTP) и служебные
// методы, к пути запроса не относящиеся. Здесь — третий слой: 1554 -> 1311
// непустых строк в proxy.go, 271 здесь (с этим заголовком).
//
// ГРАНИЦА. Сюда вынесено то, что НЕ обслуживает конкретный запрос: зависимости и
// сеттеры (EventBus, GetLlamaCppRouter, SetHealthChecker), метрики (UpdateMetrics,
// ClearAgentMetrics), жизненный цикл (Shutdown) и две чистые проверки класса
// (HasAmbiguousHost, determineRequestBackendType).
//
// ЧТО ОСТАЛОСЬ В proxy.go ОСОЗНАННО. struct Proxy и ServeHTTP. ServeHTTP — не
// «крупная функция для порядка», а разделяемая точка входа: его читают стюарды
// internal/balancer/*_test.go (R301/R340/R493/R659/R696) по ИМЕНИ ФАЙЛА proxy.go.
// Вынос в отдельный файл сломал бы их не по поведению, а по адресу — а один из них
// (R340) негативный: он молча стал бы проверять пустоту. Поэтому ServeHTTP остаётся
// на месте; его собственный разрез (937 непустых строк — долг §15c) — отдельная
// работа на горячем пути, а не перекладывание строк между файлами.
//
// ЗАОДНО УБРАН МУСОР. Над `determineRequestBackendType` в proxy.go лежал осиротевший
// комментарий `// NewProxy - создание нового прокси` — наследство R629, когда
// конструктор уехал в proxy_new.go, а его однострочный комментарий остался. После
// выноса методов он встал бы прямо над ServeHTTP и читался как его описание, поэтому
// удалён (у NewProxy есть свой заголовок в proxy_new.go).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть, построчная сверка удалённого и
// перенесённого совпадает, гейты пакета зелёные.
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// EventBus — геттер для EventBus (используется API-сервером для F.α SSE endpoint).
// Не nil после NewProxy, всегда возвращает валидный EventBus.
func (p *Proxy) EventBus() *EventBus {
	return p.eventBus
}

// GetLlamaCppRouter — R60.11 (2026-09-07): expose LlamaCppRouter
// для admin endpoint /api/v1/balancer/load-backoff (нужен доступ к loadBackoff).
func (p *Proxy) GetLlamaCppRouter() *LlamaCppRouter {
	if p == nil {
		return nil
	}
	return p.llamaCppRouter
}

// ClearAgentMetrics — R-MultiHost (2026-10-07): забыть метрики, присланные
// агентом бэкенда.
//
// Вызывается, когда агент замолчал (agent_manager.go). Без этого последние
// присланные GPU/CPU/VRAM оставались в MetricsManager навсегда и отдавались в
// /api/v1/backends как текущие: выключенная машина продолжала «показывать»
// 52 °C и 405 MHz — числа последнего опроса, выданные за живые. После удаления
// GetClusterState собирает ответ без них (честный ноль, UI рисует прочерк).
func (p *Proxy) ClearAgentMetrics(backendID string) {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return
	}
	p.metricsMgr.ClearBackendMetrics(backendID)
}

// HasAmbiguousHost — R-MultiHost (2026-10-07): у ЭТОГО бэкенда такой же Host, как
// у другого бэкенда, принадлежащего ДРУГОМУ узлу.
//
// Зачем это health-check'у. URL бэкенда строится из Host, а имя контейнера внутри
// docker-сети балансера разрешается в ЕГО СОБСТВЕННЫЙ контейнер. Значит в такой
// паре хотя бы один бэкенд заведомо проверяется не там, где находится, — и
// проверка будет успешной даже когда его машина выключена. Наблюдалось на живой
// паре: после выключения 192.0.2.11 запись IMAGEWORKER-34 (host «imageworker»)
// оставалась healthy по health-check'у ЛОКАЛЬНОГО контейнера, хотя её
// собственный агент замолчал, и в UI висели её замороженные метрики.
func (p *Proxy) HasAmbiguousHost(backendID string) bool {
	if p == nil {
		return false
	}
	target := p.GetBackend(backendID)
	if target == nil || target.Host == "" || target.NodeAddr == "" {
		return false
	}
	for _, other := range p.GetAllBackends() {
		if other.ID == backendID || other.Host != target.Host {
			continue
		}
		if other.NodeAddr != "" && other.NodeAddr != target.NodeAddr {
			return true
		}
	}
	return false
}

// determineRequestBackendType определяет тип бэкенда на основе пути входящего запроса.
//   - Ollama API endpoints (/api/generate, /api/chat, /api/tags, ...) →
//     если режим разрешает оба текстовых типа — возвращает "", позволяя
//     selectBackend выбрать бэкенд ЛЮБОГО типа, где загружена модель (это и есть
//     «Ollama API работает одинаково для всех клиентов», независимо от того,
//     обслуживает запрос cppworker или Ollama-нода).
//   - llama.cpp / OpenAI-compatible endpoints (/v1/chat/completions, ...) → BackendTypeLlamaCpp
//   - Прочие пути → "" (будет определено из OperatingMode через getDefaultAllowedTypes)
func (p *Proxy) determineRequestBackendType(r *http.Request) types.BackendType {
	path := r.URL.Path

	// R-Image (2026-09-27): явные признаки image-запроса (endpoint или
	// префикс имени модели) имеют приоритет над всеми остальными правилами и
	// действуют на ОБЕИХ поверхностях:
	//   - /v1/images/*, /sdapi/v1/*, /api/image/*  → image-бэкенд;
	//   - model: "sd:...", "image:...", "img/..."  → image-бэкенд.
	// Никакой классификации по тексту промпта — только явные признаки.
	// Разделение обеспечивается фильтром типов в selectBackend/expandCandidates:
	// текстовый запрос не может уйти на image-бэкенд и наоборот.
	if isImageEndpointPath(path) {
		return types.BackendTypeImage
	}
	if model := p.requestModelHint(r); hasImageModelPrefix(model) {
		return types.BackendTypeImage
	}

	if strings.HasPrefix(path, "/api/") {
		// Проверяем, какие типы бэкендов разрешены текущим режимом.
		//
		// R112 (2026-10-11): ЗДЕСЬ БЫЛО НАВЯЗЫВАНИЕ ТИПА ПО КОНФИГУ. Если в
		// config.json лежал явный backendEngine (а он выводился из operatingMode
		// при инициализации и сохранялся), /api/* жёстко уезжал на бэкенд только
		// этого типа — второй тип не получал запросов вообще, и клиент видел
		// «модель не найдена» при живой модели на соседнем бэкенде другого типа.
		// Теперь тип определяет selectBackend по фактическому наличию модели.
		allowed := p.getDefaultAllowedTypes()
		if len(allowed) > 1 {
			// Разрешено больше одного типа — "", чтобы selectBackend выбрал
			// бэкенд любого из них по модели/ресурсам.
			return ""
		}
		if len(allowed) == 1 {
			return allowed[0]
		}
		// Фолбэк: неизвестный режим — только Ollama для обратной совместимости
		return types.BackendTypeOllama
	}
	if strings.HasPrefix(path, "/v1/") {
		// OpenAI-поверхность обслуживается ДИСПЕТЧЕРОМ balancer'а
		// (llamaCppRouter: handleOpenAIChatCompletions/handleOpenAICompletion),
		// который выбирает бэкенд сам. Тип остаётся llama_cpp: это не «деление
		// стенда по типу», а признак того, каким кодом обслуживается путь.
		// R112: Ollama-поверхность (/api/*) от типа не зависит вовсе.
		return types.BackendTypeLlamaCpp
	}
	return ""
}

// SetHealthChecker — устанавливает HealthChecker после создания Proxy
// (избегаем circular dep Proxy ↔ HealthChecker).
// Round 8 (2026-07-10): нужен для MarkUnhealthy при persistent connection failures.
func (p *Proxy) SetHealthChecker(hc *HealthChecker) {
	p.healthChecker = hc
}

// Shutdown — graceful shutdown с завершением активных SSE-сессий и сохранением состояния.
//
// Порядок остановки (R52.5, 2026-08-24: расширен per-component Stop):
//  1. Запрет новых запросов (возврат 503) — p.shuttingDown
//  2. Остановка приёма очереди — QueueManager.Stop
//  3. Остановка background controllers:
//     - UnloadScheduler (auto-unload idle models)
//     - AgentTimeoutChecker (agent health)
//     - SessionManager (session stickiness)
//     - LlamaCppMetricsPoller (cppworker metrics poll)
//     - GroupController (replication group) — R52.5 NEW
//     - NCtxReloadCoordinator (n_ctx async reload) — R52.5 NEW
//     - EventBus (subscriber channels) — R52.5 NEW
//  4. Завершение активных SSE-сессий с таймаутом
//  5. Сохранение состояния
func (p *Proxy) Shutdown(ctx context.Context) error {
	logger.Get().Infow("shutdown initiated, stopping new requests")

	// 1. Запрещаем приём новых запросов
	p.shuttingDown.Store(true)

	// 2. Останавливаем QueueManager (workers завершат текущие задачи, новые не принимаются)
	if p.queueMgr != nil {
		logger.Get().Infow("stopping queue manager...")
		p.queueMgr.Stop()
	}

	// 3. Останавливаем background controllers
	if p.unloadScheduler != nil {
		logger.Get().Infow("stopping unload scheduler...")
		p.unloadScheduler.Stop()
	}
	if p.agentChecker != nil {
		logger.Get().Infow("stopping agent timeout checker...")
		p.StopAgentTimeoutChecker()
	}

	// R118.15: останавливаем петлю замеров канала раскладки.
	if p.linkMeasureStop != nil {
		logger.Get().Infow("stopping rpc link measurer...")
		close(p.linkMeasureStop)
		p.linkMeasureStop = nil
	}
	if p.sessionMgr != nil {
		logger.Get().Infow("stopping session manager...")
		p.sessionMgr.Stop()
	}
	if p.llamaCppMetricsPoller != nil {
		logger.Get().Infow("stopping llama.cpp metrics poller...")
		p.llamaCppMetricsPoller.Stop()
	}
	// R269: останавливаем петлю опроса версии llama.cpp (иначе goroutine-leak
	// при перезапуске/в тестах).
	logger.Get().Infow("stopping llama upstream version tracker...")
	p.stopLlamaUpstreamTracking()
	// R52.5 (2026-08-24): per-component Stop for graceful shutdown.
	// Pre-R52.5 эти контроллеры НЕ останавливались → goroutine-leak при shutdown.
	// Покрытие: replicationCtrl (modelreplication.GroupController),
	// nctxReload (n_ctx async reload), eventBus (subscriber channels).
	if p.replicationCtrl != nil {
		logger.Get().Infow("stopping replication controller...")
		if err := p.replicationCtrl.Stop(); err != nil {
			logger.Get().Warnw("replication controller stop error", "error", err)
		}
	}
	if p.nctxReload != nil {
		logger.Get().Infow("stopping nctx reload coordinator...")
		p.nctxReload.Shutdown()
	}
	if p.eventBus != nil {
		logger.Get().Infow("stopping event bus...")
		p.eventBus.Stop()
	}

	// 4. Ожидаем завершения активных SSE-сессий с таймаутом
	logger.Get().Infow("waiting for active streams to finish...")
	streamsDone := make(chan struct{})
	go func() {
		p.activeStreams.Wait()
		close(streamsDone)
	}()

	select {
	case <-streamsDone:
		logger.Get().Infow("all active streams completed")
	case <-ctx.Done():
		logger.Get().Warnw("shutdown deadline exceeded, forcing exit",
			"active_streams_remaining", "unknown")
		// Принудительно не обрываем — Go закроет соединения при выходе процесса
	}

	// 5. Сохраняем состояние
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to save state during shutdown", "error", err)
		return fmt.Errorf("failed to save state: %w", err)
	}

	logger.Get().Infow("shutdown completed successfully")
	return nil
}

// UpdateMetrics - обновление метрик бэкенда с прогнозированием
func (p *Proxy) UpdateMetrics(backendID string, metrics *types.BackendMetrics) {
	// Round 15 (2026-07-10): нормализуем metrics.ID = backendID. Агент может
	// отправить metrics с ID=agentID (старый handler использовал agentID
	// как ключ). После dedup attach бэкенд имеет другой ID — нормализуем
	// чтобы listBackends нашёл metricsMap[backend.ID].
	metrics.ID = backendID

	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()

	if exists {
		state.mu.Lock()
		proxyActiveReqs := state.ActiveReqs
		state.mu.Unlock()

		if metrics.Ollama.ActiveRequests == 0 && proxyActiveReqs > 0 {
			metrics.Ollama.ActiveRequests = proxyActiveReqs
		}

		freeSlots := metrics.Ollama.MaxConcurrentRequests - metrics.Ollama.ActiveRequests
		if freeSlots < 0 {
			freeSlots = 0
		}
		metrics.Ollama.FreeSlots = freeSlots

		p.predictor.UpdateHistory(state, metrics)
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[backendID] = metrics
	// Синхронизируем llamaMetrics, чтобы GetClusterState корректно отображал
	// загруженные модели llama.cpp бэкендов, установленные через UpdateMetrics.
	if metrics != nil && len(metrics.LlamaCpp.LoadedModels) > 0 {
		p.metricsMgr.llamaMetrics[backendID] = &metrics.LlamaCpp
	}
	p.metricsMgr.mu.Unlock()
}
