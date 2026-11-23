// load_failure_events.go — R83 (2026-09-25): уведомление о провале загрузки.
//
// СВЯЗКА. cppworker отдаёт причину в /api/models (load_failure) → agent забирает
// её в своём обычном цикле опроса и пересылает в метриках → балансер публикует
// событие в EventBus → WebUI рисует его в bell-меню (notifications.js).
//
// РОЛЬ БАЛАНСЕРА ЗДЕСЬ МИНИМАЛЬНА, и это осознанно: он НЕ опрашивает cppworker
// ради этой фичи (это уже делает агент), НЕ агрегирует и НЕ хранит историю —
// только одно сравнение на уже полученный push и Publish, который по коду
// неблокирующий (см. publishTransportEOF). Оператор при этом получает причину
// («границы конфига», «нет памяти», «модель не найдена») с числами, а не сырой
// текст llama.cpp.
//
// ДЕДУПЛИКАЦИЯ ОБЯЗАТЕЛЬНА: agent шлёт метрики каждые ~10 с, запись о провале
// живёт 10 минут → без неё это ~60 одинаковых уведомлений, а SSE ring buffer
// вмещает 100 событий и полезное было бы вытеснено.
package balancer

import (
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// PublishLoadFailureTransition публикует уведомление о провале загрузки, но
// только когда состояние СМЕНИЛОСЬ:
//
//	нет → есть (или причина изменилась)  → уведомление с severity причины;
//	есть → нет                           → одно info «загрузка восстановлена».
//
// Повторные push'и с той же причиной молчат. Вызов дешёвый: мьютекс + сравнение
// строки, никаких таймеров и горутин.
//
// Экспортирован для api-слоя: agent присылает метрики в
// POST /api/v1/backends/{id}/agent/metrics (internal/api/handlers_agents.go).
func (p *Proxy) PublishLoadFailureTransition(backendID string, lf *types.LoadFailureInfo) {
	if p == nil {
		return
	}
	key := lf.Key()

	p.loadFailureMu.Lock()
	if p.loadFailureSeen == nil {
		p.loadFailureSeen = make(map[string]string)
	}
	prev, hadPrev := p.loadFailureSeen[backendID]
	if key == "" {
		// Провала больше нет.
		if !hadPrev {
			p.loadFailureMu.Unlock()
			return // и раньше не было — молчим (иначе «восстановление» на пустом месте)
		}
		delete(p.loadFailureSeen, backendID)
		p.loadFailureMu.Unlock()
		p.publishLoadFailureEvent(types.EventNotification, backendID, "", types.SeverityInfo,
			"Загрузка модели восстановлена", map[string]interface{}{
				"event_kind": "load_recovered",
			})
		return
	}
	if hadPrev && prev == key {
		p.loadFailureMu.Unlock()
		return // та же причина — уже сообщили
	}
	p.loadFailureSeen[backendID] = key
	p.loadFailureMu.Unlock()

	p.publishLoadFailureEvent(types.EventNotification, backendID, lf.Model,
		loadFailureEventSeverity(lf.Severity), loadFailureEventMessage(lf),
		map[string]interface{}{
			"event_kind":  "load_failed",
			"reason":      lf.Reason,
			"raw_error":   lf.Error,
			"occurred_at": lf.At,
			// Числа для разбора — то, ради чего фича и делается: оператор видит
			// запрошенный n_ctx и границы, а не «failed to load model».
			"diagnostics": lf.Diagnostics,
		})
}

// PublishLoadDegradedTransition — уведомление о том, что модель загружена в
// деградированном режиме (R83 §9.4 шаг 1б, вариант D, 2026-09-26).
//
// ОТЛИЧИЕ ОТ PublishLoadFailureTransition: там «не загрузилось» (severity по
// причине, позже info «восстановлено»), здесь «загрузилось, но без GPU».
// Смешивать нельзя: для работающей модели событие «не хватило памяти» — ложь.
//
// Дедупликация — по DegradedLoadInfo.Key() (модель + стадия) в ТОЙ ЖЕ карте
// loadFailureSeen, но с префиксом "degraded:", чтобы ключи двух типов не
// сталкивались (иначе «cpu_only» модели M и «провал» модели M с тем же ключом
// глушили бы друг друга). Пустой key сбрасывает запись и молчит: снятие
// деградации — не событие (модель просто загрузили нормально).
func (p *Proxy) PublishLoadDegradedTransition(backendID string, d *types.DegradedLoadInfo) {
	if p == nil {
		return
	}
	key := d.Key()

	p.loadFailureMu.Lock()
	if p.loadFailureSeen == nil {
		p.loadFailureSeen = make(map[string]string)
	}
	dedupKey := "degraded:" + backendID
	prev, hadPrev := p.loadFailureSeen[dedupKey]
	if key == "" {
		if hadPrev {
			delete(p.loadFailureSeen, dedupKey)
		}
		p.loadFailureMu.Unlock()
		return // снятие деградации молчит: «загрузилось нормально» — не инцидент
	}
	if hadPrev && prev == key {
		p.loadFailureMu.Unlock()
		return // та же деградация — уже сообщили (agent шлёт метрики каждые ~10 с)
	}
	p.loadFailureSeen[dedupKey] = key
	p.loadFailureMu.Unlock()

	p.publishLoadFailureEvent(types.EventNotification, backendID, d.Model,
		types.SeverityWarning, loadDegradedEventMessage(d),
		map[string]interface{}{
			"event_kind":  "load_degraded",
			"stage":       d.Stage,
			"reason":      d.Reason,
			"detail":      d.Detail,
			"occurred_at": d.At,
			// Те же числа, что в логе раскладки и в ответе cppworker, — оператор
			// видит, ПОЧЕМУ раскладка такая, а не только «cpu_only».
			"diagnostics": d.Diagnostics,
		})
}

// loadDegradedEventMessage — человеческая фраза про деградацию.
//
// Формулировка обязана читаться как «работает, но медленно»: оператор должен
// понимать, что модель доступна, а не искать, почему она не загрузилась.
func loadDegradedEventMessage(d *types.DegradedLoadInfo) string {
	if d.IsEmpty() {
		return "Модель загружена в деградированном режиме"
	}
	model := d.Model
	if model == "" {
		model = "модель"
	}
	switch d.Stage {
	case "cpu_only":
		msg := model + " загружена без GPU (cpu_only): веса не влезли в VRAM, " +
			"инференс будет медленным. Уменьшите n_ctx или gpuLayers, чтобы вернуть слои на GPU."
		if d.Detail != "" {
			msg += " Причина: " + d.Detail
		}
		return msg
	}
	msg := model + " загружена в деградированном режиме (" + d.Stage + ")"
	if d.Detail != "" {
		msg += ": " + d.Detail
	}
	return msg
}

// publishLoadFailureEvent — низкоуровневая публикация (nil-safe по eventBus).
func (p *Proxy) publishLoadFailureEvent(evType types.EventType, backendID, model string,
	severity types.EventSeverity, message string, data map[string]interface{}) {
	if p.eventBus == nil {
		return
	}
	p.eventBus.Publish(types.Event{
		Type:      evType,
		Timestamp: time.Now(),
		BackendID: backendID,
		Model:     model,
		Severity:  severity,
		Source:    "load",
		Message:   message,
		Data:      data,
	})
	logger.Get().Infow("load failure event published",
		"backend", backendID, "model", model, "severity", severity, "message", message)
}

// publishLoadFailureEventDeduped — публикация с дедупликацией по (backend, key).
//
// R83 §9.3 (2026-09-26): нужно для путей, которые повторяются при каждом запросе
// клиента (например, провал async auto-load): без этого одно и то же событие
// уходило бы в EventBus на каждую попытку и вытесняло полезную историю из
// ring buffer (100 событий), а оператор получал бы лавину уведомлений.
//
// Семантика: сообщаем, только когда ключ СМЕНИЛСЯ. Пустой key сбрасывает запись.
func (p *Proxy) publishLoadFailureEventDeduped(backendID, key string,
	severity types.EventSeverity, message string, data map[string]interface{}) {
	if p == nil || key == "" {
		return
	}
	p.loadFailureMu.Lock()
	if p.loadFailureSeen == nil {
		p.loadFailureSeen = make(map[string]string)
	}
	dedupKey := "pump:" + backendID
	if prev, ok := p.loadFailureSeen[dedupKey]; ok && prev == key {
		p.loadFailureMu.Unlock()
		return
	}
	p.loadFailureSeen[dedupKey] = key
	p.loadFailureMu.Unlock()

	p.publishLoadFailureEvent(types.EventNotification, backendID, "", severity, message, data)
}

// loadFailureEventSeverity — severity события из severity причины. Неизвестное
// значение трактуем как warning: пугать оператора error'ом без причины нельзя.
func loadFailureEventSeverity(reasonSeverity string) types.EventSeverity {
	switch reasonSeverity {
	case string(types.SeverityInfo):
		return types.SeverityInfo
	case string(types.SeverityWarning):
		return types.SeverityWarning
	case string(types.SeverityError):
		return types.SeverityError
	case string(types.SeverityCritical):
		return types.SeverityCritical
	}
	return types.SeverityWarning
}

// loadFailureEventMessage — человеческая фраза для уведомления.
//
// Текст строится здесь, а не в WebUI, чтобы одинаково выглядеть во всех
// потребителях (bell, логи). Сырой текст остаётся в Data["raw_error"].
func loadFailureEventMessage(lf *types.LoadFailureInfo) string {
	if lf == nil {
		return "Модель не загрузилась"
	}
	model := lf.Model
	if model == "" {
		model = "модель"
	}
	diag := lf.Diagnostics
	switch lf.Reason {
	case "model_not_found":
		return "Модель не найдена: " + model +
			". Проверьте имя — доступные модели перечислены в подробностях."
	case "config_out_of_bounds":
		return "Конфигурация вне границ: " + model +
			" — запрошенный n_ctx больше допустимого на этом железе."
	case "insufficient_resources":
		return "Не хватило памяти для " + model +
			": уменьшите n_ctx или используйте меньшую модель."
	case "gguf_incompatible":
		return "Файл модели " + model + " не принят llama.cpp (формат/архитектура)."
	// R697 (2026-11-24): «сборка не знает архитектуру» — отдельная формулировка с
	// именем архитектуры и сборкой, а не «файл не принят».
	//
	// ЖИВОЙ СЛУЧАЙ: модель Inkling-2-Mini (general.architecture="inkling2") на
	// воркере gpu-r660; llama.cpp отвечал unknown model architecture, но оператор
	// видел только «файл модели не принят (формат/архитектура)» — то есть шёл
	// проверять файл, хотя лечится обновлением сабмодуля и пересборкой образа.
	// Текст строится здесь (а не в WebUI), поэтому bell-меню показывает его как есть.
	case "model_unsupported":
		msg := "Архитектура модели " + model + " не поддерживается сборкой llama.cpp"
		if arch, ok := lf.Diagnostics["arch"].(string); ok && arch != "" {
			msg += " (" + arch + ")"
		}
		if build, ok := lf.Diagnostics["llama_build"].(string); ok && build != "" {
			msg += " " + build
		}
		msg += ": файл новее движка, повторы не помогут."
		if hint, ok := lf.Diagnostics["hint"].(string); ok && hint != "" {
			msg += " Что делать: " + hint
		}
		return msg
	// R704 (2026-11-24): архитектуру сборка знает, а схему файла — нет
	// (loop-развёрнутый GGUF: 24 физических блока при block_count=48). Прежний
	// текст был бы «файл модели не принят (формат/архитектура)» — то есть снова
	// отправлял бы оператора проверять файл, хотя файл на месте и исправен.
	// Числа (block_count, физические блоки) и рецепт (ре-экспорт в <arch>_loop)
	// приходят в Diagnostics от cppworker (model_schema_unsupported.go).
	case "model_schema_unsupported":
		msg := "Схема файла модели " + model + " не поддерживается загрузчиком llama.cpp"
		facts := make([]string, 0, 3)
		if arch, ok := lf.Diagnostics["arch"].(string); ok && arch != "" {
			facts = append(facts, arch)
		}
		if bc := diagnosticsCount(diag, "block_count"); bc != "" {
			facts = append(facts, "block_count="+bc)
		}
		if pb := diagnosticsCount(diag, "physical_blocks"); pb != "" {
			facts = append(facts, "физических блоков="+pb)
		}
		if len(facts) > 0 {
			msg += " (" + strings.Join(facts, ", ") + ")"
		}
		msg += ": файл на месте, дело в схеме тензоров, повторы не помогут."
		if hint, ok := diag["hint"].(string); ok && hint != "" {
			msg += " Что делать: " + hint
		}
		return msg
	case "backend_not_ready":
		return "Бэкенд не готов к загрузке " + model + " (менеджер моделей не инициализирован)."
	case "load_timeout":
		return "Загрузка " + model + " не уложилась в отведённое время."
	case "alias_broken":
		return "Алиас " + model + " указывает на отсутствующий файл."
	}
	return "Модель " + model + " не загрузилась: " + lf.Error
}

// diagnosticsCount — положительное целое из Diagnostics в виде строки.
//
// ЗАЧЕМ ОТДЕЛЬНО. Diagnostics идут cppworker → agent → балансер как JSON, поэтому
// в map лежит float64, а не int: приведение `.(int)` молча дало бы ноль, и
// уведомление потеряло бы числа (block_count=48, физических блоков=24) — ровно то,
// из-за чего R704 и появился. Пустая строка = числа нет (не выдумываем ноль).
func diagnosticsCount(diag map[string]interface{}, key string) string {
	if diag == nil {
		return ""
	}
	switch v := diag[key].(type) {
	case float64:
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(int(v))
	case int:
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(v)
	case int64:
		if v <= 0 {
			return ""
		}
		return strconv.FormatInt(v, 10)
	}
	return ""
}
