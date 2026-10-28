package api

// handlers_backend_disabled_r250.go — R250 (2026-10-28): переключатель
// «не использовать бэкенд» (POST/GET /api/v1/backends/{id}/enabled).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА. Нужно исключить узел из выбора балансером, не
// останавливая контейнер: остановленный узел исчезает из панели (а его надо
// настраивать), снятая роль ломает раскладку. Здесь — третий способ: флаг,
// который читает только СЕЛЕКТОР.
//
// ЧТО ВАЖНО ДЛЯ ОПЕРАТОРА (и поэтому отдаётся в ответе):
//   - `enabled` — то, что реально записано (а не то, что прислали);
//   - `loadedModels` — модели, которые СЕЙЧАС работают на узле. Выключение их не
//     выгружает: запрос уже играет, и обрывать его из-за галочки нельзя. Панель
//     обязана это сказать, иначе «выключил, а GPU всё ещё занята» выглядит
//     поломкой;
//   - `persisted`/`warning` — переживёт ли решение рестарт. Sidecar
//     (runtime-overrides/backend-disabled.json) есть не всегда (dev-режим), и
//     молчать об этом нельзя: оператор настраивает узел «на неделю».
//
// ПОЧЕМУ ОТВЕТ СРАЗУ. Как в R249: здесь нет ни одного сетевого вызова к узлу.
// Флаг живёт в памяти балансера (+ маленький файл override), поэтому ответ
// приходит за единицы миллисекунд даже когда сам узел недоступен — а именно
// недоступный узел оператор чаще всего и выключает.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/runtimeoverrides"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// BackendDisabledStore — R250: НЕОБЯЗАТЕЛЬНАЯ часть хранилища override —
// персист решения «не использовать бэкенд».
//
// Отдельный интерфейс по той же причине, что BackendRolesStore: существующие
// реализации и тестовые дубли не обязаны про этот флаг знать, а проверяем мы
// type-assert'ом. Нужен потому, что config.json в bundled-развёртывании
// read-only, а запись бэкенда пересобирается саморегистрацией агента.
type BackendDisabledStore interface {
	LoadBackendDisabled() (map[string]runtimeoverrides.BackendDisabledRecord, bool, error)
	SaveBackendDisabled(map[string]runtimeoverrides.BackendDisabledRecord) error
}

// readyToReport — бэкенд найден? Общий пролог GET/POST (404 вместо паники).
func (s *Server) backendEnabledTarget(w http.ResponseWriter, backendID string) bool {
	if s.proxy == nil {
		http.Error(w, `{"error":"proxy not available"}`, http.StatusInternalServerError)
		return false
	}
	if s.proxy.GetBackend(backendID) == nil {
		http.Error(w, `{"error":"backend not found"}`, http.StatusNotFound)
		return false
	}
	return true
}

// backendEnabled — GET/POST /api/v1/backends/{id}/enabled.
//
// Тело POST: {"enabled": false, "reason": "плановое обслуживание"}.
// `enabled` обязателен (указатель): «ключа нет» и «enabled=false» — разные
// запросы, и молча трактовать отсутствие ключа как «выключить» нельзя.
func (s *Server) backendEnabled(w http.ResponseWriter, r *http.Request, backendID string) {
	if !s.backendEnabledTarget(w, backendID) {
		return
	}

	disabled := false
	reason := ""
	var persistWarning string
	status := "ok"

	switch r.Method {
	case http.MethodGet:
		d, why, _, ok := s.proxy.BackendDisabledInfo(backendID)
		if !ok {
			http.Error(w, `{"error":"backend not found"}`, http.StatusNotFound)
			return
		}
		disabled, reason = d, why

	case http.MethodPost, http.MethodPut:
		var req struct {
			Enabled *bool  `json:"enabled"`
			Reason  string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if req.Enabled == nil {
			http.Error(w, `{"error":"enabled is required (true|false)"}`, http.StatusBadRequest)
			return
		}
		wantDisabled := !*req.Enabled
		reason = strings.TrimSpace(req.Reason)
		if !wantDisabled {
			// Причина и момент выключения при включении не имеют смысла:
			// оставлять «причина: плановое обслуживание» у работающего узла —
			// прямая ложь в панели.
			reason = ""
		}

		if !s.proxy.SetBackendDisabled(backendID, wantDisabled, reason) {
			http.Error(w, `{"error":"failed to store backend flag"}`, http.StatusInternalServerError)
			return
		}
		disabled = wantDisabled
		status = "updated"

		// Решение оператора — в persistent-override. Ошибка записи НЕ отменяет
		// флаг в памяти (он уже действует): оператор должен видеть, что выбор
		// применился сейчас, но может не пережить рестарт.
		if err := s.persistBackendDisabled(backendID, wantDisabled, reason); err != nil {
			persistWarning = err.Error()
			logger.Get().Warnw(i18n.T("api.handlers_backends.persist_disabled_failed"),
				"backend", backendID, "error", err)
		}
		logger.Get().Infow(i18n.T("api.handlers_backends.enabled_changed"),
			"backend", backendID, "enabled", !wantDisabled, "reason", reason)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, why, since, _ := s.proxy.BackendDisabledInfo(backendID)
	loaded := s.proxy.LoadedModelNames(backendID)

	resp := map[string]interface{}{
		"status":  status,
		"backend": backendID,
		// enabled — инверсия флага: оператор думает «включён/выключен», а поле в
		// записи называется Disabled. Отдаём оба, чтобы ответ читался без
		// выворачивания логики (и UI, и curl).
		"enabled":        !disabled,
		"disabled":       disabled,
		"reason":         why,
		"loadedModels":   loaded,
		"loadedModelCnt": len(loaded),
	}
	if since != nil {
		resp["since"] = since.UTC().Format(time.RFC3339)
	}
	if persistWarning != "" {
		resp["persisted"] = false
		resp["warning"] = persistWarning
	} else {
		resp["persisted"] = true
	}
	if disabled && len(loaded) > 0 {
		// Самое частое недопонимание: «выключил, а GPU занята». Модель остаётся
		// загруженной до конца текущих запросов — выгрузка это отдельное действие
		// (страница моделей), и делать её молча из тумблера нельзя.
		resp["modelsStillLoaded"] = loaded
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// persistBackendDisabled — сохранить решение в sidecar (best-effort).
func (s *Server) persistBackendDisabled(backendID string, disabled bool, reason string) error {
	store, ok := s.overridesStore.(BackendDisabledStore)
	if !ok || store == nil {
		// Dev-режим / хранилище не сконфигурировано: персиста нет, но флаг
		// действует. Возвращаем nil: шумная ошибка тут врала бы про «не применилось».
		return nil
	}

	s.backendDisabledMu.Lock()
	if s.backendDisabled == nil {
		s.backendDisabled = map[string]runtimeoverrides.BackendDisabledRecord{}
	}
	if disabled {
		s.backendDisabled[backendID] = runtimeoverrides.NewBackendDisabledRecord(reason)
	} else {
		// Явное «включить» тоже решение оператора: оно обязано перебить флаг,
		// который мог остаться в state.json от прошлой сессии.
		s.backendDisabled[backendID] = runtimeoverrides.BackendDisabledRecord{Disabled: false}
	}
	snapshot := make(map[string]runtimeoverrides.BackendDisabledRecord, len(s.backendDisabled))
	for id, rec := range s.backendDisabled {
		snapshot[id] = rec
	}
	s.backendDisabledMu.Unlock()

	return store.SaveBackendDisabled(snapshot)
}

// restoreBackendDisabled — поднять сохранённые флаги с диска при старте.
//
// Зеркало restoreBackendRoles: записи бэкендов к этому моменту могут быть ещё не
// зарегистрированы (агенты приходят позже), поэтому дополнительно запускаем
// периодический возврат флага в записи — см. reapplyBackendDisabledLoop.
func (s *Server) restoreBackendDisabled() {
	store, ok := s.overridesStore.(BackendDisabledStore)
	if !ok || store == nil {
		return
	}
	records, exists, err := store.LoadBackendDisabled()
	if err != nil {
		logger.Get().Warnw(i18n.T("api.handlers_backends.restore_disabled_failed"), "error", err)
		return
	}
	if !exists || len(records) == 0 {
		return
	}

	s.backendDisabledMu.Lock()
	s.backendDisabled = records
	s.backendDisabledMu.Unlock()

	logger.Get().Infow(i18n.T("api.handlers_backends.restore_disabled_done"), "records", len(records))

	// Применяем к уже существующим записям и держим флаг в них дальше.
	s.reapplyBackendDisabledOnce()
	if s.stopCh != nil {
		go s.reapplyBackendDisabledLoop()
	}
}

// reapplyBackendDisabledOnce — один проход: вернуть флаг в записи бэкендов.
//
// Зачем цикл, а не разовая подстановка: запись пересобирается НЕСКОЛЬКИМИ путями
// (создание, правка из WebUI, регистрация и heartbeat агента), и агент о решении
// оператора не знает. Ровно эта беда уже была с ролями (R118): роль читалась с
// диска, но в записях оставалась пустой. Здесь тот же приём — балансер сам
// приводит записи к сохранённому решению. Операция идемпотентна.
//
// Возвращает число записей, которым флаг вернули (для тестов и логов).
func (s *Server) reapplyBackendDisabledOnce() int {
	if s.proxy == nil {
		return 0
	}
	s.backendDisabledMu.Lock()
	saved := make(map[string]runtimeoverrides.BackendDisabledRecord, len(s.backendDisabled))
	for id, rec := range s.backendDisabled {
		saved[id] = rec
	}
	s.backendDisabledMu.Unlock()
	if len(saved) == 0 {
		return 0
	}

	restored := 0
	for id, rec := range saved {
		disabled, reason, _, ok := s.proxy.BackendDisabledInfo(id)
		if !ok {
			continue
		}
		if disabled == rec.Disabled && (!rec.Disabled || reason == rec.Reason) {
			continue
		}
		if s.proxy.SetBackendDisabled(id, rec.Disabled, rec.Reason) {
			restored++
			logger.Get().Infow(i18n.T("api.handlers_backends.reapply_disabled_returned"),
				"backend", id, "enabled", !rec.Disabled)
		}
	}
	return restored
}

// reapplyBackendDisabledLoop — периодический возврат флага (тик 15 с, как у ролей).
func (s *Server) reapplyBackendDisabledLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.reapplyBackendDisabledOnce()
		}
	}
}

// backendDisabledView — то, что отдаётся в списке бэкендов и в карточке.
//
// Отдельная функция, а не пять строк на месте: поля обязаны совпадать во всех
// ответах, иначе UI читает флаг из списка, а карточка его не знает (эта ошибка
// уже была с rpcEnabledOverride).
func (s *Server) backendDisabledView(backend types.Backend) map[string]interface{} {
	view := map[string]interface{}{
		"disabled": backend.Disabled,
	}
	if backend.Disabled {
		view["disabledReason"] = backend.DisabledReason
		if backend.DisabledAt != nil {
			view["disabledSince"] = backend.DisabledAt.UTC().Format(time.RFC3339)
		}
	}
	return view
}

// disabledSinceRFC3339 — момент выключения строкой для JSON (пусто = не выключен).
//
// Зачем строка, а не time.Time: у включённого бэкенда поля быть не должно вовсе,
// иначе панель покажет «01.01.1» (нулевое время Go приходит как
// "0001-01-01T00:00:00Z" — ровно так уже ломался lastAgentContact).
func disabledSinceRFC3339(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// cloneDisabledSince — копия указателя момента выключения.
//
// Пути, которые пересобирают запись бэкенда (PUT, heartbeat агента), обязаны
// ПЕРЕНОСИТЬ поле. Копируем значение, а не указатель: запись состояния не должна
// делить его со снимком, который уже отдан наружу.
func cloneDisabledSince(src *time.Time) *time.Time {
	if src == nil {
		return nil
	}
	v := *src
	return &v
}
