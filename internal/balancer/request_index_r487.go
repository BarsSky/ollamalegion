// request_index_r487.go — R487 (2026-11-11): ОТМЕНА ДОЛЖНА ЗНАТЬ, ЧТО ОТМЕНЯЕТ.
//
// ЖИВОЙ ДЕФЕКТ (архив второго стенда 2026-11-11, два прогона подряд).
// Клиент снял запрос, который СТОЯЛ В ОЧЕРЕДИ, и получил:
//   - запись в очереди осталась висеть (её никто не снял);
//   - генерация, которая в этот момент РАБОТАЛА на занятом бэкенде, была убита,
//     хотя её никто не отменял;
//   - в cppworker прилетел POST /api/cancel, которого там быть не должно было.
//
// ПОЧЕМУ. `POST /api/cancel` (R229) строит адресатов через `cancelTargets(model)`
// — это ВСЕ healthy llama.cpp, у которых модель загружена или грузится, — и
// рассылает приказ каждому. Ни `request_id`, ни сессия, ни очередь при выборе
// адресата не участвуют: идентификатор едет в теле, но адресат уже выбран по
// модели. Очередь же реагирует только на ctx.Done() СВОЕЙ http-сессии
// (admission_queue.go), а Stop приходит отдельным запросом — то есть заявку в
// очереди он не трогает вовсе.
//
// ЧТО ЗДЕСЬ. Индекс «request_id → где заявка»: в очереди (queued, со своим
// cancel) или на конкретном бэкенде (running). Заполняется там, где это
// достоверно известно (admission_queue.go и proxy_request.go), читается в
// handleClientCancel. Порядок резолва отмены:
//
//  1. `request_id` известен индексу:
//     queued  → снять с очереди (cancel), воркер НЕ трогать;
//     running → приказ ровно на ЕГО бэкенд, с ЕГО request_id;
//     `request_id` неизвестен → НЕ рассылать по бэкендам (это и убивало чужое).
//  2. есть явная личность клиента (X-Client-ID / X-User-Id / user_id тела) →
//     отменить только заявки этой личности (очередь снимается локально,
//     running-генерации — каждая на своём бэкенде).
//  3. ничего из перечисленного → прежняя широковещательная семантика по модели
//     (совместимость с OpenWebUI, который присылает только model), но в ответе
//     явно стоит scope:"model_broadcast", а в лог пишется предупреждение: такой
//     Stop не может быть адресным, потому что клиент не сказал, чей он.
package balancer

import (
	"context"
	"strings"
	"sync"
)

// trackedTaskR487 — одна заявка, которую можно снять или отменить.
type trackedTaskR487 struct {
	// Identity — явная личность клиента (X-Client-ID / X-User-Id / user_id тела).
	// Пусто = личность не заявлена; тогда сессия остаётся единственным признаком.
	Identity string
	ID       string
	Session  string
	Backend  string
	Queued   bool
}

// requestIndexR487 — индекс заявок.
//
// cancel-функции лежат ОТДЕЛЬНОЙ картой, а не полем trackedTaskR487: структура с
// указателем внутри держала бы GC-сканером весь свой объём (fieldalignment), а
// задача — это четыре строки и флаг, копировать её в ответы/тесты дешевле без
// функции. Внешнее поведение от этого не меняется.
type requestIndexR487 struct {
	byID    map[string]*trackedTaskR487
	cancels map[string]context.CancelFunc
	mu      sync.Mutex
}

func newRequestIndexR487() *requestIndexR487 {
	return &requestIndexR487{
		byID:    make(map[string]*trackedTaskR487),
		cancels: make(map[string]context.CancelFunc),
	}
}

// requestIndexR487 — ленивый доступ к индексу прокси.
//
// Ленивое создание, а не поле в конструкторе: Proxy в тестах собирается
// литералом (&Proxy{...}) в десятках мест, и обязательное поле сломало бы их все
// ради структуры, которая нужна только отмене.
func (p *Proxy) requestIndexR487() *requestIndexR487 {
	if p == nil {
		return nil
	}
	p.reqIdxMuR487.Lock()
	defer p.reqIdxMuR487.Unlock()
	if p.reqIdxR487 == nil {
		p.reqIdxR487 = newRequestIndexR487()
	}
	return p.reqIdxR487
}

// TrackQueued — заявка встала в admission-очередь и может быть снята локально.
func (x *requestIndexR487) TrackQueued(id, session, identity, backend string, cancel context.CancelFunc) {
	if x == nil || strings.TrimSpace(id) == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byID[id] = &trackedTaskR487{
		ID: id, Session: session, Identity: identity, Backend: backend,
		Queued: true,
	}
	if cancel != nil {
		x.cancels[id] = cancel
	}
}

// CancelQueued — снять заявку с очереди (вызвать её cancel). Возвращает false,
// если заявка уже не в очереди: тогда снимать нечего и врать «снято» нельзя.
func (x *requestIndexR487) CancelQueued(id string) bool {
	if x == nil {
		return false
	}
	x.mu.Lock()
	cancel, ok := x.cancels[id]
	x.mu.Unlock()
	if !ok || cancel == nil {
		return false
	}
	cancel()
	return true
}

// MarkRunning — заявка ушла на конкретный бэкенд. Снимает queued-состояние;
// session/identity сохраняются, если были известны (их ставит TrackQueued или
// MarkRunningIdentity).
func (x *requestIndexR487) MarkRunning(id, backend string) {
	if x == nil || strings.TrimSpace(id) == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	task, ok := x.byID[id]
	if !ok {
		task = &trackedTaskR487{ID: id}
		x.byID[id] = task
	}
	task.Backend = backend
	task.Queued = false
	delete(x.cancels, id)
}

// MarkIdentity — запомнить личность клиента для заявки, которая не проходила
// через очередь (быстрый путь без ожидания слота).
func (x *requestIndexR487) MarkIdentity(id, identity, session string) {
	if x == nil || strings.TrimSpace(id) == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	task, ok := x.byID[id]
	if !ok {
		task = &trackedTaskR487{ID: id}
		x.byID[id] = task
	}
	if identity != "" {
		task.Identity = identity
	}
	if session != "" {
		task.Session = session
	}
}

// Drop — заявка завершилась (успех, ошибка, обрыв) — из индекса убираем.
func (x *requestIndexR487) Drop(id string) {
	if x == nil || strings.TrimSpace(id) == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.byID, id)
	delete(x.cancels, id)
}

// Lookup — копия записи по request_id.
func (x *requestIndexR487) Lookup(id string) (trackedTaskR487, bool) {
	if x == nil {
		return trackedTaskR487{}, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	task, ok := x.byID[id]
	if !ok {
		return trackedTaskR487{}, false
	}
	return *task, true
}

// ByIdentity — все заявки с этой явной личностью клиента.
func (x *requestIndexR487) ByIdentity(identity string) []trackedTaskR487 {
	if x == nil || strings.TrimSpace(identity) == "" {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]trackedTaskR487, 0, len(x.byID))
	for _, task := range x.byID {
		if task.Identity == identity {
			out = append(out, *task)
		}
	}
	return out
}

// Len — сколько заявок в индексе (диагностика и тесты).
func (x *requestIndexR487) Len() int {
	if x == nil {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.byID)
}

// cancelByScopeR487 — адресная отмена. Возвращает (ответ, true), если отмена
// обработана по scope; (nil, false) — резолвить нечем, обработчик пойдёт
// прежним широковещательным путём.
func (p *Proxy) cancelByScopeR487(req clientCancelBody, identity string) (map[string]interface{}, bool) {
	idx := p.requestIndexR487()
	if idx == nil {
		return nil, false
	}

	if id := strings.TrimSpace(req.RequestID); id != "" {
		task, ok := idx.Lookup(id)
		if !ok {
			// Неизвестный id — это НЕ повод рассылать приказ по всем бэкендам с
			// моделью: именно так отмена чужой заявки убивала работающую генерацию.
			return map[string]interface{}{
				"status":     "not_cancelled",
				"scope":      "request_id",
				"request_id": id,
				"model":      req.Model,
				"backends":   []string{},
				"hint": "request_id неизвестен балансеру: отмена НЕ рассылается на бэкенды " +
					"(R487). Проверьте, что клиент присылает тот же request_id, который получил.",
			}, true
		}
		if task.Queued {
			idx.CancelQueued(id)
			idx.Drop(id)
			return map[string]interface{}{
				"status":       "cancelled_queued",
				"scope":        "request_id",
				"request_id":   id,
				"model":        req.Model,
				"backend":      task.Backend,
				"backends":     []string{},
				"session":      task.Session,
				"session_from": SessionIdentitySource(task.Session),
				"hint": "заявка снята с admission-очереди; генерация на бэкенде не начиналась, " +
					"поэтому воркер не вызывался",
			}, true
		}
		okSend, detail := p.sendCancelToBackend(task.Backend, clientCancelBody{
			RequestID: id, Model: req.Model, UserID: req.UserID, CancelLoad: req.CancelLoad,
		})
		status := "not_cancelled"
		if okSend {
			status = "cancelled"
		}
		row := map[string]interface{}{"backend": task.Backend, "ok": okSend}
		for k, v := range detail {
			row[k] = v
		}
		return map[string]interface{}{
			"status":     status,
			"scope":      "request_id",
			"request_id": id,
			"model":      req.Model,
			"backends":   backendsOfR487(okSend, task.Backend),
			"results":    []map[string]interface{}{row},
		}, true
	}

	if identity != "" {
		tasks := idx.ByIdentity(identity)
		if len(tasks) > 0 {
			return p.cancelTasksR487(tasks, req, "identity", identity), true
		}
	}
	return nil, false
}

// cancelTasksR487 — снять/отменить набор заявок одной личности.
//
// Очередь снимается ЛОКАЛЬНО (воркер не звоним), идущие генерации отменяются
// каждая на своём бэкенде и своим request_id — это и есть «не разрушать работу
// соседней сессии».
func (p *Proxy) cancelTasksR487(tasks []trackedTaskR487, req clientCancelBody, scope, scopeValue string) map[string]interface{} {
	idx := p.requestIndexR487()
	queued := 0
	backends := make([]string, 0, len(tasks))
	results := make([]map[string]interface{}, 0, len(tasks))
	for _, task := range tasks {
		if task.Queued {
			if idx != nil {
				idx.CancelQueued(task.ID)
				idx.Drop(task.ID)
			}
			queued++
			continue
		}
		okSend, detail := p.sendCancelToBackend(task.Backend, clientCancelBody{
			RequestID: task.ID, Model: req.Model, UserID: req.UserID, CancelLoad: req.CancelLoad,
		})
		row := map[string]interface{}{"backend": task.Backend, "request_id": task.ID, "ok": okSend}
		for k, v := range detail {
			row[k] = v
		}
		results = append(results, row)
		if okSend {
			backends = append(backends, task.Backend)
		}
	}
	status := "not_cancelled"
	switch {
	case len(results) == 0 && queued > 0:
		status = "cancelled_queued"
	case len(backends) > 0:
		status = "cancelled"
	}
	return map[string]interface{}{
		"status":         status,
		"scope":          scope,
		"scope_value":    scopeValue,
		"model":          req.Model,
		"backends":       backends,
		"queued_removed": queued,
		"results":        results,
	}
}

// backendsOfR487 — список из одного элемента или пустой (для JSON-ответа).
func backendsOfR487(ok bool, backend string) []string {
	if !ok || backend == "" {
		return []string{}
	}
	return []string{backend}
}
