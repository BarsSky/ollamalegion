package balancer

// cancel_client_r229.go — R229 (2026-10-22): отмена генерации, инициированная
// КЛИЕНТОМ (кнопка Stop в OpenWebUI, Cline, скрипт), а не разрывом соединения.
//
// ЖИВОЙ ДЕФЕКТ (замер на стенде ДО правки). Клиент отправил длинный стрим и через
// 2 секунды — POST /api/cancel с именем модели. Балансер НЕ ответил (таймаут 15 с),
// а стрим продолжился до конца: 380 чанков, 62 с. То есть отмена клиента не
// доходила до воркера вовсе: балансер пытался маршрутизировать /api/cancel как
// обычную генерацию (выбор бэкенда, автозагрузка, ожидание слота), вместо того
// чтобы просто приказать воркеру прекратить работу.
//
// У воркера штатная отмена — POST /api/cancel {request_id|user_id|model}; R184 уже
// отправлял её при РАЗРЫВЕ клиента. Теперь обрабатывается и ЯВНАЯ отмена.
//
// Ответ клиенту быстрый и честный: сколько бэкендов приняли приказ. Если отменять
// нечего — 200 {"status":"no_active_generation", ...} (Ollama на пустом списке
// отвечает 200/404 в зависимости от версии; клиенты воспринимают это как успех).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// clientCancelTimeout — сколько ждём ответа одного воркера на отмену. Отмена —
// служебный приказ: воркер может быть занят самомй генерацией, но приказ уже в
// сокете; ждать дольше незачем (клиенту нужен быстрый ответ).
const clientCancelTimeout = 2 * time.Second

// clientCancelBody — тело /api/cancel. Все поля необязательные: клиенты присылают
// разные наборы (OpenWebUI — model, Cline — request_id, скрипты — пустое тело).
type clientCancelBody struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
	// R363 (2026-10-31, аудит abort A4): прервать и ИДУЩУЮ ЗАГРУЗКУ модели.
	//
	// Ставится балансером ТОЛЬКО на явную клиентскую отмену (этот обработчик —
	// клиентский POST /api/cancel). Сторожевые обрывы (таймаут стрима, тишина upstream)
	// идут другим путём (cancelUpstreamGeneration) и загрузку НЕ трогают: таймаут одного
	// клиента не должен обрывать загрузку, которую ждут другие.
	// УКАЗАТЕЛЬ, а не bool: «поле не прислано» и «прислано false» — разные вещи.
	// Не прислано → для клиентской отмены считаем true (Stop должен прерывать и загрузку).
	// Прислано false → клиент явно не хочет трогать загрузку (например, отменяет только
	// свою генерацию, пока модель грузится для другого клиента).
	CancelLoad *bool `json:"cancel_load"`
}

// isClientCancelPath — путь явной отмены на Ollama-поверхности. OpenAI-клиенты
// отменяют разрывом соединения (это R184, отдельный механизм), поэтому здесь
// только нативный путь.
func isClientCancelPath(path string) bool {
	return path == "/api/cancel" || path == "/api/cancel/"
}

// handleClientCancel — обработать POST /api/cancel от клиента.
func (p *Proxy) handleClientCancel(w http.ResponseWriter, r *http.Request) {
	if p == nil {
		writeClientCancelError(w, http.StatusServiceUnavailable, "balancer is not ready")
		return
	}
	if r.Method != http.MethodPost {
		writeClientCancelError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req clientCancelBody
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(bytes.TrimSpace(body)) > 0 {
		// Терпимое декодирование: лишние поля от клиентов не должны давать 400.
		_ = json.Unmarshal(body, &req)
	}
	req.Model = trimSpaceOnly(req.Model)
	// R363: это КЛИЕНТСКАЯ отмена (кнопка Stop) — по умолчанию разрешаем прерывать и
	// загрузку модели; явный cancel_load=false уважаем.
	if req.CancelLoad == nil {
		allowLoadCancel := true
		req.CancelLoad = &allowLoadCancel
	}

	// R487 (2026-11-11): СНАЧАЛА адресная отмена — по request_id, затем по явной
	// личности клиента. Только если клиент не сказал ни того, ни другого, идёт
	// прежняя широковещательная рассылка по модели (совместимость с OpenWebUI,
	// который присылает одно поле model).
	//
	// ЖИВОЙ ДЕФЕКТ, ради которого это здесь: снятие запроса, стоявшего В ОЧЕРЕДИ,
	// оставляло его висеть, а приказ по модели убивал генерацию, которая в этот
	// момент РАБОТАЛА на занятом бэкенде (и доезжал до cppworker).
	identity := cancelIdentityR487(r.Header, req.UserID)
	if scoped, handled := p.cancelByScopeR487(req, identity); handled {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, scoped)
		return
	}

	targets := p.cancelTargets(req.Model)
	accepted := make([]string, 0, len(targets))
	results := make([]map[string]interface{}, 0, len(targets))
	for _, backendID := range targets {
		ok, detail := p.sendCancelToBackend(backendID, req)
		row := map[string]interface{}{"backend": backendID, "ok": ok}
		if detail != nil {
			// Воркер сообщает, сколько генераций реально отменено и почему не вышло
			// (R229: status/no_active_generation + подсказка про порт 50052).
			for k, v := range detail {
				row[k] = v
			}
		}
		results = append(results, row)
		if ok {
			accepted = append(accepted, backendID)
		}
	}

	// R343 (2026-10-30, аудит docs/plans/2026-10-30-abort-and-metrics-audit.md):
	// СТАТУС СЧИТАЕТСЯ ПО ФАКТУ ОТМЕНЫ, А НЕ ПО HTTP-КОДУ.
	//
	// Живой дефект: успехом считался любой 2xx, а воркер на пустом трекере
	// отвечает 200 + cancelled:0. Верхний status печатал "cancelled" и тогда,
	// когда генерация продолжалась. Плюс имя модели сверялось на двух концах
	// по-разному (балансер — мягко, воркер — точно), поэтому "доставлено" не
	// означало "отменено".
	status, hint := cancelStatusR343(results, accepted, len(targets))
	if logger.Get() != nil {
		logger.Get().Infow(i18n.T("balancer.cancel_client.broadcast_info"),
			"model", req.Model, "request_id", req.RequestID, "user_id", req.UserID,
			"targets", targets, "accepted", accepted, "status", status)
	}
	w.Header().Set("Content-Type", "application/json")
	responseBody := map[string]interface{}{
		"status": status,
		"model":  req.Model,
		// R487: адресного признака не было ни в request_id, ни в личности клиента —
		// значит это широковещательная отмена по модели. Говорим это прямо, чтобы
		// «Stop убил не ту сессию» больше не искали в логах воркера.
		"scope": "model_broadcast",
		"hint_scope": "клиент не прислал ни request_id, ни X-User-Id/X-Client-ID: " +
			"отмена разослана по всем бэкендам с этой моделью (R487)",
		"backends": accepted,
		// results — детали по каждому воркеру: сколько отменено и статус. Клиенту
		// достаточно "cancelled", оператору нужен ответ «почему не остановилось».
		"results": results,
	}
	if hint != "" {
		responseBody["hint"] = hint
	}
	writeJSON(w, http.StatusOK, responseBody)
}

// cancelStatusR343 — честный статус отмены по фактическим результатам воркеров.
//
// Возвращает (status, hint):
//   - "cancelled"          — хотя бы одна генерация реально отменена;
//   - "not_cancelled"      — воркеры ответили, но не отменили ничего (нет активной
//     генерации либо имя модели не совпало точно) — с подсказкой, что проверить;
//   - "no_targets"         — отменять было некому (нет healthy llama.cpp-бэкендов);
//   - "unreachable"        — цели были, но ни один воркер не принял приказ.
//
// Считаем по сумме cancelled в ответах воркеров: HTTP 200 + cancelled:0 — это
// НЕ отмена (живой дефект, ради которого функция появилась).
func cancelStatusR343(results []map[string]interface{}, accepted []string, targetCount int) (status, hint string) {
	totalCancelled := 0
	for _, row := range results {
		switch v := row["cancelled"].(type) {
		case float64:
			totalCancelled += int(v)
		case int:
			totalCancelled += v
		}
	}
	// R363 (2026-10-31, аудит abort A4): отмена ЗАГРУЗКИ модели — это тоже успех.
	// Оператор решил: «загрузку прерывать при отмене». Воркер отвечает
	// load_cancelled (остановлена) или load_abort_requested (флаг выставлен, ещё
	// останавливается); второй случай честно не выдаём за «готово», но и не считаем
	// провалом — флаг проверяется раз в тензор.
	loadCancelled := false
	loadAbortPending := false
	for _, row := range results {
		switch s, _ := row["status"].(string); s {
		case "load_cancelled":
			loadCancelled = true
		case "load_abort_requested":
			loadAbortPending = true
		}
	}
	if loadCancelled {
		return "load_cancelled", "загрузка модели прервана по отмене клиента; " +
			"частично прочитанные веса освобождены"
	}
	if loadAbortPending && totalCancelled == 0 {
		return "load_abort_requested", "флаг отмены загрузки выставлен, но C-мост ещё не " +
			"остановил чтение весов (проверка идёт на каждом тензоре); повторите через несколько секунд"
	}

	// R362 (2026-10-31, аудит A6): сборка воркера БЕЗ C-bridge (stub) физически не умеет
	// отменять генерацию, но раньше отвечала «cancelled: N» — и балансер с клиентом
	// считали, что Stop сработал. Такой воркер честно сообщает supported=false; верхний
	// статус обязан это отразить, иначе мы просто перекладываем ложь на другой уровень.
	notSupported := 0
	for _, row := range results {
		if v, ok := row["supported"].(bool); ok && !v {
			notSupported++
		}
	}
	if notSupported > 0 && totalCancelled == 0 {
		return "not_supported", "воркер собран без C-bridge (stub): отмена генерации не поддерживается, " +
			"модель остановится сама по исчерпанию n_predict; сообщение supported=false в results"
	}

	switch {
	case totalCancelled > 0:
		return "cancelled", ""
	case targetCount == 0:
		// ВАЖНО: значение "no_active_generation" оставлено прежним — его уже
		// проверяет TestR229_ClientCancelNoBackendsIsNotTimeout и на него могут
		// смотреть клиенты. Менялось только то, что раньше ВРАЛО ("cancelled"
		// при cancelled:0), а не словарь статусов.
		return "no_active_generation", "на кластере нет доступных llama.cpp-бэкендов — отменять некому"
	case len(accepted) == 0:
		return "unreachable", "ни один воркер не принял приказ об отмене (сеть/токен/адрес); смотрите results[].ok"
	default:
		return "not_cancelled", "воркеры приняли приказ, но не отменили ни одной генерации: " +
			"либо активных генераций нет, либо имя модели не совпало с именем загруженной модели " +
			"(балансер сверяет мягко, воркер — точно)"
	}
}

// cancelTargets — кого просить прекратить генерацию.
//
//   - указана модель → бэкенды, где она ЗАГРУЖЕНА (по метрикам), иначе все
//     healthy llama.cpp: воркер сам скажет «нет активных» или отменит;
//   - модель не указана → все healthy llama.cpp (у воркера есть отмена по
//     request_id/user_id, а пустой приказ отменяет активные генерации модели).
func (p *Proxy) cancelTargets(model string) []string {
	out := make([]string, 0, 4)
	seen := map[string]struct{}{}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	backends := p.GetAllBackends()
	for i := range backends {
		b := &backends[i]
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if b.Status != types.StatusHealthy && b.Status != "" {
			continue
		}
		if model == "" {
			add(b.ID)
			continue
		}
		if lm := p.llamaMetricsForPresence(b.ID); lm != nil {
			matched := false
			for _, m := range lm.LoadedModels {
				if modelNameMatches(m.Name, model) {
					matched = true
					break
				}
			}
			// R363 (аудит A4): загрузка модели — тоже адресат отмены. Раньше смотрели
			// только LoadedModels, поэтому Stop во время загрузки не доходил до узла,
			// который эту загрузку и выполняет (модели в loadedModels ещё нет).
			if !matched {
				for _, m := range lm.LoadingModels {
					if modelNameMatches(m.Name, model) {
						matched = true
						break
					}
				}
			}
			if matched {
				add(b.ID)
			}
		}
	}
	// R359 (2026-10-31, аудит abort A3): ЖИВОЙ учёт стримов важнее кэша поллера.
	//
	// Кэш поллера обновляется раз в 30 с, а статус health мо��ет быть не-healthy у
	// работающего llama.cpp-бэкенда (например ollama_unavailable без Ollama-порта).
	// Из-за этого приказ об отмене не уходил на бэкенд, где стрим РЕАЛЬНО идёт, и
	// клиент видел «нажал Stop, а оно продолжает». У балансера есть точный источник
	// правды — собственный учёт активных стримов (active_streams_r271.go).
	//
	// Добавляем такие бэкенды независимо от кэша и статуса: стрим существует →
	// бэкенд обслуживает эту модель прямо сейчас.
	for _, backendID := range p.backendsWithActiveStreamForR359(model) {
		add(backendID)
	}

	// Модель нигде не загружена и стримов не видно — шлём всем: приказ дешёвый, а
	// воркер, у которого генерация всё-таки идёт, отменит её.
	if len(out) == 0 && model != "" {
		for i := range backends {
			b := &backends[i]
			if normalizeBackendType(b.Type) == types.BackendTypeLlamaCpp &&
				(b.Status == types.StatusHealthy || b.Status == "") {
				add(b.ID)
			}
		}
	}
	return out
}

// cancelModelNameForBackend — имя модели для приказа об отмене этому бэкенду.
//
// R353 (2026-10-31, аудит abort §1.4 L1). См. вызов в sendCancelToBackend: балансер
// выбирает адресатов мягким сравнением, а воркер сравнивает имена ТОЧНО. Возвращаем
// каноническое имя загруженной у бэкенда модели, если она мягко совпала с клиентским
// именем; иначе — клиентское имя как есть (воркер мог не отчитаться о загрузке).
func (p *Proxy) cancelModelNameForBackend(backendID, model string) string {
	if p == nil || model == "" {
		return model
	}
	lm := p.llamaMetricsForPresence(backendID)
	if lm == nil {
		return model
	}
	for _, m := range lm.LoadedModels {
		if m.Name != "" && modelNameMatches(m.Name, model) {
			if m.Name != model && logger.Get() != nil {
				logger.Get().Debugw("cancel: клиентское имя модели заменено на загруженное",
					"backend", backendID, "client_model", model, "loaded_model", m.Name)
			}
			return m.Name
		}
	}
	return model
}

// sendCancelToBackend — один воркер: POST /api/cancel с тела клиента.
func (p *Proxy) sendCancelToBackend(backendID string, req clientCancelBody) (bool, map[string]interface{}) {
	addr := p.backendHTTPAddrByID(backendID)
	if addr == "" {
		return false, nil
	}
	payload := map[string]interface{}{}
	if req.CancelLoad != nil {
		// R363: явная клиентская отмена — по умолчанию загрузку тоже прерываем (значение
		// пришло из обработчика: nil уже развёрнут выше).
		payload["cancel_load"] = *req.CancelLoad
	}
	if req.Model != "" {
		// R353 (2026-10-31, аудит abort §1.4 L1): ВОРКЕРУ УХОДИТ ТО ИМЯ, ПОД КОТОРЫМ
		// МОДЕЛЬ ЗАГРУЖЕНА У НЕГО, а не клиентский псевдоним.
		//
		// Живой дефект. Балансер выбирает адресатов «мягко» (modelNameMatches:
		// регистр, путь, .gguf, тег Ollama, частичное совпадение), а воркер
		// сравнивает ИМЕНА ТОЧНО (CancelByModel: entry.model == model), где
		// entry.model — каноническое имя файла. Клиент жмёт Stop с "gemma4:latest"
		// или "Qwen3-Instruct-2507-q4km.gguf" → адресат выбран верно, приказ
		// доставлен, воркер отвечает cancelled:0, стрим продолжается. Снаружи это
		// «кнопка Stop не работает», и R343 честно печатал not_cancelled, но
		// причины не называл.
		//
		// Поэтому: если в кэше поллера у этого бэкенда есть загруженная модель,
		// которая «мягко» совпала с клиентским именем, отправляем ЕЁ имя. Клиентское
		// имя остаётся фолбэком: воркер мог не отчитаться о загрузке.
		payload["model"] = p.cancelModelNameForBackend(backendID, req.Model)
	}
	if req.RequestID != "" {
		payload["request_id"] = req.RequestID
	}
	if req.UserID != "" {
		payload["user_id"] = req.UserID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, nil
	}
	httpReq, err := http.NewRequest(http.MethodPost, addr+"/api/cancel", bytes.NewReader(body))
	if err != nil {
		return false, nil
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b := p.GetBackend(backendID); b != nil && b.CppWorkerApiToken != "" {
		httpReq.Header.Set("X-API-Token", b.CppWorkerApiToken)
	}
	client := &http.Client{Timeout: clientCancelTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		if logger.Get() != nil {
			logger.Get().Warnw(i18n.T("balancer.cancel_client.not_accepted_warn"),
				"backend", backendID, "model", req.Model, "error", err)
		}
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	detail := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &detail)
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300, detail
}

// writeClientCancelError — короткий JSON-отказ (клиенты ждут JSON, не текст).
func writeClientCancelError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, code, map[string]interface{}{"error": msg})
}

// trimSpaceOnly — убрать пробелы вокруг имени модели. РЕГИСТР НЕ МЕНЯЕМ:
// имена моделей бывают смешанными (Qwen3.8-27B), и приведение к нижнему регистру
// могло бы не совпасть на воркере.
func trimSpaceOnly(s string) string {
	return strings.TrimSpace(s)
}
